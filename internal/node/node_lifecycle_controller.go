package node

import (
	"context"
	"errors"
	"fmt"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	coreinformers "k8s.io/client-go/informers/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	v1lister "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/record"
	cloudprovider "k8s.io/cloud-provider"
	cloudproviderapi "k8s.io/cloud-provider/api"
	cloudnodeutil "k8s.io/cloud-provider/node/helpers"
	controllersmetrics "k8s.io/component-base/metrics/prometheus/controllers"
	nodeutil "k8s.io/component-helpers/node/util"
	"k8s.io/klog/v2"
)

const (
	deleteNodeEvent = "DeletingNode"
)

//nolint:gochecknoglobals // can't construct const structs
var ShutdownTaint = &v1.Taint{
	Key:    cloudproviderapi.TaintNodeShutdown,
	Effect: v1.TaintEffectNoSchedule,
}

var ErrInstancesNotSupported = errors.New("cloud provider does not support instances")

var (
	ErrNoCloudProvider     = errors.New("no cloud provider provided")
	ErrNilKubernetesClient = errors.New("kubernetes client is nil")
)

// CloudNodeLifecycleController is responsible for deleting/updating kubernetes
// nodes that have been deleted/shutdown on the cloud provider.
type CloudNodeLifecycleController struct {
	kubeClient clientset.Interface
	nodeLister v1lister.NodeLister

	broadcaster record.EventBroadcaster
	recorder    record.EventRecorder

	cloud cloudprovider.Interface

	// Value controlling NodeController monitoring period, i.e. how often does NodeController
	// check node status posted from kubelet. This value should be lower than nodeMonitorGracePeriod
	// set in controller-manager
	nodeMonitorPeriod time.Duration
}

func NewCloudNodeLifecycleController(
	nodeInformer coreinformers.NodeInformer,
	kubeClient clientset.Interface,
	cloud cloudprovider.Interface,
	nodeMonitorPeriod time.Duration,
) (*CloudNodeLifecycleController, error) {
	eventBroadcaster := record.NewBroadcaster()
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "cloud-node-lifecycle-controller"})

	if kubeClient == nil {
		return nil, ErrNilKubernetesClient
	}

	if cloud == nil {
		return nil, ErrNoCloudProvider
	}

	_, instancesSupported := cloud.Instances()
	_, instancesV2Supported := cloud.InstancesV2()
	if !instancesSupported && !instancesV2Supported {
		return nil, ErrInstancesNotSupported
	}

	c := &CloudNodeLifecycleController{
		kubeClient:        kubeClient,
		nodeLister:        nodeInformer.Lister(),
		broadcaster:       eventBroadcaster,
		recorder:          recorder,
		cloud:             cloud,
		nodeMonitorPeriod: nodeMonitorPeriod,
	}

	return c, nil
}

// Run starts the main loop for this controller. Run is blocking so should
// be called via a goroutine.
func (c *CloudNodeLifecycleController) Run(ctx context.Context,
	controllerManagerMetrics *controllersmetrics.ControllerManagerMetrics,
) {
	defer utilruntime.HandleCrash()
	controllerManagerMetrics.ControllerStarted("cloud-node-lifecycle")
	defer controllerManagerMetrics.ControllerStopped("cloud-node-lifecycle")

	// Start event processing pipeline.
	klog.Info("Sending events to api server")
	c.broadcaster.StartStructuredLogging(0)
	c.broadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: c.kubeClient.CoreV1().Events("")})
	defer c.broadcaster.Shutdown()

	// The following loops run communicate with the APIServer with a worst case complexity
	// of O(num_nodes) per cycle. These functions are justified here because these events fire
	// very infrequently. DO NOT MODIFY this to perform frequent operations.

	// Start a loop to periodically check if any nodes have been
	// deleted or shutdown from the cloudprovider
	wait.UntilWithContext(ctx, c.MonitorNodes, c.nodeMonitorPeriod)
}

