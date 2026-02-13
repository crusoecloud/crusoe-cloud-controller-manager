package node

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	coreinformers "k8s.io/client-go/informers/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	v1lister "k8s.io/client-go/listers/core/v1"
	cloudnodeutil "k8s.io/cloud-provider/node/helpers"
	controllersmetrics "k8s.io/component-base/metrics/prometheus/controllers"
	"k8s.io/klog/v2"

	"github.com/crusoecloud/crusoe-cloud-controller-manager/internal/client"
)

const (
	TaintLabelPrefix       = "crusoe.ai/taint."
	ManagedTaintKeyPrefix  = "crusoe.ai/"
	InstanceGroupIDLabel   = "crusoe.ai/instance.group.id"
	NodeTaintControllerName = "node-taint-controller"
)

// NodeTaintController applies taints to Kubernetes nodes based on nodepool labels
// from the Crusoe Cloud API.
type NodeTaintController struct {
	kubeClient clientset.Interface
	nodeLister v1lister.NodeLister
	apiClient  client.APIClient
	clusterID  string
	syncPeriod time.Duration

	// Cache of instance group ID -> desired taints, refreshed each sync.
	nodepoolTaints sync.Map
}

// NewNodeTaintController creates a new NodeTaintController.
func NewNodeTaintController(
	nodeInformer coreinformers.NodeInformer,
	kubeClient clientset.Interface,
	apiClient client.APIClient,
	clusterID string,
	syncPeriod time.Duration,
) (*NodeTaintController, error) {
	if kubeClient == nil {
		return nil, ErrNilKubernetesClient
	}
	if apiClient == nil {
		return nil, fmt.Errorf("API client is nil")
	}
	if clusterID == "" {
		return nil, fmt.Errorf("cluster ID is empty")
	}

	return &NodeTaintController{
		kubeClient: kubeClient,
		nodeLister: nodeInformer.Lister(),
		apiClient:  apiClient,
		clusterID:  clusterID,
		syncPeriod: syncPeriod,
	}, nil
}

// Run starts the main loop for this controller.
func (c *NodeTaintController) Run(ctx context.Context,
	controllerManagerMetrics *controllersmetrics.ControllerManagerMetrics,
) {
	defer utilruntime.HandleCrash()
	controllerManagerMetrics.ControllerStarted(NodeTaintControllerName)
	defer controllerManagerMetrics.ControllerStopped(NodeTaintControllerName)

	klog.Info("Starting NodeTaintController")

	// Run the sync loop periodically
	wait.UntilWithContext(ctx, c.syncTaints, c.syncPeriod)
}

// syncTaints fetches nodepools, parses taint labels, and applies taints to nodes.
func (c *NodeTaintController) syncTaints(ctx context.Context) {
	// Step 1: Fetch all nodepools for this cluster
	nodepools, err := c.apiClient.ListNodePools(ctx, c.clusterID)
	if err != nil {
		klog.Errorf("Failed to list node pools: %v", err)
		return
	}

	// Step 2: Parse node_labels with crusoe.ai/taint. prefix into taints and cache them
	for _, np := range nodepools {
		taints := parseTaintsFromLabels(np.NodeLabels)
		c.nodepoolTaints.Store(np.InstanceGroupID, taints)
		klog.V(4).Infof("Cached %d taints for nodepool %s (instance group %s)",
			len(taints), np.Name, np.InstanceGroupID)
	}

	// Step 3: List all Kubernetes nodes
	nodes, err := c.nodeLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("Failed to list nodes: %v", err)
		return
	}

	// Step 4: For each node, apply taints based on its nodepool
	for _, node := range nodes {
		if err := c.reconcileNodeTaints(ctx, node); err != nil {
			klog.Errorf("Failed to reconcile taints for node %s: %v", node.Name, err)
		}
	}
}

