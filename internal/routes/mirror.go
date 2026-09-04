package routes

import (
	"context"
	"fmt"
	"net/netip"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	v1lister "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	controllersmetrics "k8s.io/component-base/metrics/prometheus/controllers"
	nodeutil "k8s.io/component-helpers/node/util"
	"k8s.io/klog/v2"
)

// mirrorControllerName is the component name for the PodCIDR mirror.
const mirrorControllerName = "crusoe-podcidr-mirror"

// PodCIDRMirror is a write-once controller that copies the first IPv4 /24 from
// CiliumNode.spec.ipam.podCIDRs into node.spec.podCIDR/podCIDRs, because
// cilium cluster-pool IPAM never populates them and the upstream route
// controller reads nothing else (section 7). It also seeds
// NodeNetworkUnavailable=True on nodes that have no such condition yet, so
// scheduling is gated until the route controller has programmed the route
// (section 11).
type PodCIDRMirror struct {
	kubeClient       clientset.Interface
	ciliumNodeLister cache.GenericLister
	nodeLister       v1lister.NodeLister

	ciliumNodesSynced cache.InformerSynced
	nodesSynced       cache.InformerSynced

	queue workqueue.TypedRateLimitingInterface[string] // key: node name
}

// NewPodCIDRMirror wires the mirror. Handlers enqueue by node name; there is no
// delete or Node-update handler, the field is immutable once set, so nothing
// needs reconciling after the first write (informer resync is the backstop).
func NewPodCIDRMirror(
	kubeClient clientset.Interface,
	ciliumNodeInformer cache.SharedIndexInformer,
	ciliumNodeLister cache.GenericLister,
	nodeInformer cache.SharedIndexInformer,
	nodeLister v1lister.NodeLister,
) (*PodCIDRMirror, error) {
	m := &PodCIDRMirror{
		kubeClient:        kubeClient,
		ciliumNodeLister:  ciliumNodeLister,
		nodeLister:        nodeLister,
		ciliumNodesSynced: ciliumNodeInformer.HasSynced,
		nodesSynced:       nodeInformer.HasSynced,
		queue: workqueue.NewTypedRateLimitingQueue(
			workqueue.DefaultTypedControllerRateLimiter[string]()),
	}

	if _, err := ciliumNodeInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { m.enqueueName(obj) },
		UpdateFunc: func(_, newObj any) { m.enqueueName(newObj) },
	}); err != nil {
		return nil, fmt.Errorf("failed to add CiliumNode event handler: %w", err)
	}
	// Node Add covers CiliumNode-before-Node ordering.
	if _, err := nodeInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) { m.enqueueName(obj) },
	}); err != nil {
		return nil, fmt.Errorf("failed to add Node event handler: %w", err)
	}

	return m, nil
}

// Run blocks; call via goroutine. It waits for cache sync, then runs a single
// worker until ctx is cancelled.
func (m *PodCIDRMirror) Run(ctx context.Context,
	controllerManagerMetrics *controllersmetrics.ControllerManagerMetrics,
) {
	defer utilruntime.HandleCrash()
	defer m.queue.ShutDown()

	controllerManagerMetrics.ControllerStarted(mirrorControllerName)
	defer controllerManagerMetrics.ControllerStopped(mirrorControllerName)

	klog.Info("Starting crusoe-podcidr-mirror")
	if !cache.WaitForCacheSync(ctx.Done(), m.ciliumNodesSynced, m.nodesSynced) {
		klog.Error("crusoe-podcidr-mirror: failed to sync informer caches")

		return
	}

	go wait.UntilWithContext(ctx, m.runWorker, time.Second)

	<-ctx.Done()
	klog.Info("Stopping crusoe-podcidr-mirror")
}

// enqueueName extracts a resource name (tolerating tombstones) and enqueues it.
func (m *PodCIDRMirror) enqueueName(obj any) {
	if name, ok := metaName(obj); ok {
		m.queue.Add(name)
	}
}

// metaName extracts a resource name from an informer object, tolerating
// tombstones (cache.DeletedFinalStateUnknown).
func metaName(obj any) (string, bool) {
	if m, ok := obj.(metav1.Object); ok {
		return m.GetName(), true
	}
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		if m, ok := tombstone.Obj.(metav1.Object); ok {
			return m.GetName(), true
		}
	}

	return "", false
}

func (m *PodCIDRMirror) runWorker(ctx context.Context) {
	for m.processNextItem(ctx) {
	}
}

