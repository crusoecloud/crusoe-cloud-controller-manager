package routes

import (
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/klog/v2"
)

// asUnstructured coerces a lister object to *unstructured.Unstructured.
func asUnstructured(obj any) (*unstructured.Unstructured, bool) {
	u, ok := obj.(*unstructured.Unstructured)

	return u, ok
}

// eventf records an event on the Node when it exists; otherwise it is a no-op
// (events are skipped when the Node is absent, §13).
func (c *RouteController) eventf(node *v1.Node, eventType, reason, messageFmt string, args ...any) {
	if node == nil {
		return
	}
	c.recorder.Eventf(node, eventType, reason, messageFmt, args...)
}

// warnMultiCIDR logs a warning once per node when a CiliumNode carries more than
// one podCIDR (only podCIDRs[0] is routed).
func (c *RouteController) warnMultiCIDR(nodeName string, cidrs []string) {
	st := c.ensureState(nodeName)

	c.mu.Lock()
	already := st.warnedMultiCIDR
	st.warnedMultiCIDR = true
	c.mu.Unlock()

	if !already {
		klog.Warningf("node %s has %d podCIDRs %v; routing only the first", nodeName, len(cidrs), cidrs)
	}
}

// ensureState returns the soft state for a node, creating it if absent.
func (c *RouteController) ensureState(nodeName string) *nodeState {
	c.mu.Lock()
	defer c.mu.Unlock()

	st, ok := c.state[nodeName]
	if !ok {
		st = &nodeState{}
		c.state[nodeName] = st
	}

	return st
}

// setAllocationID records the resolved allocation id in soft state.
func (c *RouteController) setAllocationID(nodeName, allocID string) {
	st := c.ensureState(nodeName)

	c.mu.Lock()
	st.allocationID = allocID
	c.mu.Unlock()
}

// startCreateTimer records the create start time once (for provision latency).
func (c *RouteController) startCreateTimer(nodeName string) {
	st := c.ensureState(nodeName)

	c.mu.Lock()
	if st.createStart.IsZero() {
		st.createStart = time.Now()
	}
	c.mu.Unlock()
}

// observeProvisionLatency records create-issued -> SUCCEEDED latency if a create
// start time was captured.
func (c *RouteController) observeProvisionLatency(nodeName string) {
	c.mu.Lock()
	st, ok := c.state[nodeName]
	var start time.Time
	if ok {
		start = st.createStart
	}
	c.mu.Unlock()

	if !start.IsZero() {
		metricProvisionSeconds.Observe(time.Since(start).Seconds())
	}
}

// markConflict sets conflictSince if unset and reports whether this is the first
// occurrence of the current episode (used to dedup the conflict event).
func (c *RouteController) markConflict(nodeName string) bool {
	st := c.ensureState(nodeName)

	c.mu.Lock()
	defer c.mu.Unlock()

	if st.conflictSince.IsZero() {
		st.conflictSince = time.Now()

		return true
	}

	return false
}

// markReady flags the node ready in soft state (steady-state fast path).
func (c *RouteController) markReady(nodeName string) {
	st := c.ensureState(nodeName)

	c.mu.Lock()
	st.ready = true
	st.conflictSince = time.Time{}
	c.mu.Unlock()
}

// onReadyTransition emits the AllocationCreated event and success metric on the
// first transition to ready (C11), clearing any conflict episode.
func (c *RouteController) onReadyTransition(nodeName string, node *v1.Node) {
	st := c.ensureState(nodeName)

	c.mu.Lock()
	first := !st.ready
	st.ready = true
	st.conflictSince = time.Time{}
	c.mu.Unlock()

	if first {
		c.eventf(node, v1.EventTypeNormal, reasonAllocationCreated,
			"crusoe-route-controller programmed SDN pod CIDR allocation")
		metricReconcileTotal.WithLabelValues(resultSuccess).Inc()
	}
}
