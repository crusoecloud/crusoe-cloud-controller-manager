package routes

import (
	"context"
	"time"
)

// reconcile is the per-node create state machine. The full implementation lands
// in the reconcile-state-machine commit; this skeleton satisfies the worker-loop
// contract (returns requeueAfter, err) so the controller wiring compiles and is
// testable in isolation.
//
// Contract:
//
//	err != nil       -> AddRateLimited (backoff)
//	requeueAfter > 0 -> Forget + AddAfter (deliberate wait)
//	both zero        -> Forget (done)
func (c *RouteController) reconcile(_ context.Context, _ string) (time.Duration, error) {
	return 0, nil
}
