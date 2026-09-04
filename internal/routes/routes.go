package routes

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/crusoecloud/crusoe-cloud-controller-manager/internal/client"
	"github.com/crusoecloud/crusoe-cloud-controller-manager/internal/routes/sdn"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	v1lister "k8s.io/client-go/listers/core/v1"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/klog/v2"
)

const (
	createPollInterval = 5 * time.Second
	createPollTimeout  = 2 * time.Minute // CreateRoute's self-imposed deadline (section 6.2)
)

// cachedNIC records a node's resolved instance + NIC. The instance id lets a
// lookup detect a node deleted and recreated under the same name with a new VM
// (CloudRoutes has no delete signal, unlike the old CiliumNode-delete handler).
type cachedNIC struct {
	instanceID string
	nicID      string
}

// CloudRoutes implements cloudprovider.Routes over the SDN pod CIDR allocation
// seam. The upstream node-route-controller is the sole consumer: it serializes
// passes but fans Create/Delete out on goroutines, so every method is
// goroutine-safe.
type CloudRoutes struct {
	cfg        *Config
	sdn        sdn.PodCIDRAllocationClient
	apiClient  client.APIClient
	nodeLister v1lister.NodeLister // injected via SetNodeLister before controllers start (section 8)

	mu           sync.Mutex
	nicByNode    map[string]cachedNIC       // nodeName -> {instanceID, nicID}; section 9
	reservations []sdn.VPCPrefixReservation // configured reservations, fetched once (immutable)
}

var _ cloudprovider.Routes = (*CloudRoutes)(nil)

// NewCloudRoutes builds a CloudRoutes. The node lister is injected later via
// SetNodeLister (section 8), before the first ListRoutes.
func NewCloudRoutes(cfg *Config, sdnClient sdn.PodCIDRAllocationClient, apiClient client.APIClient) *CloudRoutes {
	return &CloudRoutes{
		cfg:       cfg,
		sdn:       sdnClient,
		apiClient: apiClient,
		nicByNode: make(map[string]cachedNIC),
	}
}

// SetNodeLister injects the shared Node lister (section 8).
func (r *CloudRoutes) SetNodeLister(l v1lister.NodeLister) {
	r.nodeLister = l
}

