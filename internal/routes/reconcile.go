package routes

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crusoecloud/crusoe-cloud-controller-manager/internal/routes/sdn"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/klog/v2"
)

// Event reasons emitted on the Node object.
const (
	reasonAllocationCreated      = "AllocationCreated"
	reasonAllocationCreateFailed = "AllocationCreateFailed"
	reasonAllocationConflict     = "AllocationConflict"
)

// reconcile is the per-node create state machine (§10). Every step is idempotent
// and safe to re-enter. Invariant: op-id label present <=> a create is (believed)
// in flight; the final patch clears it.
//
// It returns (requeueAfter, err) per the worker-loop contract.
func (c *RouteController) reconcile(ctx context.Context, nodeName string) (time.Duration, error) {
	cn, gone, err := c.fetchCiliumNode(nodeName)
	if err != nil {
		return 0, err
	}
	if gone {
		c.forgetNode(nodeName)

		return 0, nil
	}

	// C1: no podCIDRs yet — purely event-driven; wait for the CiliumNode update.
	if len(cn.PodCIDRs) == 0 {
		klog.V(4).InfoS("CiliumNode has no podCIDRs yet", "node", nodeName)

		return 0, nil
	}

	// C2: route only podCIDRs[0]; warn once on extras.
	cidr := cn.PodCIDRs[0]
	if len(cn.PodCIDRs) > 1 {
		c.warnMultiCIDR(nodeName, cn.PodCIDRs)
	}

	node := c.getNode(nodeName)

	// C3 fast path: already finalized.
	if node != nil && c.isSteadyStateReady(node) {
		c.markReady(nodeName)

		return 0, nil
	}

	// C4: resolve NIC.
	nicID, err := c.resolveNIC(ctx, nodeName, node)
	if err != nil {
		return 0, err
	}

	return c.driveCreate(ctx, nodeName, node, nicID, cidr)
}

// driveCreate runs C5-C11: resume an in-flight op, adopt an existing allocation,
// or create a new one, then finalize.
func (c *RouteController) driveCreate(
	ctx context.Context, nodeName string, node *v1.Node, nicID, cidr string,
) (time.Duration, error) {
	// C5: read op-id label off the Node (node==nil => absent).
	opID := ""
	if node != nil {
		opID = node.Labels[OpIDLabel]
	}

	if opID != "" {
		// C6: resume the in-flight op.
		return c.resumeOp(ctx, nodeName, node, opID, cidr)
	}

	// C7: no label — List before create (create may have succeeded pre-label).
	alloc, found, err := c.lookupAllocation(ctx, cidr)
	if err != nil {
		return 0, err
	}
	if found {
		if alloc.NetworkInterfaceID == nicID {
			c.setAllocationID(nodeName, alloc.ID)

			return c.finalize(ctx, nodeName, node)
		}

		return c.handleConflict(nodeName, node) // C10
	}

	// C8: create.
	return c.createAllocation(ctx, nodeName, node, nicID, cidr)
}

// resumeOp implements C6: consume the tracker result, or fall back to List when
// the op is unknown SDN-side.
func (c *RouteController) resumeOp(
	ctx context.Context, nodeName string, node *v1.Node, opID, cidr string,
) (time.Duration, error) {
	op, ok := c.tracker.TakeResult(opID)
	if !ok {
		if c.tracker.isTracking(opID) {
			return c.cfg.PollInterval, nil // still IN_PROGRESS
		}
		// Tracker dropped it (misses) or never knew it (failover): re-register and
		// fall back to List-based recovery.
		c.tracker.Track(opID, nodeName)

		return c.recoverFromList(ctx, nodeName, node, opID, cidr)
	}

	switch op.State {
	case sdn.OperationStateSucceeded:
		return c.onOpSucceeded(ctx, nodeName, node, op)
	case sdn.OperationStateFailed:
		return c.onOpFailed(ctx, nodeName, node, op)
	case sdn.OperationStateInProgress:
		return c.cfg.PollInterval, nil
	default:
		return c.cfg.PollInterval, nil
	}
}

