package routes

import "context"

// reapOnce is one pass of the periodic desired-vs-actual sweep that owns all
// allocation deletes. The full implementation lands in the reaper commit; this
// skeleton lets the controller launch its reaper goroutine.
func (c *RouteController) reapOnce(_ context.Context) {
}
