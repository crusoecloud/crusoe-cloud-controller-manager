package routes

import (
	"context"

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

const recorderBuffer = 64

// newTestController builds a minimally-wired RouteController for unit tests that
// exercise NIC/location resolution without the full informer/queue machinery.
func newTestController(cfg *Config, apiClient client.APIClient) *RouteController {
	return &RouteController{
		cfg:       cfg,
		apiClient: apiClient,
		state:     make(map[string]*nodeState),
	}
}

// reconcileHarness wraps a RouteController wired to fake clients and
// indexer-backed listers for driving reconcile()/reapOnce() directly (no
// informers).
type reconcileHarness struct {
	controller    *RouteController
	ciliumIndexer cache.Indexer
	nodeIndexer   cache.Indexer
}

// newReconcileHarness builds a reconcileHarness.
func newReconcileHarness(
	cfg *Config,
	kubeClient clientset.Interface,
	sdnClient sdn.PodCIDRAllocationClient,
	apiClient client.APIClient,
) *reconcileHarness {
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

	return &reconcileHarness{controller: c, ciliumIndexer: ciliumIndexer, nodeIndexer: nodeIndexer}
}

// addCiliumNode inserts an unstructured CiliumNode into the test lister.
func (h *reconcileHarness) addCiliumNode(u *unstructured.Unstructured) error {
	return h.ciliumIndexer.Add(u) //nolint:wrapcheck // test helper
}

// addNode inserts a Node into the test lister.
func (h *reconcileHarness) addNode(node *v1.Node) error {
	return h.nodeIndexer.Add(node) //nolint:wrapcheck // test helper
}

// syncNodeFromClient refreshes the node lister's copy from the fake clientset,
// emulating what the informer does after a patch. Tests call this between
// reconcile steps.
func (h *reconcileHarness) syncNodeFromClient(ctx context.Context) error {
	node, err := h.controller.kubeClient.CoreV1().Nodes().Get(ctx, rcNode, metav1.GetOptions{})
	if err != nil {
		return err //nolint:wrapcheck // test helper
	}

	return h.nodeIndexer.Update(node) //nolint:wrapcheck // test helper
}
