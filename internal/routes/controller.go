package routes

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/crusoecloud/crusoe-cloud-controller-manager/internal/client"
	"github.com/crusoecloud/crusoe-cloud-controller-manager/internal/routes/sdn"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	coreinformers "k8s.io/client-go/informers"
	corev1informers "k8s.io/client-go/informers/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	v1lister "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	controllersmetrics "k8s.io/component-base/metrics/prometheus/controllers"
	"k8s.io/klog/v2"
)

const (
	// controllerName is the component name used for events and metrics.
	controllerName = "crusoe-route-controller"

	// PodsUnroutableTaintKey is applied by kubelet via --register-with-taints;
	// the CCM only removes it.
	PodsUnroutableTaintKey = "crusoe.ai/pods-unroutable"

	// OpIDLabel carries the in-flight CreatePodCIDRAllocations operation id,
	// written immediately after the create RPC returns. Its presence is the
	// invariant "an allocation create is in flight for this node".
	OpIDLabel = "crusoe.ai/pod-cidr-allocation-op-id"
	// ReadyAtLabel is the CCM-observed completion time as Unix epoch SECONDS
	// (decimal string — RFC3339 is an illegal label value), set in the same
	// final patch that removes the taint.
	ReadyAtLabel = "crusoe.ai/pod-cidr-allocation-ready-at"
)

// RouteController creates one SDN pod CIDR allocation per node and gates
// scheduling (via the pods-unroutable taint) until the allocation is ready.
type RouteController struct {
	cfg        *Config
	kubeClient clientset.Interface
	dynClient  dynamic.Interface
	sdn        sdn.PodCIDRAllocationClient
	apiClient  client.APIClient // NIC + cluster resolution

	ciliumNodeLister  cache.GenericLister
	ciliumNodesSynced cache.InformerSynced
	nodeLister        v1lister.NodeLister
	nodesSynced       cache.InformerSynced

	queue workqueue.TypedRateLimitingInterface[string] // key: node name

	tracker *opTracker // single batched poll loop, no per-node goroutines

	broadcaster record.EventBroadcaster
	recorder    record.EventRecorder

	mu    sync.Mutex            // guards state + cfg.Location derivation
	state map[string]*nodeState // key: node name; soft cache only
}

// nodeState is soft state; everything durable lives on the Node (labels) or in
// SDN (List APIs). It is rebuilt on restart. Fields beyond nicID are added by
// the reconcile-state-machine commit that uses them.
type nodeState struct {
	nicID string // immutable per instance once resolved
}

// NewRouteController wires the controller. Clients and informers are supplied by
// the registration wrapper.
func NewRouteController(
	cfg *Config,
	kubeClient clientset.Interface,
	dynClient dynamic.Interface,
	sdnClient sdn.PodCIDRAllocationClient,
	apiClient client.APIClient,
	ciliumNodeInformer coreinformers.GenericInformer,
	nodeInformer corev1informers.NodeInformer,
) (*RouteController, error) {
	broadcaster := record.NewBroadcaster()
	recorder := broadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: controllerName})

	c := &RouteController{
		cfg:               cfg,
		kubeClient:        kubeClient,
		dynClient:         dynClient,
		sdn:               sdnClient,
		apiClient:         apiClient,
		ciliumNodeLister:  ciliumNodeInformer.Lister(),
		ciliumNodesSynced: ciliumNodeInformer.Informer().HasSynced,
		nodeLister:        nodeInformer.Lister(),
		nodesSynced:       nodeInformer.Informer().HasSynced,
		queue: workqueue.NewTypedRateLimitingQueue(
			workqueue.DefaultTypedControllerRateLimiter[string]()),
		tracker:     newOpTracker(),
		broadcaster: broadcaster,
		recorder:    recorder,
		state:       make(map[string]*nodeState),
	}

	if _, err := ciliumNodeInformer.Informer().AddEventHandler(c.ciliumNodeHandlers()); err != nil {
		return nil, fmt.Errorf("failed to add CiliumNode event handler: %w", err)
	}
	if _, err := nodeInformer.Informer().AddEventHandler(c.nodeHandlers()); err != nil {
		return nil, fmt.Errorf("failed to add Node event handler: %w", err)
	}

	return c, nil
}

// Run blocks; call via goroutine. It starts the event broadcaster, waits for
// cache sync, then launches the reconcile workers, the opTracker poll loop and
// the reaper, all stopping on ctx cancellation.
func (c *RouteController) Run(ctx context.Context,
	controllerManagerMetrics *controllersmetrics.ControllerManagerMetrics,
) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	controllerManagerMetrics.ControllerStarted(controllerName)
	defer controllerManagerMetrics.ControllerStopped(controllerName)

	klog.Info("Starting crusoe-route-controller")
	c.broadcaster.StartStructuredLogging(0)
	c.broadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: c.kubeClient.CoreV1().Events("")})
	defer c.broadcaster.Shutdown()

	if !cache.WaitForCacheSync(ctx.Done(), c.ciliumNodesSynced, c.nodesSynced) {
		klog.Error("crusoe-route-controller: failed to sync informer caches")

		return
	}

	for range c.cfg.Workers {
		go wait.UntilWithContext(ctx, c.runWorker, time.Second)
	}
	go wait.UntilWithContext(ctx, c.pollOpsOnce, c.cfg.PollInterval)
	go wait.UntilWithContext(ctx, c.reapOnce, c.cfg.ReaperInterval)

	<-ctx.Done()
	klog.Info("Stopping crusoe-route-controller")
}