// ListRoutes builds the current allocation set as cloudprovider.Routes. It maps
// each allocation to its owning node via NIC resolution (section 6.1). clusterName is
// ignored, scoping is by reservation ids (List) and --cluster-cidr containment
// (upstream's isResponsibleForRoute).
//
// A node whose NIC cannot be resolved does not fail the pass: its allocation is
// attributed by pod cidr instead, which protects it from orphan GC without an
// API call. The NIC check that matters happens in CreateRoute.
func (r *CloudRoutes) ListRoutes(ctx context.Context, _ string) ([]*cloudprovider.Route, error) {
	nodes, err := r.nodeLister.List(labels.Everything())
	if err != nil {
		return nil, fmt.Errorf("failed to list nodes: %w", err)
	}
	r.evictDeletedNodes(nodes)

	nicToNode := make(map[string]string, len(nodes))
	cidrToNode := make(map[string]string)
	for _, node := range nodes {
		nicID, resolveErr := r.resolveNIC(ctx, node.Name, node)
		if resolveErr != nil {
			klog.ErrorS(resolveErr, "resolving NIC for node, attributing its allocations by pod cidr",
				"node", node.Name)
			for _, cidr := range node.Spec.PodCIDRs {
				cidrToNode[cidr] = node.Name
			}

			continue
		}
		nicToNode[nicID] = node.Name
	}

	allocs, err := r.sdn.ListPodCIDRAllocations(ctx, sdn.ListPodCIDRAllocationsQuery{
		ProjectID:               r.cfg.ProjectID, // the server requires project + vpc on every List
		VPCNetworkID:            r.cfg.VPCID,
		VPCPrefixReservationIDs: r.cfg.VPCPrefixReservationIDs,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list pod cidr allocations: %w", err)
	}

	routes := make([]*cloudprovider.Route, 0, len(allocs))
	for i := range allocs {
		// An allocation whose NIC maps to no live node gets TargetNode "" ->
		// upstream deletes it (orphan GC), unless a node we could not resolve
		// claims its pod cidr.
		target := nicToNode[allocs[i].NetworkInterfaceID]
		if target == "" {
			target = cidrToNode[allocs[i].DestinationCIDR]
		}
		routes = append(routes, &cloudprovider.Route{
			Name:            allocs[i].ID,
			TargetNode:      types.NodeName(target),
			DestinationCIDR: allocs[i].DestinationCIDR,
		})
	}

	return routes, nil
}

// evictDeletedNodes drops nicByNode entries for node names the lister no longer
// returns, so a deleted node's NIC does not linger in the cache.
func (r *CloudRoutes) evictDeletedNodes(nodes []*v1.Node) {
	live := make(map[string]struct{}, len(nodes))
	for _, node := range nodes {
		live[node.Name] = struct{}{}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	for name := range r.nicByNode {
		if _, ok := live[name]; !ok {
			delete(r.nicByNode, name)
		}
	}
}

// CreateRoute creates (or adopts) the allocation backing route.TargetNode's pod
// cidr, synchronously polling the create operation to a terminal state within
// createPollTimeout (section 6.2). nameHint and clusterName are ignored.
//
// It lists before creating: an allocation already held by this node's NIC is
// adopted without touching the SDN write path, so a route that was programmed
// once is never re-created no matter what drives the pass (the SDN create is
// idempotent, but the List is the cheaper and safer guard).
func (r *CloudRoutes) CreateRoute(ctx context.Context, _, _ string, route *cloudprovider.Route) error {
	nodeName := string(route.TargetNode)

	node, err := r.nodeLister.Get(nodeName)
	if err != nil {
		// Upstream built the route from this same lister, so NotFound is a
		// transient race, retry next pass.
		return fmt.Errorf("failed to get node %s: %w", nodeName, err)
	}

	nicID, err := r.resolveNIC(ctx, nodeName, node)
	if err != nil {
		return fmt.Errorf("resolving NIC for node %s: %w", nodeName, err)
	}

	cidr := route.DestinationCIDR

	// List-before-create / adopt (section 6.2 step 3).
	alloc, found, err := r.lookupAllocation(ctx, cidr)
	if err != nil {
		return err
	}
	if found {
		if alloc.NetworkInterfaceID == nicID {
			klog.InfoS("adopted existing pod cidr allocation", "node", nodeName,
				"cidr", cidr, "allocationID", alloc.ID, "nicID", nicID)

			return nil
		}

		return fmt.Errorf("pod cidr %s for node %s allocated to another interface: %w",
			cidr, nodeName, sdn.ErrDestinationConflict)
	}

	rsvID, err := r.reservationForCIDR(ctx, cidr)
	if err != nil {
		return err
	}

	op, err := r.sdn.CreatePodCIDRAllocations(ctx, sdn.CreatePodCIDRAllocationsRequest{
		Allocations: []sdn.PodCIDRAllocationSpec{
			{VPCPrefixReservationID: rsvID, NetworkInterfaceID: nicID, DestinationCIDR: cidr},
		},
		Context: r.allocationContext(),
	})
	if err != nil {
		return fmt.Errorf("creating pod cidr allocation for node %s (cidr %s): %w", nodeName, cidr, err)
	}

	klog.InfoS("created pod cidr allocation, polling to terminal", "node", nodeName,
		"cidr", cidr, "nicID", nicID, "operationID", op.OperationID)

	return r.pollCreateOp(ctx, nodeName, cidr, op.OperationID)
}

// pollCreateOp waits for op to reach a terminal state within createPollTimeout
// (section 6.2 step 6). A surviving row from a FAILED/timed-out op is adopted by the
// next pass's List-before-create.
func (r *CloudRoutes) pollCreateOp(ctx context.Context, nodeName, cidr, opID string) error {
	var termErr error
	waitErr := wait.PollUntilContextTimeout(ctx, createPollInterval, createPollTimeout, true,
		func(ctx context.Context) (bool, error) {
			ops, err := r.sdn.ListPodCIDRAllocationOperations(ctx, sdn.ListPodCIDRAllocationOperationsQuery{
				OperationIDs: []string{opID},
			})
			if err != nil {
				return false, fmt.Errorf("listing operation %s: %w", opID, err)
			}
			for i := range ops {
				if ops[i].OperationID != opID {
					continue
				}
				switch ops[i].State {
				case sdn.OperationStateSucceeded:
					return true, nil
				case sdn.OperationStateFailed:
					termErr = fmt.Errorf("pod cidr allocation operation %s failed for node %s: %s",
						opID, nodeName, ops[i].Error)

					return false, termErr
				case sdn.OperationStateInProgress:
					return false, nil
				}
			}
			// Op not returned yet (retention gap), keep polling.
			return false, nil
		})
	if termErr != nil {
		return termErr
	}
	if waitErr != nil {
		return fmt.Errorf("waiting for pod cidr allocation operation %s (node %s, cidr %s): %w",
			opID, nodeName, cidr, waitErr)
	}

	klog.InfoS("pod cidr allocation ready", "node", nodeName, "cidr", cidr, "operationID", opID)

	return nil
}

// DeleteRoute deletes the allocation named by route.Name (section 6.3). The server is
// the kill switch: DeletePodCIDRAllocations is Unimplemented server-side, so the
// error surfaces here until the server lands it.
func (r *CloudRoutes) DeleteRoute(ctx context.Context, _ string, route *cloudprovider.Route) error {
	_, err := r.sdn.DeletePodCIDRAllocations(ctx, sdn.DeletePodCIDRAllocationsRequest{
		IDs:     []string{route.Name},
		Context: r.allocationContext(),
	})
	if err != nil {
		return fmt.Errorf("deleting pod cidr allocation %s: %w", route.Name, err)
	}

	return nil
}

// lookupAllocation lists by destination cidr within the configured reservations
// (both bounding filters) and returns the single allocation if present.
func (r *CloudRoutes) lookupAllocation(ctx context.Context, cidr string) (sdn.PodCIDRAllocation, bool, error) {
	allocs, err := r.sdn.ListPodCIDRAllocations(ctx, sdn.ListPodCIDRAllocationsQuery{
		ProjectID:               r.cfg.ProjectID, // the server requires project + vpc on every List
		VPCNetworkID:            r.cfg.VPCID,
		DestinationCIDR:         cidr,
		VPCPrefixReservationIDs: r.cfg.VPCPrefixReservationIDs,
	})
	if err != nil {
		return sdn.PodCIDRAllocation{}, false, fmt.Errorf("failed to list allocation for cidr %s: %w", cidr, err)
	}
	if len(allocs) == 0 {
		return sdn.PodCIDRAllocation{}, false, nil
	}

	return allocs[0], true, nil
}

// allocationContext builds the RPC context from the immutable config.
func (r *CloudRoutes) allocationContext() sdn.PodCIDRAllocationContext {
	return sdn.PodCIDRAllocationContext{
		ProjectID:    r.cfg.ProjectID,
		VPCNetworkID: r.cfg.VPCID,
		Location:     r.cfg.SDNLocation, // the region coordinator accepts internal location names only
	}
}

// resolveNIC returns the network_interface_id for node, caching {instanceID,
// nicID} in nicByNode (section 9). A cached entry is reused only while the node's
// instance id still matches, a node recreated under the same name with a new
// VM re-resolves.
func (r *CloudRoutes) resolveNIC(ctx context.Context, nodeName string, node *v1.Node) (string, error) {
	instanceID := ""
	if node != nil {
		instanceID = instanceIDFromNode(node)
	}

	r.mu.Lock()
	cached, ok := r.nicByNode[nodeName]
	r.mu.Unlock()
	if ok && cached.nicID != "" && cached.instanceID == instanceID {
		return cached.nicID, nil
	}

	inst, err := resolveInstance(ctx, r.apiClient, nodeName, node)
	if err != nil {
		return "", fmt.Errorf("failed to resolve NIC for node %s: %w", nodeName, err)
	}

	warnMetadataMismatch(inst, r.cfg)

	nicID, err := selectNICID(inst, r.cfg.VPCID)
	if err != nil {
		return "", fmt.Errorf("failed to resolve NIC for node %s: %w", nodeName, err)
	}

	r.mu.Lock()
	r.nicByNode[nodeName] = cachedNIC{instanceID: instanceID, nicID: nicID}
	r.mu.Unlock()

	return nicID, nil
}
