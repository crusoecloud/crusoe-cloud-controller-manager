package routes

import (
	"context"
	"time"

	"github.com/crusoecloud/crusoe-cloud-controller-manager/internal/routes/sdn"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"
)

// maxDeleteChunk is the contract cap on delete ids per DeletePodCIDRAllocations
// call.
const maxDeleteChunk = 50

const reasonOrphanedAllocationDeleted = "OrphanedAllocationDeleted"

// reapOnce is one pass of the periodic desired-vs-actual sweep. It is the ONLY
// caller of DeletePodCIDRAllocations. Deletes are grace-gated to protect racing
// joins and in-flight VM deletes; missing/mismatched nodes are enqueued through
// the normal reconcile path so the reaper never races the state machine on
// creates.
func (c *RouteController) reapOnce(ctx context.Context) {
	if c.locationEmpty() {
		klog.Warning("reaper: skipping pass, location not yet resolved")

		return
	}

	desired := c.buildDesired(ctx)

	actual, err := c.sdn.ListPodCIDRAllocations(ctx, sdn.ListPodCIDRAllocationsQuery{
		VPCPrefixReservationIDs: []string{c.cfg.VPCPrefixReservationID},
	})
	if err != nil {
		klog.ErrorS(err, "reaper: list allocations failed; aborting pass")

		return
	}

	metricAllocationsDesired.Set(float64(len(desired)))
	metricAllocationsActual.Set(float64(len(actual)))

	c.deleteOrphans(ctx, desired, actual)
	c.enqueueMissing(desired, actual)
}

// buildDesired maps destination_cidr -> nicID for every live CiliumNode with a
// routed podCIDR. On a state-cache miss it resolves the NIC; a node whose NIC
// cannot be resolved is skipped (never guessed).
func (c *RouteController) buildDesired(ctx context.Context) map[string]string {
	desired := make(map[string]string)

	objs, err := c.ciliumNodeLister.List(labels.Everything())
	if err != nil {
		klog.ErrorS(err, "reaper: list CiliumNodes failed")

		return desired
	}

	for i := range objs {
		u, ok := asUnstructured(objs[i])
		if !ok {
			continue
		}
		cn, convErr := ciliumNodeFromUnstructured(u)
		if convErr != nil {
			klog.ErrorS(convErr, "reaper: CiliumNode conversion failed")

			continue
		}
		if cn.DeletionTimestamp != nil || len(cn.PodCIDRs) == 0 {
			continue
		}

		nicID := c.desiredNICID(ctx, cn.Name)
		if nicID == "" {
			klog.Warningf("reaper: skipping node %s, NIC unresolved", cn.Name)

			continue
		}
		desired[cn.PodCIDRs[0]] = nicID
	}

	return desired
}

// desiredNICID returns the cached NIC id for a node, resolving it on a miss.
func (c *RouteController) desiredNICID(ctx context.Context, nodeName string) string {
	if st := c.getState(nodeName); st != nil && st.nicID != "" {
		return st.nicID
	}

	nicID, err := c.resolveNIC(ctx, nodeName, c.getNode(nodeName))
	if err != nil {
		return ""
	}

	return nicID
}

// deleteOrphans deletes allocations that are orphaned (destination not desired)
// or NIC-mismatched (node replaced), skipping any within the grace period. It
// batches ids into chunks of at most maxDeleteChunk. For each deleted allocation
// whose cidr maps to an existing Node it emits an event; otherwise klog only.
func (c *RouteController) deleteOrphans(
	ctx context.Context, desired map[string]string, actual []sdn.PodCIDRAllocation,
) {
	now := time.Now()
	candidates := make([]sdn.PodCIDRAllocation, 0, len(actual))
	for i := range actual {
		a := actual[i]
		wantNIC, ok := desired[a.DestinationCIDR]
		if ok && wantNIC == a.NetworkInterfaceID {
			continue // matches desired
		}
		if now.Sub(a.CreatedAt) < c.cfg.ReaperGrace {
			continue // too young; protect racing joins / in-flight VM deletes
		}
		candidates = append(candidates, a)
	}

	if len(candidates) == 0 {
		return
	}

	c.deleteInChunks(ctx, candidates)
}

