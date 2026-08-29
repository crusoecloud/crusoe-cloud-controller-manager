package routes

import (
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// eventf records an event on the Node when it exists; otherwise it is a no-op
// (events are skipped when the Node is absent, §13).
func (c *RouteController) eventf(node *v1.Node, eventType, reason, messageFmt string, args ...any) {
	if node == nil {
		return
	}
	c.recorder.Eventf(node, eventType, reason, messageFmt, args...)
}

// withState runs fn against the node's soft state under c.mu, creating the entry
// if absent. Keep side effects that must not hold the lock (metrics, events)
// outside fn, capturing what they need via closure.
func (c *RouteController) withState(nodeName string, fn func(*nodeState)) {
	c.mu.Lock()
	defer c.mu.Unlock()

	st, ok := c.state[nodeName]
	if !ok {
		st = &nodeState{}
		c.state[nodeName] = st
	}
	fn(st)
}

// warnMultiCIDR logs a warning once per node when a CiliumNode carries more than
// one podCIDR (only podCIDRs[0] is routed).
func (c *RouteController) warnMultiCIDR(nodeName string, cidrs []string) {
	var already bool
	c.withState(nodeName, func(st *nodeState) {
		already = st.warnedMultiCIDR
		st.warnedMultiCIDR = true
	})

	if !already {
		klog.Warningf("node %s has %d podCIDRs %v; routing only the first", nodeName, len(cidrs), cidrs)
	}
}

// setAllocationID records the resolved allocation id in soft state.
func (c *RouteController) setAllocationID(nodeName, allocID string) {
	c.withState(nodeName, func(st *nodeState) { st.allocationID = allocID })
}

// startCreateTimer records the create start time once (for provision latency).
func (c *RouteController) startCreateTimer(nodeName string) {
	c.withState(nodeName, func(st *nodeState) {
		if st.createStart.IsZero() {
			st.createStart = time.Now()
		}
	})
}

// observeProvisionLatency records create-issued -> SUCCEEDED latency if a create
// start time was captured.
func (c *RouteController) observeProvisionLatency(nodeName string) {
	var start time.Time
	c.withState(nodeName, func(st *nodeState) { start = st.createStart })

	if !start.IsZero() {
		metricProvisionSeconds.Observe(time.Since(start).Seconds())
	}
}

// markConflict sets conflictSince if unset and reports whether this is the first
// occurrence of the current episode (used to dedup the conflict event).
func (c *RouteController) markConflict(nodeName string) bool {
	var first bool
	c.withState(nodeName, func(st *nodeState) {
		if st.conflictSince.IsZero() {
			st.conflictSince = time.Now()
			first = true
		}
	})

	return first
}

// markReady flags the node ready in soft state (steady-state fast path).
func (c *RouteController) markReady(nodeName string) {
	c.withState(nodeName, func(st *nodeState) {
		st.ready = true
		st.conflictSince = time.Time{}
	})
}

// onReadyTransition emits the AllocationCreated event and success metric on the
// first transition to ready (C11), clearing any conflict episode.
func (c *RouteController) onReadyTransition(nodeName string, node *v1.Node) {
	var first bool
	c.withState(nodeName, func(st *nodeState) {
		first = !st.ready
		st.ready = true
		st.conflictSince = time.Time{}
	})

	if first {
		c.eventf(node, v1.EventTypeNormal, reasonAllocationCreated,
			"crusoe-route-controller programmed SDN pod CIDR allocation")
		metricReconcileTotal.WithLabelValues(resultSuccess).Inc()
	}
}
