// Package sdn contains the CCM-side seam over the SDN service
// island.v2.region.PodCIDRAllocationManagement (schemas MR 4075). The types
// here are plain Go structs that mirror the merged proto shapes so that the
// controller depends on no grpc/protobuf types. When the generated gRPC client
// is adopted, only a single new file (sdn/grpc.go) maps between these structs
// and the pb types; the state machine, tracker, reaper and tests are unchanged.
package sdn

import "time"

// OperationState mirrors island.v2.component.OperationState.
type OperationState string

const (
	// OperationStateInProgress is the initial state of every async operation.
	OperationStateInProgress OperationState = "IN_PROGRESS"
	// OperationStateSucceeded is a terminal success state.
	OperationStateSucceeded OperationState = "SUCCEEDED"
	// OperationStateFailed is a terminal failure state (OVN/DB failure only).
	OperationStateFailed OperationState = "FAILED"
)

// Operation mirrors island.v2.component.Operation as used by
// PodCIDRAllocationManagement. Only OVN/DB failures land in the operation
// result; validation failures fail the RPC synchronously and write nothing.
type Operation struct {
	OperationID   string
	State         OperationState
	AllocationIDs []string // pod CIDR allocation ids the operation acts on
	Error         string   // set iff State == FAILED
}

// PodCIDRAllocationContext scopes every RPC. A project mismatch yields NOT_FOUND.
type PodCIDRAllocationContext struct {
	ProjectID    string
	VPCNetworkID string
	Location     string
}

// PodCIDRAllocationSpec describes one allocation to create. All fields are
// required. The reservation must contain destination_cidr; the NIC must be in
// the same vpc+location, PRIMARY, with a private IP; host bits must be zero;
// destination is unique per VPC.
type PodCIDRAllocationSpec struct {
	VPCPrefixReservationID string
	NetworkInterfaceID     string
	DestinationCIDR        string
}

// PodCIDRAllocation is a static route on the VPC logical router plus a
// port_security widening on the NIC, created/deleted as an atomic pair. It is
// immutable (there is no update RPC). NextHopIP is output-only.
type PodCIDRAllocation struct {
	ID                     string
	Context                PodCIDRAllocationContext
	VPCPrefixReservationID string
	NetworkInterfaceID     string
	DestinationCIDR        string
	NextHopIP              string
	CreatedAt              time.Time
}

// CreatePodCIDRAllocationsRequest is a batch, all-or-nothing request executed as
// ONE OVN transaction. It is capped at 1 allocation per call for now (>1 today
// returns INVALID_ARGUMENT; the repeated shape lets the cap rise without a
// contract change).
type CreatePodCIDRAllocationsRequest struct {
	Allocations []PodCIDRAllocationSpec
	Context     PodCIDRAllocationContext
}

// DeletePodCIDRAllocationsRequest is a batch-by-id request, at most 50 ids,
// all-or-nothing, one OVN transaction. It is intent-based: an id that no longer
// exists is skipped (success). An id in another vpc/location fails the WHOLE
// call with NOT_FOUND.
type DeletePodCIDRAllocationsRequest struct {
	IDs     []string
	Context PodCIDRAllocationContext
}

// ListPodCIDRAllocationsQuery requires at least ONE bounding filter (Location
// alone does NOT count) else INVALID_ARGUMENT. Results are ordered by
// destination_cidr. An empty result is success. Zero-valued fields are unset;
// set fields intersect.
type ListPodCIDRAllocationsQuery struct {
	PodCIDRAllocationIDs    []string
	ProjectID               string
	VPCNetworkID            string
	Location                string
	VPCPrefixReservationIDs []string
	NetworkInterfaceID      string
	DestinationCIDR         string // exact match
}

// VPCPrefixReservation mirrors the KM-owned reservation object
// (VPCPrefixReservationManagement, schemas MR 4113) — only the fields the CCM
// needs to map a pod cidr to its containing reservation after a pod-range
// expansion adds a second reservation.
type VPCPrefixReservation struct {
	ID     string
	Prefix string // the reserved range allocations are carved from
}

// ListPodCIDRAllocationOperationsQuery filters operations. It supports repeated
// operation_ids so ONE call covers every in-flight operation (the opTracker
// relies on this).
type ListPodCIDRAllocationOperationsQuery struct {
	OperationIDs        []string
	ProjectIDs          []string
	PodCIDRAllocationID string
	OperationStates     []OperationState
}