// recoverFromList handles a resumed op whose history is lost: List by
// destination_cidr to decide adopt / conflict / recreate.
func (c *RouteController) recoverFromList(
	ctx context.Context, nodeName string, node *v1.Node, opID, cidr string,
) (time.Duration, error) {
	c.tracker.Forget(opID)

	alloc, found, err := c.lookupAllocation(ctx, cidr)
	if err != nil {
		return 0, err
	}
	if !found {
		if clearErr := c.clearOpIDIfNode(ctx, node); clearErr != nil {
			return 0, clearErr
		}

		nicID, resolveErr := c.resolveNIC(ctx, nodeName, node)
		if resolveErr != nil {
			return 0, resolveErr
		}

		return c.createAllocation(ctx, nodeName, node, nicID, cidr)
	}

	nicID, resolveErr := c.resolveNIC(ctx, nodeName, node)
	if resolveErr != nil {
		return 0, resolveErr
	}
	if alloc.NetworkInterfaceID == nicID {
		c.setAllocationID(nodeName, alloc.ID)

		return c.finalize(ctx, nodeName, node)
	}

	return c.handleConflict(nodeName, node)
}

// onOpSucceeded records the allocation id, observes provision latency and
// finalizes (C6 SUCCEEDED -> C9).
func (c *RouteController) onOpSucceeded(
	ctx context.Context, nodeName string, node *v1.Node, op sdn.Operation,
) (time.Duration, error) {
	if len(op.AllocationIDs) > 0 {
		c.setAllocationID(nodeName, op.AllocationIDs[0])
	}
	c.observeProvisionLatency(nodeName)

	return c.finalize(ctx, nodeName, node)
}

// onOpFailed clears the op-id label, emits a warning, and returns an error so
// the rate-limited retry re-creates (create is idempotent).
func (c *RouteController) onOpFailed(
	ctx context.Context, nodeName string, node *v1.Node, op sdn.Operation,
) (time.Duration, error) {
	if err := c.clearOpIDIfNode(ctx, node); err != nil {
		return 0, err
	}
	c.eventf(node, v1.EventTypeWarning, reasonAllocationCreateFailed,
		"pod CIDR allocation operation failed: %s", op.Error)

	return 0, fmt.Errorf("pod CIDR allocation operation %s failed for node %s: %s",
		op.OperationID, nodeName, op.Error)
}

// createAllocation implements C8: create exactly one allocation, then patch the
// op-id label immediately.
func (c *RouteController) createAllocation(
	ctx context.Context, nodeName string, node *v1.Node, nicID, cidr string,
) (time.Duration, error) {
	req := sdn.CreatePodCIDRAllocationsRequest{
		Allocations: []sdn.PodCIDRAllocationSpec{
			{VPCPrefixReservationID: c.cfg.VPCPrefixReservationID, NetworkInterfaceID: nicID, DestinationCIDR: cidr},
		},
		Context: sdn.PodCIDRAllocationContext{
			ProjectID: c.cfg.ProjectID, VPCNetworkID: c.cfg.VPCID, Location: c.cfg.Location,
		},
	}

	op, err := c.sdn.CreatePodCIDRAllocations(ctx, req)
	switch {
	case errors.Is(err, sdn.ErrDestinationConflict):
		return c.handleConflict(nodeName, node) // C10
	case errors.Is(err, sdn.ErrInvalidArgument):
		c.eventf(node, v1.EventTypeWarning, reasonAllocationCreateFailed,
			"pod CIDR allocation request rejected as invalid (needs operator/config fix): %v", err)
		klog.ErrorS(err, "permanent create failure", "node", nodeName, "cidr", cidr)

		return 0, fmt.Errorf("create rejected for node %s: %w", nodeName, err)
	case err != nil:
		return 0, fmt.Errorf("create failed for node %s: %w", nodeName, err)
	}

	c.startCreateTimer(nodeName)

	// Patch the op-id label immediately (durable before anything depends on it).
	if node != nil {
		if patchErr := c.patchOpIDLabel(ctx, node, op.OperationID); patchErr != nil {
			return 0, patchErr
		}
	}
	c.tracker.Track(op.OperationID, nodeName)

	return c.cfg.PollInterval, nil
}

