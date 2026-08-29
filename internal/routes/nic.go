package routes

import (
	"context"
	"errors"
	"fmt"
	"strings"

	crusoeapi "github.com/crusoecloud/client-go/swagger/v1alpha5"
	"github.com/crusoecloud/crusoe-cloud-controller-manager/internal/instances"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// ErrNoNetworkInterfaces indicates the resolved instance has no NICs.
var ErrNoNetworkInterfaces = errors.New("instance has no network interfaces")

// resolveNIC returns the network_interface_id for nodeName, caching it in
// nodeState.nicID (immutable per instance). Resolution order:
//  1. Node.Spec.ProviderID ("crusoe://<uuid>") -> GetInstanceByID
//  2. Node.Status.NodeInfo.SystemUUID -> GetInstanceByID
//  3. GetInstanceByName(nodeName)
//
// The NIC whose Network == cfg.VPCID is chosen; if none matches, NICs[0] is used
// with a warning. Node may be nil (not yet registered), which skips steps 1-2.
//
// Side effect (§5.1): if cfg.Location is still empty the first successful resolve
// fills it from instance.Location (under c.mu); if non-empty a mismatch logs a
// warning. The context's project id is taken from instance.ProjectId
// (authoritative), warning if it differs from CRUSOE_PROJECT_ID.
func (c *RouteController) resolveNIC(ctx context.Context, nodeName string, node *v1.Node) (string, error) {
	if st := c.getState(nodeName); st != nil && st.nicID != "" {
		return st.nicID, nil
	}

	inst, err := c.resolveInstance(ctx, nodeName, node)
	if err != nil {
		return "", fmt.Errorf("failed to resolve NIC for node %s: %w", nodeName, err)
	}

	c.crossCheckProjectAndLocation(inst)

	nicID, err := selectNICID(inst, c.cfg.VPCID)
	if err != nil {
		return "", fmt.Errorf("failed to resolve NIC for node %s: %w", nodeName, err)
	}

	c.setNICID(nodeName, nicID)

	return nicID, nil
}

// resolveInstance fetches the instance backing nodeName using providerID, then
// SystemUUID, then name, mirroring internal/instances resolution.
func (c *RouteController) resolveInstance(ctx context.Context, nodeName string, node *v1.Node,
) (*crusoeapi.InstanceV1Alpha5, error) {
	if node != nil {
		if inst := c.instanceByID(ctx, nodeName, node); inst != nil {
			return inst, nil
		}
	}

	inst, err := c.apiClient.GetInstanceByName(ctx, nodeName)
	if err != nil {
		return nil, fmt.Errorf("failed to get instance by name %s: %w", nodeName, err)
	}

	return inst, nil
}

// instanceByID attempts the id-based lookup, returning nil (and logging) on any
// miss so the caller falls back to the name lookup.
func (c *RouteController) instanceByID(
	ctx context.Context, nodeName string, node *v1.Node,
) *crusoeapi.InstanceV1Alpha5 {
	id := instanceIDFromNode(node)
	if id == "" {
		return nil
	}

	inst, resp, err := c.apiClient.GetInstanceByID(ctx, id)
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		klog.V(4).InfoS("resolveNIC: GetInstanceByID failed, falling back to name",
			"node", nodeName, "instanceID", id, "err", err)

		return nil
	}

	return inst
}

// instanceIDFromNode returns the crusoe instance id derived from the Node,
// preferring providerID and falling back to SystemUUID (kept in sync by kubelet)
// when they disagree, matching internal/instances behavior.
func instanceIDFromNode(node *v1.Node) string {
	providerID := strings.TrimPrefix(node.Spec.ProviderID, instances.ProviderPrefix)
	sysUUID := node.Status.NodeInfo.SystemUUID
	if sysUUID != "" && providerID != sysUUID {
		return sysUUID
	}

	return providerID
}

// selectNICID picks the NIC whose Network == vpcID, else NICs[0] with a warning.
func selectNICID(inst *crusoeapi.InstanceV1Alpha5, vpcID string) (string, error) {
	if len(inst.NetworkInterfaces) == 0 {
		return "", ErrNoNetworkInterfaces
	}
	for i := range inst.NetworkInterfaces {
		if inst.NetworkInterfaces[i].Network == vpcID {
			return inst.NetworkInterfaces[i].Id, nil
		}
	}
	klog.Warningf("no NIC on instance %s matches VPC %s; using first NIC %s",
		inst.Id, vpcID, inst.NetworkInterfaces[0].Id)

	return inst.NetworkInterfaces[0].Id, nil
}

// crossCheckProjectAndLocation fills cfg.Location from the instance if it is
// still empty, and warns on any project/location disagreement.
func (c *RouteController) crossCheckProjectAndLocation(inst *crusoeapi.InstanceV1Alpha5) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if inst.ProjectId != "" && c.cfg.ProjectID != "" && inst.ProjectId != c.cfg.ProjectID {
		klog.Warningf("instance project id %s differs from CRUSOE_PROJECT_ID %s; using instance value",
			inst.ProjectId, c.cfg.ProjectID)
	}
	if inst.ProjectId != "" {
		c.cfg.ProjectID = inst.ProjectId
	}

	if c.cfg.Location == "" {
		c.cfg.Location = inst.Location

		return
	}
	if inst.Location != "" && inst.Location != c.cfg.Location {
		klog.Warningf("instance location %s differs from resolved location %s (check --cluster-name rendering)",
			inst.Location, c.cfg.Location)
	}
}

// getState returns the soft state for a node, or nil if absent.
func (c *RouteController) getState(nodeName string) *nodeState {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.state[nodeName]
}

// setNICID caches the resolved NIC id in the node's soft state.
func (c *RouteController) setNICID(nodeName, nicID string) {
	c.withState(nodeName, func(st *nodeState) { st.nicID = nicID })
}
