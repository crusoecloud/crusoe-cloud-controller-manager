package routes

import (
	"sync"
	"testing"

	"github.com/crusoecloud/crusoe-cloud-controller-manager/internal/routes/sdn"
)

func inProgress(opID string) sdn.Operation {
	return sdn.Operation{OperationID: opID, State: sdn.OperationStateInProgress}
}

func succeeded(opID, allocID string) sdn.Operation {
	return sdn.Operation{OperationID: opID, State: sdn.OperationStateSucceeded, AllocationIDs: []string{allocID}}
}

func TestOpTracker_TerminalDispatchesAndStashes(t *testing.T) {
	t.Parallel()
	tr := newOpTracker()
	tr.Track("op-1", "node-a")

	var enqueued []string
	tr.recordResults([]string{"op-1"}, []sdn.Operation{succeeded("op-1", "al-1")}, func(k string) {
		enqueued = append(enqueued, k)
	})

	if len(enqueued) != 1 || enqueued[0] != "node-a" {
		t.Fatalf("expected node-a enqueued, got %v", enqueued)
	}
	op, ok := tr.TakeResult("op-1")
	if !ok {
		t.Fatalf("expected terminal result stashed")
	}
	if op.State != sdn.OperationStateSucceeded {
		t.Fatalf("expected SUCCEEDED, got %s", op.State)
	}
	// After TakeResult it is gone, and no longer pending.
	if _, ok := tr.TakeResult("op-1"); ok {
		t.Fatalf("result should be consumed once")
	}
	if len(tr.pendingIDs()) != 0 {
		t.Fatalf("terminal op should not remain pending")
	}
}

func TestOpTracker_InProgressStaysPending(t *testing.T) {
	t.Parallel()
	tr := newOpTracker()
	tr.Track("op-1", "node-a")

	var enqueued []string
	tr.recordResults([]string{"op-1"}, []sdn.Operation{inProgress("op-1")}, func(k string) {
		enqueued = append(enqueued, k)
	})

	if len(enqueued) != 0 {
		t.Fatalf("in-progress op should not dispatch, got %v", enqueued)
	}
	if len(tr.pendingIDs()) != 1 {
		t.Fatalf("in-progress op should remain pending")
	}
}

func TestOpTracker_DropsAfterMisses(t *testing.T) {
	t.Parallel()
	tr := newOpTracker()
	tr.Track("op-gone", "node-a")

	var enqueued []string
	enqueue := func(k string) { enqueued = append(enqueued, k) }

	// Op absent from every poll: increments misses. Dropped at MaxOpMisses.
	for i := range maxOpMisses {
		tr.recordResults([]string{"op-gone"}, []sdn.Operation{}, enqueue)
		if i < maxOpMisses-1 {
			if len(tr.pendingIDs()) != 1 {
				t.Fatalf("op should still be pending before miss limit (i=%d)", i)
			}
		}
	}
	if len(tr.pendingIDs()) != 0 {
		t.Fatalf("op should be dropped after %d misses", maxOpMisses)
	}
	if len(enqueued) != 1 || enqueued[0] != "node-a" {
		t.Fatalf("dropped op should enqueue its node once, got %v", enqueued)
	}
}

func TestOpTracker_TrackIdempotent(t *testing.T) {
	t.Parallel()
	tr := newOpTracker()
	tr.Track("op-1", "node-a")
	tr.Track("op-1", "node-a")
	if len(tr.pendingIDs()) != 1 {
		t.Fatalf("Track should be idempotent, got %d pending", len(tr.pendingIDs()))
	}
}

func TestOpTracker_ForgetRemovesBoth(t *testing.T) {
	t.Parallel()
	tr := newOpTracker()
	tr.Track("op-1", "node-a")
	tr.recordResults([]string{"op-1"}, []sdn.Operation{succeeded("op-1", "al-1")}, func(string) {})
	tr.Forget("op-1")
	if _, ok := tr.TakeResult("op-1"); ok {
		t.Fatalf("Forget should drop the stashed result")
	}
}

func TestOpTracker_ConcurrentSafe(t *testing.T) {
	t.Parallel()
	tr := newOpTracker()
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			id := "op-" + string(rune('a'+n))
			tr.Track(id, "node")
			tr.recordResults([]string{id}, []sdn.Operation{succeeded(id, "al")}, func(string) {})
			tr.TakeResult(id)
		}(i)
	}
	wg.Wait()
}
