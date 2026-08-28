package routes

import (
	"context"

	"github.com/crusoecloud/crusoe-cloud-controller-manager/internal/client"
	"github.com/crusoecloud/crusoe-cloud-controller-manager/internal/routes/sdn"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// This file exposes unexported symbols to the external routes_test package so
// tests can exercise internal helpers without widening the production API.

// CiliumNode is the exported alias of the internal ciliumNode projection, for
// tests.
type CiliumNode = ciliumNode

// CiliumNodeFromUnstructured exposes ciliumNodeFromUnstructured to tests.
func CiliumNodeFromUnstructured(u *unstructured.Unstructured) (*CiliumNode, error) {
	return ciliumNodeFromUnstructured(u)
}

// NewTestController builds a minimally-wired RouteController for unit tests that
// exercise NIC/location resolution without the full informer/queue machinery.
func NewTestController(cfg *Config, apiClient client.APIClient) *RouteController {
	return &RouteController{
		cfg:       cfg,
		apiClient: apiClient,
		state:     make(map[string]*nodeState),
	}
}

// ResolveNIC exposes resolveNIC to tests.
func (c *RouteController) ResolveNIC(ctx context.Context, nodeName string, node *v1.Node) (string, error) {
	return c.resolveNIC(ctx, nodeName, node)
}

// ResolveLocationFromCluster exposes resolveLocationFromCluster to tests.
func ResolveLocationFromCluster(ctx context.Context,
	apiClient client.APIClient, projectID, clusterName string,
) (string, error) {
	return resolveLocationFromCluster(ctx, apiClient, projectID, clusterName)
}

// Location returns the (possibly instance-derived) resolved location, for tests.
func (c *RouteController) Location() string {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.cfg.Location
}

// ProjectID returns the (possibly instance-derived) project id, for tests.
func (c *RouteController) ProjectID() string {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.cfg.ProjectID
}

// OpTracker is the exported alias of the internal opTracker, for tests.
type OpTracker = opTracker

// NewOpTracker exposes newOpTracker to tests.
func NewOpTracker() *OpTracker { return newOpTracker() }

// RecordResults exposes recordResults to tests.
func (t *OpTracker) RecordResults(ops []sdn.Operation, enqueue func(string)) {
	t.recordResults(ops, enqueue)
}

// PendingIDs exposes pendingIDs to tests.
func (t *OpTracker) PendingIDs() []string { return t.pendingIDs() }

// TrackForTest exposes Track to tests (Track is already exported on the type).
func (t *OpTracker) TrackForTest(opID, nodeKey string) { t.Track(opID, nodeKey) }

// NodeNeedsWork exposes nodeNeedsWork to tests.
func NodeNeedsWork(node *v1.Node) bool { return nodeNeedsWork(node) }

// MetaName exposes metaName to tests.
func MetaName(obj any) (string, bool) { return metaName(obj) }

// MaxOpMisses exposes the drop threshold to tests.
const MaxOpMisses = maxOpMisses