func (m *PodCIDRMirror) processNextItem(ctx context.Context) bool {
	name, quit := m.queue.Get()
	if quit {
		return false
	}
	defer m.queue.Done(name)

	if err := m.reconcile(ctx, name); err != nil {
		klog.ErrorS(err, "podcidr mirror reconcile failed", "node", name)
		m.queue.AddRateLimited(name)

		return true
	}
	m.queue.Forget(name)

	return true
}

// reconcile seeds the NetworkUnavailable condition (section 11) and mirrors the
// CiliumNode v4 /24 onto the Node, once (section 7). The mirror step is a no-op
// when the Node already has a podCIDR or the CiliumNode has no v4 prefix yet;
// an absent Node is a no-op for both.
func (m *PodCIDRMirror) reconcile(ctx context.Context, name string) error {
	node, err := m.nodeLister.Get(name)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to get node %s: %w", name, err)
	}

	if cerr := m.ensureNetworkUnavailable(node); cerr != nil {
		return cerr
	}

	// Write-once: ValidateNodeUpdate allows unset->set and rejects any change
	// afterwards, so there is nothing to reconcile once podCIDR is set.
	if node.Spec.PodCIDR != "" {
		return nil
	}

	cidr, err := m.firstV4PodCIDR(name)
	if err != nil {
		return err
	}
	if cidr == "" {
		// No v4 prefix yet, the CiliumNode Update event re-drives.
		return nil
	}

	return m.patchPodCIDR(ctx, name, cidr)
}

// ensureNetworkUnavailable seeds NodeNetworkUnavailable=True on a node that has
// no such condition, so the KCM node-lifecycle controller taints it with
// node.kubernetes.io/network-unavailable until the upstream route controller
// flips the condition to False after CreateRoute. Nothing else provides the
// initial value: the kubelet sets none for external providers, the route
// controller sets none for a node without a podCIDR, and KCM strips a
// kubelet-registered network-unavailable taint whenever the condition is not
// True. An existing condition, whichever value, is never overridden. Same
// helper and reason string as the route controller, so the history reads as
// one owner.
func (m *PodCIDRMirror) ensureNetworkUnavailable(node *v1.Node) error {
	if _, cond := nodeutil.GetNodeCondition(&node.Status, v1.NodeNetworkUnavailable); cond != nil {
		return nil
	}

	err := nodeutil.SetNodeCondition(m.kubeClient, types.NodeName(node.Name), v1.NodeCondition{
		Type:               v1.NodeNetworkUnavailable,
		Status:             v1.ConditionTrue,
		Reason:             "NoRouteCreated",
		Message:            "crusoe-ccm: pod cidr route not yet programmed",
		LastTransitionTime: metav1.Now(),
	})
	if err != nil {
		return fmt.Errorf("failed to seed NetworkUnavailable on node %s: %w", node.Name, err)
	}

	klog.InfoS("seeded NetworkUnavailable=True until the route is programmed", "node", node.Name)

	return nil
}

// firstV4PodCIDR returns the first IPv4 prefix from the CiliumNode's
// spec.ipam.podCIDRs, or "" if the CiliumNode is absent or has none yet.
func (m *PodCIDRMirror) firstV4PodCIDR(name string) (string, error) {
	obj, err := m.ciliumNodeLister.Get(name)
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("failed to get CiliumNode %s: %w", name, err)
	}

	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrNotCiliumNode, name)
	}
	cn, err := ciliumNodeFromUnstructured(u)
	if err != nil {
		return "", err
	}

	for _, c := range cn.PodCIDRs {
		p, perr := netip.ParsePrefix(c)
		if perr != nil {
			klog.ErrorS(perr, "skipping unparseable podCIDR", "node", name, "cidr", c)

			continue
		}
		if p.Addr().Is4() {
			return c, nil
		}
	}

	return "", nil
}

// patchPodCIDR sets both spec.podCIDR and spec.podCIDRs to the same single v4
// cidr in one strategic-merge patch (they must be equal and one-element).
func (m *PodCIDRMirror) patchPodCIDR(ctx context.Context, name, cidr string) error {
	patch := fmt.Sprintf(`{"spec":{"podCIDR":%q,"podCIDRs":[%q]}}`, cidr, cidr)
	if _, err := m.kubeClient.CoreV1().Nodes().Patch(
		ctx, name, types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{},
	); err != nil {
		return fmt.Errorf("failed to patch podCIDR on node %s: %w", name, err)
	}

	klog.InfoS("mirrored pod cidr to node spec", "node", name, "cidr", cidr)

	return nil
}
