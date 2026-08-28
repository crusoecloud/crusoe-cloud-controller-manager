package routes

import (
	"context"
	"sync"

	"github.com/crusoecloud/crusoe-cloud-controller-manager/internal/routes/sdn"
	"k8s.io/klog/v2"
)

// maxOpMisses is the number of consecutive ListPodCIDRAllocationOperations
// passes an op may be absent from before the tracker drops it (retention gap /
// SDN restart). After the drop, reconcile's List-based recovery takes over.
const maxOpMisses = 3

// trackedOp is an in-flight operation and the node it belongs to.
type trackedOp struct {
	nodeKey string
	misses  int
}

// opTracker batches async-operation polling: a single poll loop issues ONE
// ListPodCIDRAllocationOperations call per tick covering every in-flight create,
// regardless of node count. There are no per-node goroutines.
type opTracker struct {
	mu      sync.Mutex
	pending map[string]trackedOp     // opID -> {nodeKey, misses}
	results map[string]sdn.Operation // terminal ops awaiting consumption by reconcile
}

func newOpTracker() *opTracker {
	return &opTracker{
		pending: make(map[string]trackedOp),
		results: make(map[string]sdn.Operation),
	}
}

// Track registers an in-flight op for a node. Idempotent: re-tracking a known op
// preserves its miss counter.
func (t *opTracker) Track(opID, nodeKey string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if _, ok := t.pending[opID]; ok {
		return
	}
	t.pending[opID] = trackedOp{nodeKey: nodeKey}
}

// TakeResult returns and removes a terminal result if the tracker has seen one.
func (t *opTracker) TakeResult(opID string) (sdn.Operation, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	op, ok := t.results[opID]
	if ok {
		delete(t.results, opID)
	}

	return op, ok
}

// Forget drops any registration/result for the op (node deleted, op consumed).
func (t *opTracker) Forget(opID string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	delete(t.pending, opID)
	delete(t.results, opID)
}

// isTracking reports whether the op is currently registered as pending.
func (t *opTracker) isTracking(opID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	_, ok := t.pending[opID]

	return ok
}

// pendingIDs snapshots the set of pending op ids.
func (t *opTracker) pendingIDs() []string {
	t.mu.Lock()
	defer t.mu.Unlock()

	ids := make([]string, 0, len(t.pending))
	for id := range t.pending {
		ids = append(ids, id)
	}

	return ids
}

// dispatch is a callback the poll loop invokes to enqueue a node key when an op
// becomes terminal or is dropped.
type dispatch func(nodeKey string)

// recordResults processes a batch of ops returned by a poll: terminal ops move
// pending -> results and their node is dispatched; ops absent from the returned
// set have their miss counter incremented and are dropped (and dispatched) after
// maxOpMisses. Returns nothing; it mutates tracker state and calls dispatch.
func (t *opTracker) recordResults(ops []sdn.Operation, enqueue dispatch) {
	t.mu.Lock()

	seen := make(map[string]bool, len(ops))
	toDispatch := make([]string, 0, len(ops))

	for i := range ops {
		op := ops[i]
		tracked, ok := t.pending[op.OperationID]
		if !ok {
			continue
		}
		seen[op.OperationID] = true
		if op.State == sdn.OperationStateInProgress {
			continue
		}
		// Terminal: move to results, dispatch the node.
		t.results[op.OperationID] = op
		delete(t.pending, op.OperationID)
		toDispatch = append(toDispatch, tracked.nodeKey)
	}

	// Ops we did not see this pass: count a miss, drop after the limit.
	for id, tracked := range t.pending {
		if seen[id] {
			continue
		}
		tracked.misses++
		if tracked.misses >= maxOpMisses {
			delete(t.pending, id)
			toDispatch = append(toDispatch, tracked.nodeKey)
			klog.InfoS("opTracker dropping op after repeated misses",
				"operationID", id, "node", tracked.nodeKey, "misses", tracked.misses)

			continue
		}
		t.pending[id] = tracked
	}

	t.mu.Unlock()

	for _, key := range toDispatch {
		enqueue(key)
	}
}

// pollOpsOnce is the single poll-loop tick (run via wait.UntilWithContext). It
// snapshots the pending op ids, issues one batched
// ListPodCIDRAllocationOperations, and records the results. It makes no RPC when
// nothing is pending.
func (c *RouteController) pollOpsOnce(ctx context.Context) {
	ids := c.tracker.pendingIDs()
	if len(ids) == 0 {
		return
	}

	ops, err := c.sdn.ListPodCIDRAllocationOperations(ctx,
		sdn.ListPodCIDRAllocationOperationsQuery{OperationIDs: ids})
	if err != nil {
		klog.ErrorS(err, "opTracker poll failed; leaving pending intact", "pending", len(ids))

		return
	}

	c.tracker.recordResults(ops, c.enqueue)
}