// MonitorNodes checks to see if nodes in the cluster have been deleted
// or shutdown. If deleted, it deletes the node resource. If shutdown it
// applies a shutdown taint to the node.
//
//nolint:funlen,cyclop // copied from upstream
func (c *CloudNodeLifecycleController) MonitorNodes(ctx context.Context) {
	nodes, err := c.nodeLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("error listing nodes from cache: %s", err)

		return
	}

	for _, node := range nodes {
		// Default NodeReady status to v1.ConditionUnknown
		status := v1.ConditionUnknown
		if _, c := nodeutil.GetNodeCondition(&node.Status, v1.NodeReady); c != nil {
			status = c.Status
		}

		if status == v1.ConditionTrue {
			// if taint exist remove taint
			err = cloudnodeutil.RemoveTaintOffNode(c.kubeClient, node.Name, node, ShutdownTaint)
			if err != nil {
				klog.Errorf("error patching node taints: %v", err)
			}

			continue
		}

		// At this point the node has NotReady status, we need to check if the node has been removed
		// from the cloud provider. If node cannot be found in cloudprovider, then delete the node
		exists, err := c.ensureNodeExistsByProviderID(ctx, node)
		if err != nil {
			klog.Errorf("error checking if node %s exists: %v", node.Name, err)

			continue
		}

		//nolint:nestif // not that complex
		if !exists {
			// Current node does not exist, we should delete it, its taints do not matter anymore

			klog.V(2).Infof("deleting node since it is no longer present in cloud provider: %s", node.Name)

			ref := &v1.ObjectReference{
				Kind:      "Node",
				Name:      node.Name,
				UID:       node.UID,
				Namespace: "",
			}

			c.recorder.Eventf(ref, v1.EventTypeNormal, deleteNodeEvent,
				"Deleting node %s because it does not exist in the cloud provider", node.Name)

			err := CleanUpVolumeAttachmentsForNode(ctx, c.kubeClient, node.Name)
			if err != nil {
				klog.Errorf("failed to clean up volume attachments for node %s: %v", node.Name, err)
			}

			if err := c.kubeClient.CoreV1().Nodes().Delete(ctx, node.Name, metav1.DeleteOptions{}); err != nil {
				klog.Errorf("unable to delete node %q: %v", node.Name, err)
			}
		} else {
			// Node exists. We need to check this to get taint working in similar in all cloudproviders
			// current problem is that shutdown nodes are not working in similar way ie. all cloudproviders
			// does not delete node from kubernetes cluster when instance it is shutdown see issue #46442
			shutdown, err := c.shutdownInCloudProvider(ctx, node)
			if err != nil {
				klog.Errorf("error checking if node %s is shutdown: %v", node.Name, err)
			}

			if shutdown && err == nil {
				// if node is shutdown add shutdown taint
				err = cloudnodeutil.AddOrUpdateTaintOnNode(c.kubeClient, node.Name, ShutdownTaint)
				if err != nil {
					klog.Errorf("failed to apply shutdown taint to node %s, it may have been deleted.", node.Name)
				}
			}
		}
	}
}

// getProviderID returns the provider ID for the node. If Node CR has no provider ID,
// it will be the one from the cloud provider.
func (c *CloudNodeLifecycleController) getProviderID(ctx context.Context, node *v1.Node) (string, error) {
	if node.Spec.ProviderID != "" {
		return node.Spec.ProviderID, nil
	}

	if instanceV2, ok := c.cloud.InstancesV2(); ok {
		metadata, err := instanceV2.InstanceMetadata(ctx, node)
		if err != nil {
			return "", fmt.Errorf("failed to get instance metadata: %w", err)
		}

		return metadata.ProviderID, nil
	}

	providerID, err := cloudprovider.GetInstanceProviderID(ctx, c.cloud, types.NodeName(node.Name))
	if err != nil {
		return "", fmt.Errorf("failed to get instance provider id: %w", err)
	}

	return providerID, nil
}

// shutdownInCloudProvider returns true if the node is shutdown on the cloud provider.
func (c *CloudNodeLifecycleController) shutdownInCloudProvider(ctx context.Context, node *v1.Node) (bool, error) {
	if instanceV2, ok := c.cloud.InstancesV2(); ok {
		shutDown, err := instanceV2.InstanceShutdown(ctx, node)

		return shutDown, fmt.Errorf("failed to get instance shutdown status: %w", err)
	}

	instances, ok := c.cloud.Instances()
	if !ok {
		return false, errors.New("cloud provider does not support instances")
	}

	providerID, err := c.getProviderID(ctx, node)
	if err != nil {
		if errors.Is(err, cloudprovider.InstanceNotFound) {
			return false, nil
		}

		return false, fmt.Errorf("failed to get instance provider id: %w", err)
	}

	shutdown, err := instances.InstanceShutdownByProviderID(ctx, providerID)
	if err != nil {
		if errors.Is(err, cloudprovider.NotImplemented) {
			return false, nil
		}

		return false, fmt.Errorf("failed to get instance shutdown status: %w", err)
	}

	return shutdown, nil
}

// ensureNodeExistsByProviderID checks if the instance exists by the provider id,.
func (c *CloudNodeLifecycleController) ensureNodeExistsByProviderID(ctx context.Context, node *v1.Node) (bool, error) {
	if instanceV2, ok := c.cloud.InstancesV2(); ok {
		exists, err := instanceV2.InstanceExists(ctx, node)

		return exists, fmt.Errorf("failed to get instance existence: %w", err)
	}

	instances, ok := c.cloud.Instances()
	if !ok {
		return false, errors.New("instances interface not supported in the cloud provider")
	}

	providerID, err := c.getProviderID(ctx, node)
	if err != nil {
		if errors.Is(err, cloudprovider.InstanceNotFound) {
			return false, nil
		}

		return false, fmt.Errorf("failed to get instance provider id: %w", err)
	}

	exists, err := instances.InstanceExistsByProviderID(ctx, providerID)
	if err != nil {
		return exists, fmt.Errorf("failed to get instance existence by provider ID: %w", err)
	}

	return exists, nil
}
