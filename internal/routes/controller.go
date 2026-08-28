package routes

import (
	"sync"

	"github.com/crusoecloud/crusoe-cloud-controller-manager/internal/client"
)

// RouteController creates one SDN pod CIDR allocation per node and gates
// scheduling (via the pods-unroutable taint) until the allocation is ready. The
// struct is assembled incrementally across the commit series; fields required
// by NIC/location resolution live here from the start.
//
// Additional wiring (kube/dynamic clients, listers, workqueue, opTracker, event
// recorder) is added by the controller-skeleton commit.
type RouteController struct {
	cfg       *Config
	apiClient client.APIClient // NIC + cluster resolution

	mu    sync.Mutex            // guards state + cfg.Location derivation
	state map[string]*nodeState // key: node name; soft cache only
}

// nodeState is soft state; everything durable lives on the Node (labels) or in
// SDN (List APIs). It is rebuilt on restart. Fields are introduced as the state
// machine that uses them lands; NIC resolution needs only nicID.
type nodeState struct {
	nicID string // immutable per instance once resolved
}
