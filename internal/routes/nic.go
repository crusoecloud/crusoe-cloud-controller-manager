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

var (
	// ErrNoNetworkInterfaces indicates the resolved instance has no NICs.
	ErrNoNetworkInterfaces = errors.New("instance has no network interfaces")
	// ErrNoNICInVPC indicates none of the instance's NICs is in the configured
	// VPC — a mis-wired deployment (wrong CRUSOE_VPC_ID or wrong network).
	ErrNoNICInVPC = errors.New("no network interface in configured VPC")
)

// resolveNIC returns the network_interface_id for nodeName, caching it in
// nodeState.nicID (immutable per instance). Resolution order:
//  1. Node.Spec.ProviderID ("crusoe://<uuid>") -> GetInstanceByID
//  2. Node.Status.NodeInfo.SystemUUID -> GetInstanceByID
//  3. GetInstanceByName(nodeName)
//
// The NIC whose Network == cfg.VPCID is chosen; if none matches, resolution
// fails (retryable) — the SDN contract requires the NIC to be in the
// allocation's vpc+location, so any other pick can only fail downstream with a
// less actionable error. Node may be nil (not yet registered), which skips
// steps 1-2.
//
// Each successful resolve also sanity-checks the instance's project/location
// against the startup-resolved config, warning on mismatch (§5.1).
func (c *RouteController) resolveNIC(ctx context.Context, nodeName string, node *v1.Node) (string, error) {
	if st := c.getState(nodeName); st != nil && st.nicID != "" {
		return st.nicID, nil
	}

	inst, err := c.resolveInstance(ctx, nodeName, node)
	if err != nil {
		return "", fmt.Errorf("failed to resolve NIC for node %s: %w", nodeName, err)
	}

	c.warnMetadataMismatch(inst)

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

// selectNICID picks the NIC whose Network == vpcID; a miss is a hard (but
// retryable) error naming the VPC and the NICs found.
func selectNICID(inst *crusoeapi.InstanceV1Alpha5, vpcID string) (string, error) {
	if len(inst.NetworkInterfaces) == 0 {
		return "", ErrNoNetworkInterfaces
	}
	nics := make([]string, 0, len(inst.NetworkInterfaces))
	for i := range inst.NetworkInterfaces {
		if inst.NetworkInterfaces[i].Network == vpcID {
			return inst.NetworkInterfaces[i].Id, nil
		}
		nics = append(nics, fmt.Sprintf("%s (network %s)",
			inst.NetworkInterfaces[i].Id, inst.NetworkInterfaces[i].Network))
	}

	return "", fmt.Errorf("%w %s on instance %s (check CRUSOE_VPC_ID; have: %s)",
		ErrNoNICInVPC, vpcID, inst.Id, strings.Join(nics, ", "))
}

// warnMetadataMismatch warns when instance metadata disagrees with the
// startup-resolved config. cfg is immutable once the controller starts
// (location resolves fail-fast at startup, §5.1), so no lock is needed: this
// is a read-only sanity check, and a mismatch means a mis-rendered
// --cluster-name or CRUSOE_PROJECT_ID.
func (c *RouteController) warnMetadataMismatch(inst *crusoeapi.InstanceV1Alpha5) {
	if inst.ProjectId != "" && inst.ProjectId != c.cfg.ProjectID {
		klog.Warningf("instance project id %s differs from CRUSOE_PROJECT_ID %s (check deployment env)",
			inst.ProjectId, c.cfg.ProjectID)
	}
	if inst.Location != "" && inst.Location != c.cfg.Location {
		klog.Warningf("instance location %s differs from cluster location %s (check --cluster-name rendering)",
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