// reconcileNodeTaints ensures the node has the correct taints based on its nodepool.
func (c *NodeTaintController) reconcileNodeTaints(ctx context.Context, node *v1.Node) error {
	instanceGroupID := node.Labels[InstanceGroupIDLabel]
	if instanceGroupID == "" {
		klog.V(4).Infof("Node %s has no instance group ID label, skipping", node.Name)
		return nil
	}

	// Get desired taints for this nodepool
	var desiredTaints []v1.Taint
	if cached, ok := c.nodepoolTaints.Load(instanceGroupID); ok {
		desiredTaints = cached.([]v1.Taint)
	}

	// Get current managed taints on the node
	currentManagedTaints := getManagedTaints(node.Spec.Taints)

	// Calculate taints to add and remove
	taintsToAdd, taintsToRemove := diffTaints(currentManagedTaints, desiredTaints)

	if len(taintsToAdd) == 0 && len(taintsToRemove) == 0 {
		klog.V(5).Infof("Node %s taints are up to date", node.Name)
		return nil
	}

	// Apply taint changes
	for _, taint := range taintsToRemove {
		taintCopy := taint
		if err := cloudnodeutil.RemoveTaintOffNode(c.kubeClient, node.Name, node, &taintCopy); err != nil {
			klog.Errorf("Failed to remove taint %s from node %s: %v", taint.Key, node.Name, err)
		} else {
			klog.V(2).Infof("Removed taint %s from node %s", taint.Key, node.Name)
		}
	}

	for _, taint := range taintsToAdd {
		taintCopy := taint
		if err := cloudnodeutil.AddOrUpdateTaintOnNode(c.kubeClient, node.Name, &taintCopy); err != nil {
			klog.Errorf("Failed to add taint %s to node %s: %v", taint.Key, node.Name, err)
		} else {
			klog.V(2).Infof("Added taint %s=%s:%s to node %s",
				taint.Key, taint.Value, taint.Effect, node.Name)
		}
	}

	return nil
}

// parseTaintsFromLabels extracts taints from nodepool labels with the crusoe.ai/taint. prefix.
// Supports two formats:
//   - "crusoe.ai/taint.key": "Effect" -> taint key=key, effect=Effect
//   - "crusoe.ai/taint.key": "value:Effect" -> taint key=key, value=value, effect=Effect
func parseTaintsFromLabels(nodeLabels map[string]string) []v1.Taint {
	var taints []v1.Taint

	for labelKey, labelValue := range nodeLabels {
		if !strings.HasPrefix(labelKey, TaintLabelPrefix) {
			continue
		}

		taintKey := strings.TrimPrefix(labelKey, TaintLabelPrefix)
		if taintKey == "" {
			klog.Warningf("Empty taint key in label %s", labelKey)
			continue
		}

		// Prefix the taint key with crusoe.ai/ so we can identify managed taints
		fullTaintKey := ManagedTaintKeyPrefix + taintKey

		var taintValue string
		var taintEffect v1.TaintEffect

		// Check if value contains ":" for value:effect format
		if strings.Contains(labelValue, ":") {
			parts := strings.SplitN(labelValue, ":", 2)
			taintValue = parts[0]
			taintEffect = parseTaintEffect(parts[1])
		} else {
			// Simple format: just the effect
			taintEffect = parseTaintEffect(labelValue)
		}

		if taintEffect == "" {
			klog.Warningf("Invalid taint effect in label %s=%s", labelKey, labelValue)
			continue
		}

		taints = append(taints, v1.Taint{
			Key:    fullTaintKey,
			Value:  taintValue,
			Effect: taintEffect,
		})
	}

	return taints
}

// parseTaintEffect converts a string to a TaintEffect.
func parseTaintEffect(effect string) v1.TaintEffect {
	switch effect {
	case string(v1.TaintEffectNoSchedule):
		return v1.TaintEffectNoSchedule
	case string(v1.TaintEffectPreferNoSchedule):
		return v1.TaintEffectPreferNoSchedule
	case string(v1.TaintEffectNoExecute):
		return v1.TaintEffectNoExecute
	default:
		return ""
	}
}

// getManagedTaints returns taints that are managed by this controller (crusoe.ai/* prefix).
// Excludes the shutdown taint which is managed by CloudNodeLifecycleController.
func getManagedTaints(taints []v1.Taint) []v1.Taint {
	var managed []v1.Taint
	for _, t := range taints {
		if strings.HasPrefix(t.Key, ManagedTaintKeyPrefix) && t.Key != ShutdownTaint.Key {
			managed = append(managed, t)
		}
	}
	return managed
}

// diffTaints computes which taints need to be added and removed.
func diffTaints(current, desired []v1.Taint) (toAdd, toRemove []v1.Taint) {
	currentMap := make(map[string]v1.Taint)
	for _, t := range current {
		currentMap[t.Key] = t
	}

	desiredMap := make(map[string]v1.Taint)
	for _, t := range desired {
		desiredMap[t.Key] = t
	}

	// Find taints to add or update
	for key, desired := range desiredMap {
		if current, exists := currentMap[key]; !exists {
			toAdd = append(toAdd, desired)
		} else if current.Value != desired.Value || current.Effect != desired.Effect {
			// Taint exists but with different value/effect - remove old, add new
			toRemove = append(toRemove, current)
			toAdd = append(toAdd, desired)
		}
	}

	// Find taints to remove (in current but not in desired)
	for key, current := range currentMap {
		if _, exists := desiredMap[key]; !exists {
			toRemove = append(toRemove, current)
		}
	}

	return toAdd, toRemove
}