// deleteInChunks batch-deletes candidate allocations in chunks of at most
// maxDeleteChunk. A chunk-level NotFound aborts remaining chunks (next pass
// re-lists).
func (c *RouteController) deleteInChunks(ctx context.Context, candidates []sdn.PodCIDRAllocation) {
	for start := 0; start < len(candidates); start += maxDeleteChunk {
		end := start + maxDeleteChunk
		if end > len(candidates) {
			end = len(candidates)
		}
		chunk := candidates[start:end]

		ids := make([]string, 0, len(chunk))
		for i := range chunk {
			ids = append(ids, chunk[i].ID)
		}

		_, err := c.sdn.DeletePodCIDRAllocations(ctx, sdn.DeletePodCIDRAllocationsRequest{
			IDs: ids,
			Context: sdn.PodCIDRAllocationContext{
				ProjectID: c.cfg.ProjectID, VPCNetworkID: c.cfg.VPCID, Location: c.cfg.Location,
			},
		})
		if err != nil {
			klog.ErrorS(err, "reaper: delete chunk failed; aborting remaining chunks", "ids", ids)

			return
		}

		for i := range chunk {
			c.reportDeletedAllocation(&chunk[i])
		}
	}
}

// reportDeletedAllocation emits an event on the owning Node when one exists
// (NIC-mismatch case), else structured klog only.
func (c *RouteController) reportDeletedAllocation(a *sdn.PodCIDRAllocation) {
	node := c.nodeForCIDR(a.DestinationCIDR)
	if node != nil {
		c.eventf(node, v1.EventTypeNormal, reasonOrphanedAllocationDeleted,
			"deleted stale pod CIDR allocation %s (cidr %s) held by another interface", a.ID, a.DestinationCIDR)

		return
	}
	klog.InfoS("reaper: deleted orphaned allocation",
		"allocationID", a.ID, "cidr", a.DestinationCIDR, "nicID", a.NetworkInterfaceID)
}

// nodeForCIDR returns the Node whose CiliumNode routes the given cidr, or nil.
func (c *RouteController) nodeForCIDR(cidr string) *v1.Node {
	objs, err := c.ciliumNodeLister.List(labels.Everything())
	if err != nil {
		return nil
	}
	for i := range objs {
		u, ok := asUnstructured(objs[i])
		if !ok {
			continue
		}
		cn, convErr := ciliumNodeFromUnstructured(u)
		if convErr != nil || len(cn.PodCIDRs) == 0 || cn.PodCIDRs[0] != cidr {
			continue
		}

		return c.getNode(cn.Name)
	}

	return nil
}

// enqueueMissing enqueues nodes whose desired cidr has no actual allocation (or
// whose actual allocation had a NIC mismatch that was just deleted), so creation
// flows through the single reconcile path.
func (c *RouteController) enqueueMissing(desired map[string]string, actual []sdn.PodCIDRAllocation) {
	present := make(map[string]string, len(actual))
	for i := range actual {
		present[actual[i].DestinationCIDR] = actual[i].NetworkInterfaceID
	}

	for cidr, wantNIC := range desired {
		haveNIC, ok := present[cidr]
		if ok && haveNIC == wantNIC {
			continue
		}
		if name := c.nodeNameForCIDR(cidr); name != "" {
			c.enqueue(name)
		}
	}
}

// nodeNameForCIDR returns the CiliumNode name routing the given cidr, or "".
func (c *RouteController) nodeNameForCIDR(cidr string) string {
	objs, err := c.ciliumNodeLister.List(labels.Everything())
	if err != nil {
		return ""
	}
	for i := range objs {
		u, ok := asUnstructured(objs[i])
		if !ok {
			continue
		}
		cn, convErr := ciliumNodeFromUnstructured(u)
		if convErr != nil || len(cn.PodCIDRs) == 0 {
			continue
		}
		if cn.PodCIDRs[0] == cidr {
			return cn.Name
		}
	}

	return ""
}

// locationEmpty reports whether the resolved location is still empty.
func (c *RouteController) locationEmpty() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.cfg.Location == ""
}
