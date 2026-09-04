package sdn

import "k8s.io/klog/v2"

// The fake emits exactly one structured klog line per RPC describing the call
// that would be sent, before applying any table changes. Keys mirror the
// merged proto fields so the logs read like real RPC traces.

func (f *LoggingFakeClient) logCreate(ctx PodCIDRAllocationContext, spec PodCIDRAllocationSpec, opID string) {
	klog.InfoS("SDN RPC (fake)",
		"rpc", "CreatePodCIDRAllocations",
		"project_id", ctx.ProjectID,
		"vpc_network_id", ctx.VPCNetworkID,
		"location", ctx.Location,
		"vpc_prefix_reservation_id", spec.VPCPrefixReservationID,
		"network_interface_id", spec.NetworkInterfaceID,
		"destination_cidr", spec.DestinationCIDR,
		"operation_id", opID,
	)
}

func (f *LoggingFakeClient) logDelete(ctx PodCIDRAllocationContext, ids []string, opID string) {
	klog.InfoS("SDN RPC (fake)",
		"rpc", "DeletePodCIDRAllocations",
		"project_id", ctx.ProjectID,
		"vpc_network_id", ctx.VPCNetworkID,
		"location", ctx.Location,
		"ids", ids,
		"operation_id", opID,
	)
}

func (f *LoggingFakeClient) logList(q *ListPodCIDRAllocationsQuery, matches int) {
	klog.InfoS("SDN RPC (fake)",
		"rpc", "ListPodCIDRAllocations",
		"vpc_prefix_reservation_ids", q.VPCPrefixReservationIDs,
		"destination_cidr", q.DestinationCIDR,
		"matches", matches,
	)
}

func (f *LoggingFakeClient) logListReservations(ids []string, matches int) {
	klog.InfoS("SDN RPC (fake)",
		"rpc", "ListVPCPrefixReservations",
		"ids", ids,
		"matches", matches,
	)
}

func (f *LoggingFakeClient) logListOps(q *ListPodCIDRAllocationOperationsQuery, results []Operation) {
	states := make([]OperationState, 0, len(results))
	for i := range results {
		states = append(states, results[i].State)
	}
	klog.InfoS("SDN RPC (fake)",
		"rpc", "ListPodCIDRAllocationOperations",
		"operation_ids", q.OperationIDs,
		"matches", len(results),
		"states", states,
	)
}
