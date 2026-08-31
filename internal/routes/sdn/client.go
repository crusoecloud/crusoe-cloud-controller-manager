package sdn

import (
	"context"
	"errors"
)

// Error taxonomy (from the merged proto header). The real client maps gRPC
// status codes plus google.rpc.ErrorInfo to these sentinels; the fake returns
// them directly. Contract guarantees:
//   - ALREADY_EXISTS is NEVER returned. An identical create is idempotent
//     success (never creates a second allocation). Delete of an absent id is
//     success. "Already done" always succeeds (intent-based).
//   - Validation failures (INVALID_ARGUMENT etc.) fail the RPC synchronously and
//     write nothing — permanent, do not retry until inputs change.
//   - ABORTED / UNAVAILABLE are retryable with backoff.
var (
	// ErrDestinationConflict indicates destination_cidr is held by a DIFFERENT
	// interface — FAILED_PRECONDITION with google.rpc.ErrorInfo reason
	// DESTINATION_ALLOCATED_TO_ANOTHER_INTERFACE. Callers MUST branch on the
	// ErrorInfo REASON, not the code (FAILED_PRECONDITION is also the
	// unclassified bucket). For the CCM this is RETRYABLE-WITH-BACKOFF, not
	// permanent: the stale allocation clears when the old VM's delete lands.
	ErrDestinationConflict = errors.New("destination cidr allocated to another interface")
	// ErrInvalidArgument indicates a request validation failure. Permanent.
	ErrInvalidArgument = errors.New("invalid pod cidr allocation request")
	// ErrNotFound indicates a context project mismatch, or a delete batch
	// containing an id in another vpc/location (fails the whole call).
	ErrNotFound = errors.New("pod cidr allocation not found in context")
	// ErrUnavailable indicates an ABORTED/UNAVAILABLE-class transient failure.
	// Retryable.
	ErrUnavailable = errors.New("pod cidr allocation service unavailable")
)

//go:generate mockgen -source=client.go -destination=mock/client.go

// PodCIDRAllocationClient is the CCM-side seam over the merged SDN service
// island.v2.region.PodCIDRAllocationManagement (schemas MR 4075).
type PodCIDRAllocationClient interface {
	// CreatePodCIDRAllocations is async: the returned Operation starts
	// IN_PROGRESS. Safe to retry: an identical request never creates a second
	// allocation. This controller always sends exactly 1 spec.
	CreatePodCIDRAllocations(ctx context.Context, req CreatePodCIDRAllocationsRequest) (*Operation, error)
	// DeletePodCIDRAllocations is async and intent-based (absent ids are skipped
	// as success). Sole caller is CloudRoutes.DeleteRoute, driven by the upstream
	// route controller's delete loop; still Unimplemented server-side, so every
	// call surfaces that error until the server lands it.
	DeletePodCIDRAllocations(ctx context.Context, req DeletePodCIDRAllocationsRequest) (*Operation, error)
	// ListPodCIDRAllocations returns allocations matching the query.
	ListPodCIDRAllocations(ctx context.Context, q ListPodCIDRAllocationsQuery) ([]PodCIDRAllocation, error)
	// ListPodCIDRAllocationOperations supports repeated operation_ids — ONE call
	// covers every in-flight operation (the opTracker relies on this).
	ListPodCIDRAllocationOperations(ctx context.Context, q ListPodCIDRAllocationOperationsQuery) ([]Operation, error)
	// ListVPCPrefixReservations returns the reservations with the given ids.
	// Only called when more than one reservation is configured (post pod-range
	// expansion) to pick the reservation containing a node's pod cidr.
	ListVPCPrefixReservations(ctx context.Context, ids []string) ([]VPCPrefixReservation, error)
}