// finalize implements C9 + C11: set NetworkUnavailable=False (before the taint
// patch), then the single taint/label patch, then the ready transition event.
func (c *RouteController) finalize(ctx context.Context, nodeName string, node *v1.Node) (time.Duration, error) {
	if node == nil {
		// Node not registered yet: the Node informer Add re-enqueues.
		return 0, nil
	}

	if err := c.setNetworkAvailable(nodeName, node); err != nil {
		return 0, err
	}
	if err := c.finalizeNode(ctx, node); err != nil {
		return 0, err
	}

	c.onReadyTransition(nodeName, node)

	return 0, nil
}

// handleConflict implements C10: emit one event per episode, bump the conflict
// counter every time, and back off with the node still tainted.
func (c *RouteController) handleConflict(nodeName string, node *v1.Node) (time.Duration, error) {
	metricConflictTotal.Inc()

	firstOfEpisode := c.markConflict(nodeName)
	if firstOfEpisode {
		c.eventf(node, v1.EventTypeWarning, reasonAllocationConflict,
			"pod CIDR destination allocated to another interface (expected during /24 reuse; self-resolves on VM delete)")
	}

	return 0, fmt.Errorf("pod CIDR destination for node %s allocated to another interface: %w",
		nodeName, sdn.ErrDestinationConflict)
}

// lookupAllocation lists by destination cidr within the reservation (both
// bounding filters) and returns the single allocation if present.
func (c *RouteController) lookupAllocation(ctx context.Context, cidr string) (sdn.PodCIDRAllocation, bool, error) {
	allocs, err := c.sdn.ListPodCIDRAllocations(ctx, sdn.ListPodCIDRAllocationsQuery{
		DestinationCIDR:         cidr,
		VPCPrefixReservationIDs: []string{c.cfg.VPCPrefixReservationID},
	})
	if err != nil {
		return sdn.PodCIDRAllocation{}, false, fmt.Errorf("failed to list allocation for cidr %s: %w", cidr, err)
	}
	if len(allocs) == 0 {
		return sdn.PodCIDRAllocation{}, false, nil
	}

	return allocs[0], true, nil
}

// fetchCiliumNode gets and converts the CiliumNode. gone is true when the object
// is missing or being deleted (no SDN calls follow — VM delete owns cleanup).
func (c *RouteController) fetchCiliumNode(nodeName string) (cn *ciliumNode, gone bool, err error) {
	obj, getErr := c.ciliumNodeLister.Get(nodeName)
	if getErr != nil {
		if apierrors.IsNotFound(getErr) {
			return nil, true, nil
		}

		return nil, false, fmt.Errorf("failed to get CiliumNode %s: %w", nodeName, getErr)
	}

	u, ok := asUnstructured(obj)
	if !ok {
		return nil, false, fmt.Errorf("%w: %s", ErrNotCiliumNode, nodeName)
	}

	cn, convErr := ciliumNodeFromUnstructured(u)
	if convErr != nil {
		return nil, false, convErr
	}
	if cn.DeletionTimestamp != nil {
		return nil, true, nil
	}

	return cn, false, nil
}

// clearOpIDIfNode clears the op-id label when the Node exists.
func (c *RouteController) clearOpIDIfNode(ctx context.Context, node *v1.Node) error {
	if node == nil {
		return nil
	}

	return c.clearOpIDLabel(ctx, node)
}

// getNode returns the Node from the lister, or nil if not yet registered.
func (c *RouteController) getNode(nodeName string) *v1.Node {
	node, err := c.nodeLister.Get(nodeName)
	if err != nil {
		return nil
	}

	return node
}

// isSteadyStateReady reports whether the node is already finalized: ready-at set,
// no pods-unroutable taint, NetworkUnavailable=False.
func (c *RouteController) isSteadyStateReady(node *v1.Node) bool {
	if node.Labels[ReadyAtLabel] == "" {
		return false
	}
	for i := range node.Spec.Taints {
		if node.Spec.Taints[i].Key == PodsUnroutableTaintKey {
			return false
		}
	}

	return hasNetworkAvailableCondition(node)
}

// forgetNode drops soft state and any tracked op for a departed node.
func (c *RouteController) forgetNode(nodeName string) {
	c.mu.Lock()
	delete(c.state, nodeName)
	c.mu.Unlock()
}
