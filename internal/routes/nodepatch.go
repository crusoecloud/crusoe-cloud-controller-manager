package routes

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"time"

	v1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	nodeutil "k8s.io/component-helpers/node/util"
)

const (
	networkUnavailableReason  = "CrusoePodCIDRAllocated"
	networkUnavailableMessage = "crusoe-route-controller programmed SDN pod CIDR allocation"
)

// setNetworkAvailable sets NodeNetworkUnavailable=False via the status
// subresource, skipping the PATCH if the condition already holds. It must run
// before the final taint/label patch (§12 ordering invariant 2).
func (c *RouteController) setNetworkAvailable(nodeName string, node *v1.Node) error {
	if _, cond := nodeutil.GetNodeCondition(&node.Status, v1.NodeNetworkUnavailable); cond != nil &&
		cond.Status == v1.ConditionFalse {

		return nil
	}

	err := nodeutil.SetNodeCondition(c.kubeClient, types.NodeName(nodeName), v1.NodeCondition{
		Type:    v1.NodeNetworkUnavailable,
		Status:  v1.ConditionFalse,
		Reason:  networkUnavailableReason,
		Message: networkUnavailableMessage,
	})
	if err != nil {
		return fmt.Errorf("failed to set NetworkUnavailable=False on node %s: %w", nodeName, err)
	}

	return nil
}

// patchOpIDLabel writes the in-flight operation id to the OpIDLabel immediately
// after a create returns (durable before anything depends on it).
func (c *RouteController) patchOpIDLabel(ctx context.Context, node *v1.Node, opID string) error {
	newNode := node.DeepCopy()
	if newNode.Labels == nil {
		newNode.Labels = map[string]string{}
	}
	newNode.Labels[OpIDLabel] = opID

	return c.strategicPatch(ctx, node.Name, node, newNode)
}

// clearOpIDLabel removes the OpIDLabel (create op FAILED path, C6).
func (c *RouteController) clearOpIDLabel(ctx context.Context, node *v1.Node) error {
	if _, ok := node.Labels[OpIDLabel]; !ok {
		return nil
	}
	newNode := node.DeepCopy()
	delete(newNode.Labels, OpIDLabel)

	return c.strategicPatch(ctx, node.Name, node, newNode)
}

// finalizeNode applies ONE Node patch (taint removal LAST) that simultaneously
// (a) removes the pods-unroutable taint, (b) deletes OpIDLabel, and (c) sets
// ReadyAtLabel to Unix epoch seconds. The atomicity guarantees ready-at is
// recorded iff the taint actually came off (§12 invariant 3). It is a no-op if
// the node is already untainted with ready-at set.
func (c *RouteController) finalizeNode(ctx context.Context, node *v1.Node) error {
	newNode := node.DeepCopy()

	removeTaint(newNode, PodsUnroutableTaintKey)

	if newNode.Labels == nil {
		newNode.Labels = map[string]string{}
	}
	delete(newNode.Labels, OpIDLabel)
	if _, ok := newNode.Labels[ReadyAtLabel]; !ok {
		newNode.Labels[ReadyAtLabel] = strconv.FormatInt(time.Now().Unix(), 10)
	}

	if nodesEqual(node, newNode) {
		return nil
	}

	return c.strategicPatch(ctx, node.Name, node, newNode)
}

// removeTaint strips the taint with the given key from the node's spec.
func removeTaint(node *v1.Node, key string) {
	node.Spec.Taints = slices.DeleteFunc(node.Spec.Taints, func(t v1.Taint) bool { return t.Key == key })
}

// nodesEqual reports whether the labels and taints of two nodes are equivalent
// (the only fields finalizeNode changes). Taints use Semantic.DeepEqual because
// v1.Taint's TimeAdded is a *metav1.Time that DeepCopy clones to a fresh
// pointer, so a plain == / slices.Equal would misreport unchanged taints.
func nodesEqual(a, b *v1.Node) bool {
	return maps.Equal(a.Labels, b.Labels) && apiequality.Semantic.DeepEqual(a.Spec.Taints, b.Spec.Taints)
}

// strategicPatch computes and applies a two-way strategic merge patch old->new.
// RV is stripped from the base so the patch does not carry a conflict check over
// .spec.taints (mirrors cloud-provider's PatchNodeTaints).
func (c *RouteController) strategicPatch(ctx context.Context, nodeName string, oldNode, newNode *v1.Node) error {
	oldNoRV := oldNode.DeepCopy()
	oldNoRV.ResourceVersion = ""
	oldData, err := json.Marshal(oldNoRV)
	if err != nil {
		return fmt.Errorf("failed to marshal old node %s: %w", nodeName, err)
	}

	newClone := oldNode.DeepCopy()
	newClone.ResourceVersion = ""
	newClone.Labels = newNode.Labels
	newClone.Spec.Taints = newNode.Spec.Taints
	newData, err := json.Marshal(newClone)
	if err != nil {
		return fmt.Errorf("failed to marshal new node %s: %w", nodeName, err)
	}

	patchBytes, err := strategicpatch.CreateTwoWayMergePatch(oldData, newData, v1.Node{})
	if err != nil {
		return fmt.Errorf("failed to create patch for node %s: %w", nodeName, err)
	}

	_, err = c.kubeClient.CoreV1().Nodes().Patch(
		ctx, nodeName, types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("failed to patch node %s: %w", nodeName, err)
	}

	return nil
}