// enqueue adds a node key to the workqueue.
func (c *RouteController) enqueue(nodeName string) {
	c.queue.Add(nodeName)
}

// runWorker processes queue items until the queue shuts down.
func (c *RouteController) runWorker(ctx context.Context) {
	for c.processNextItem(ctx) {
	}
}

// processNextItem dequeues one key, reconciles it, and applies the requeue
// contract:
//
//	err != nil       -> AddRateLimited (backoff)
//	requeueAfter > 0 -> Forget + AddAfter (deliberate wait, not an error)
//	both zero        -> Forget (done)
func (c *RouteController) processNextItem(ctx context.Context) bool {
	key, quit := c.queue.Get()
	if quit {
		return false
	}
	defer c.queue.Done(key)

	requeueAfter, err := c.reconcile(ctx, key)
	switch {
	case err != nil:
		metricReconcileTotal.WithLabelValues(resultError).Inc()
		klog.ErrorS(err, "reconcile failed", "node", key)
		c.queue.AddRateLimited(key)
	case requeueAfter > 0:
		metricReconcileTotal.WithLabelValues(resultRequeue).Inc()
		c.queue.Forget(key)
		c.queue.AddAfter(key, requeueAfter)
	default:
		c.queue.Forget(key)
	}

	return true
}

// ciliumNodeHandlers enqueues on Add/Update and clears soft state on Delete
// (no SDN calls — VM delete owns cleanup).
func (c *RouteController) ciliumNodeHandlers() cache.ResourceEventHandlerFuncs {
	return cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			if name, ok := metaName(obj); ok {
				c.enqueue(name)
			}
		},
		UpdateFunc: func(_, newObj any) {
			if name, ok := metaName(newObj); ok {
				c.enqueue(name)
			}
		},
		DeleteFunc: func(obj any) {
			if name, ok := metaName(obj); ok {
				c.onCiliumNodeDeleted(name)
			}
		},
	}
}

// onCiliumNodeDeleted clears in-memory soft state. It makes ZERO SDN calls: the
// VM delete owns allocation cleanup (KM MR 1314).
func (c *RouteController) onCiliumNodeDeleted(nodeName string) {
	c.mu.Lock()
	delete(c.state, nodeName)
	c.mu.Unlock()

	klog.V(4).InfoS("CiliumNode deleted; clearing soft state (no SDN calls)", "node", nodeName)
}

// nodeHandlers enqueue on Add and on Update only when the node still needs work
// (carries the taint or lacks NetworkUnavailable=False), and re-register op-id
// labels with the tracker on sync (crash/failover recovery).
func (c *RouteController) nodeHandlers() cache.ResourceEventHandlerFuncs {
	return cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			node, ok := obj.(*v1.Node)
			if !ok {
				return
			}
			c.recoverTrackedOp(node)
			c.enqueue(node.Name)
		},
		UpdateFunc: func(_, newObj any) {
			node, ok := newObj.(*v1.Node)
			if !ok {
				return
			}
			if nodeNeedsWork(node) {
				c.enqueue(node.Name)
			}
		},
	}
}

// recoverTrackedOp re-registers an op-id label with the tracker if it is not
// already being polled (leader failover / restart recovery).
func (c *RouteController) recoverTrackedOp(node *v1.Node) {
	opID := node.Labels[OpIDLabel]
	if opID == "" {
		return
	}
	if !c.tracker.isTracking(opID) {
		c.tracker.Track(opID, node.Name)
	}
}

// nodeNeedsWork reports whether a Node still requires reconcile attention.
func nodeNeedsWork(node *v1.Node) bool {
	for i := range node.Spec.Taints {
		if node.Spec.Taints[i].Key == PodsUnroutableTaintKey {
			return true
		}
	}
	if node.Labels[OpIDLabel] != "" {
		return true
	}

	return !hasNetworkAvailableCondition(node)
}

// hasNetworkAvailableCondition reports whether NetworkUnavailable=False is set.
func hasNetworkAvailableCondition(node *v1.Node) bool {
	for i := range node.Status.Conditions {
		cond := node.Status.Conditions[i]
		if cond.Type == v1.NodeNetworkUnavailable {
			return cond.Status == v1.ConditionFalse
		}
	}

	return false
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
