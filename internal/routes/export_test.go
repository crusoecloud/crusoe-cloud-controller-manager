package routes

import (
	"context"
	"time"

	"github.com/crusoecloud/crusoe-cloud-controller-manager/internal/client"
	"github.com/crusoecloud/crusoe-cloud-controller-manager/internal/routes/sdn"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientset "k8s.io/client-go/kubernetes"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
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

// ReconcileHarness wraps a RouteController wired to fake clients and
// indexer-backed listers for driving reconcile()/reapOnce() directly (no
// informers).
type ReconcileHarness struct {
	Controller    *RouteController
	ciliumIndexer cache.Indexer
	nodeIndexer   cache.Indexer
}

// NewReconcileHarness builds a ReconcileHarness.
func NewReconcileHarness(
	cfg *Config,
	kubeClient clientset.Interface,
	sdnClient sdn.PodCIDRAllocationClient,
	apiClient client.APIClient,
) *ReconcileHarness {
	ciliumIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	nodeIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})

	c := &RouteController{
		cfg:        cfg,
		kubeClient: kubeClient,
		sdn:        sdnClient,
		apiClient:  apiClient,
		ciliumNodeLister: cache.NewGenericLister(
			ciliumIndexer, schema.GroupResource{Group: "cilium.io", Resource: "ciliumnodes"}),
		nodeLister: corev1listers.NewNodeLister(nodeIndexer),
		queue:      workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()),
		tracker:    newOpTracker(),
		recorder:   record.NewFakeRecorder(recorderBuffer),
		state:      make(map[string]*nodeState),
	}

	return &ReconcileHarness{Controller: c, ciliumIndexer: ciliumIndexer, nodeIndexer: nodeIndexer}
}

const recorderBuffer = 64

// Reconcile drives reconcile for a node.
func (h *ReconcileHarness) Reconcile(ctx context.Context, nodeName string) (time.Duration, error) {
	return h.Controller.reconcile(ctx, nodeName)
}

// ReapOnce drives one reaper pass.
func (h *ReconcileHarness) ReapOnce(ctx context.Context) { h.Controller.reapOnce(ctx) }

// Tracker exposes the opTracker.
func (h *ReconcileHarness) Tracker() *OpTracker { return h.Controller.tracker }

// PollOpsOnce drives one poll-loop tick.
func (h *ReconcileHarness) PollOpsOnce(ctx context.Context) { h.Controller.pollOpsOnce(ctx) }

// AddCiliumNode inserts an unstructured CiliumNode into the test lister.
func (h *ReconcileHarness) AddCiliumNode(u *unstructured.Unstructured) error {
	return h.ciliumIndexer.Add(u) //nolint:wrapcheck // test helper
}

// DeleteCiliumNode removes a CiliumNode from the test lister.
func (h *ReconcileHarness) DeleteCiliumNode(u *unstructured.Unstructured) error {
	return h.ciliumIndexer.Delete(u) //nolint:wrapcheck // test helper
}

// AddNode inserts a Node into the test lister.
func (h *ReconcileHarness) AddNode(node *v1.Node) error {
	return h.nodeIndexer.Add(node) //nolint:wrapcheck // test helper
}

// NativeModeEnabled reports whether the loaded config would start the controller
// (mirrors the register mode-gate), for fail-fast tests.
func NativeModeEnabled(cfg *Config) bool {
	return cfg.RoutingMode == RoutingModeNative
}

// SyncNodeFromClient refreshes the node lister's copy from the fake clientset,
// emulating what the informer does after a patch. Tests call this between
// reconcile steps.
func (h *ReconcileHarness) SyncNodeFromClient(ctx context.Context, name string) error {
	node, err := h.Controller.kubeClient.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err //nolint:wrapcheck // test helper
	}

	return h.nodeIndexer.Update(node) //nolint:wrapcheck // test helper
}
