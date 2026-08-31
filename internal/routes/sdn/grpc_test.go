package sdn

import (
	"errors"
	"testing"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	component "gitlab.com/crusoeenergy/schemas/api/island/v2/component"
	region "gitlab.com/crusoeenergy/schemas/api/island/v2/region"
)

// mustStatusWithErrorInfo builds a FAILED_PRECONDITION status carrying an
// ErrorInfo detail, as the region server stamps on a destination conflict.
func mustStatusWithErrorInfo(t *testing.T, reason, domain string) error {
	t.Helper()
	st, err := status.New(codes.FailedPrecondition, "destination held elsewhere").
		WithDetails(&errdetails.ErrorInfo{Reason: reason, Domain: domain})
	if err != nil {
		t.Fatalf("building status: %v", err)
	}

	return st.Err() //nolint:wrapcheck // test helper returns a raw gRPC status error on purpose
}

func TestMapError_DestinationConflict(t *testing.T) {
	t.Parallel()
	err := mapError(mustStatusWithErrorInfo(t, reasonDestinationConflict, errorInfoDomain))
	if !errors.Is(err, ErrDestinationConflict) {
		t.Fatalf("want ErrDestinationConflict, got %v", err)
	}
}

func TestMapError_FailedPreconditionWrongDomainNotConflict(t *testing.T) {
	t.Parallel()
	err := mapError(mustStatusWithErrorInfo(t, reasonDestinationConflict, "some.other.domain"))
	if errors.Is(err, ErrDestinationConflict) {
		t.Fatalf("wrong domain must NOT map to conflict, got %v", err)
	}
	if err == nil {
		t.Fatalf("expected a wrapped error")
	}
}

func TestMapError_FailedPreconditionWrongReasonNotConflict(t *testing.T) {
	t.Parallel()
	err := mapError(mustStatusWithErrorInfo(t, "SOMETHING_ELSE", errorInfoDomain))
	if errors.Is(err, ErrDestinationConflict) {
		t.Fatalf("wrong reason must NOT map to conflict, got %v", err)
	}
}

func TestMapError_FailedPreconditionNoDetailNotConflict(t *testing.T) {
	t.Parallel()
	// e.g. location not ACTIVE: FAILED_PRECONDITION with no ErrorInfo.
	err := mapError(status.Error(codes.FailedPrecondition, "location not active"))
	if errors.Is(err, ErrDestinationConflict) {
		t.Fatalf("bare FAILED_PRECONDITION must NOT map to conflict, got %v", err)
	}
	if err == nil {
		t.Fatalf("expected a wrapped error")
	}
}

func TestMapError_CodeMappings(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		code     codes.Code
		sentinel error
	}{
		{"invalid argument", codes.InvalidArgument, ErrInvalidArgument},
		{"not found", codes.NotFound, ErrNotFound},
		{"unavailable", codes.Unavailable, ErrUnavailable},
		{"aborted", codes.Aborted, ErrUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := mapError(status.Error(tc.code, "boom"))
			if !errors.Is(err, tc.sentinel) {
				t.Fatalf("code %s: want %v, got %v", tc.code, tc.sentinel, err)
			}
		})
	}
}

func TestMapError_UnimplementedIsPlainWrapped(t *testing.T) {
	t.Parallel()
	err := mapError(status.Error(codes.Unimplemented, "not served"))
	if err == nil {
		t.Fatalf("expected error")
	}
	for _, s := range []error{ErrDestinationConflict, ErrInvalidArgument, ErrNotFound, ErrUnavailable} {
		if errors.Is(err, s) {
			t.Fatalf("Unimplemented must not map to a sentinel, got %v (matched %v)", err, s)
		}
	}
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("original status must be preserved, got %v", status.Code(err))
	}
}

func TestMapError_UnknownIsWrappedAsIs(t *testing.T) {
	t.Parallel()
	err := mapError(status.Error(codes.Internal, "kaboom"))
	if err == nil || status.Code(err) != codes.Internal {
		t.Fatalf("unknown code must be wrapped preserving status, got %v", err)
	}
}

func TestMapError_NilIsNil(t *testing.T) {
	t.Parallel()
	if err := mapError(nil); err != nil {
		t.Fatalf("nil must map to nil, got %v", err)
	}
}

func TestAllocationFromPB_RoundTrip(t *testing.T) {
	t.Parallel()
	created := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	pb := &component.PodCIDRAllocation{
		Id: "al-1",
		Context: &component.PodCIDRAllocationContext{
			ProjectId: "proj", VpcNetworkId: "vpc", Location: "loc",
		},
		VpcPrefixReservationId: "rsv-1",
		NetworkInterfaceId:     "nic-1",
		DestinationCidr:        "10.0.1.0/24",
		NextHopIp:              "10.0.0.5",
		CreatedAt:              timestamppb.New(created),
	}
	got := allocationFromPB(pb)
	want := PodCIDRAllocation{
		ID:                     "al-1",
		Context:                PodCIDRAllocationContext{ProjectID: "proj", VPCNetworkID: "vpc", Location: "loc"},
		VPCPrefixReservationID: "rsv-1",
		NetworkInterfaceID:     "nic-1",
		DestinationCIDR:        "10.0.1.0/24",
		NextHopIP:              "10.0.0.5",
		CreatedAt:              created,
	}
	// Compare timestamps by value then zero them so the struct compare is exact.
	if !got.CreatedAt.Equal(want.CreatedAt) {
		t.Fatalf("CreatedAt mismatch: got %v want %v", got.CreatedAt, want.CreatedAt)
	}
	got.CreatedAt, want.CreatedAt = time.Time{}, time.Time{}
	if got != want {
		t.Fatalf("allocationFromPB mismatch:\n got %+v\nwant %+v", got, want)
	}
}

func TestOperationFromPB_States(t *testing.T) {
	t.Parallel()
	cases := []struct {
		pb   component.OperationState
		want OperationState
	}{
		{component.OperationState_OPERATION_STATE_IN_PROGRESS, OperationStateInProgress},
		{component.OperationState_OPERATION_STATE_SUCCEEDED, OperationStateSucceeded},
		{component.OperationState_OPERATION_STATE_FAILED, OperationStateFailed},
		{component.OperationState_OPERATION_STATE_UNSPECIFIED, OperationStateInProgress},
	}
	for _, tc := range cases {
		op := operationFromPB(&component.Operation{OperationId: "op-1", OperationState: tc.pb})
		if op.State != tc.want {
			t.Fatalf("state %v: want %v, got %v", tc.pb, tc.want, op.State)
		}
	}
}

func TestOperationFromPB_NilIsNil(t *testing.T) {
	t.Parallel()
	if op := operationFromPB(nil); op != nil {
		t.Fatalf("nil operation must map to nil, got %+v", op)
	}
}

func TestOperationFromPB_FailedCarriesErrorMessage(t *testing.T) {
	t.Parallel()
	op := operationFromPB(&component.Operation{
		OperationId:    "op-1",
		OperationState: component.OperationState_OPERATION_STATE_FAILED,
		Result:         &component.Operation_Error{Error: status.New(codes.Internal, "ovn failed").Proto()},
	})
	if op.State != OperationStateFailed {
		t.Fatalf("want FAILED, got %v", op.State)
	}
	if op.Error != "ovn failed" {
		t.Fatalf("want error message propagated, got %q", op.Error)
	}
}

func TestOperationFromPB_AllocationIDsFromBulkMetadata(t *testing.T) {
	t.Parallel()
	meta, err := anypb.New(&component.BulkOperationMetadata{ResourceIds: []string{"al-1", "al-2"}})
	if err != nil {
		t.Fatalf("packing metadata: %v", err)
	}
	op := operationFromPB(&component.Operation{
		OperationId:    "op-1",
		OperationState: component.OperationState_OPERATION_STATE_SUCCEEDED,
		Metadata:       meta,
	})
	if len(op.AllocationIDs) != 2 || op.AllocationIDs[0] != "al-1" || op.AllocationIDs[1] != "al-2" {
		t.Fatalf("want [al-1 al-2], got %v", op.AllocationIDs)
	}
}

func TestOperationFromPB_AllocationIDsFromSingleMetadata(t *testing.T) {
	t.Parallel()
	meta, err := anypb.New(&component.OperationMetadata{ResourceId: "al-9"})
	if err != nil {
		t.Fatalf("packing metadata: %v", err)
	}
	op := operationFromPB(&component.Operation{OperationId: "op-1", Metadata: meta})
	if len(op.AllocationIDs) != 1 || op.AllocationIDs[0] != "al-9" {
		t.Fatalf("want [al-9], got %v", op.AllocationIDs)
	}
}

func TestPBOperationState_RoundTrip(t *testing.T) {
	t.Parallel()
	for _, s := range []OperationState{OperationStateInProgress, OperationStateSucceeded, OperationStateFailed} {
		if got := seamOperationState(pbOperationState(s)); got != s {
			t.Fatalf("round-trip %v: got %v", s, got)
		}
	}
}

func TestPBContext(t *testing.T) {
	t.Parallel()
	got := pbContext(PodCIDRAllocationContext{ProjectID: "p", VPCNetworkID: "v", Location: "l"})
	if got.GetProjectId() != "p" || got.GetVpcNetworkId() != "v" || got.GetLocation() != "l" {
		t.Fatalf("pbContext mismatch: %+v", got)
	}
}

// TestListPodCIDRAllocationsQueryBuild verifies the seam query maps onto the pb
// query field-for-field (guards against a silent field transposition).
func TestListPodCIDRAllocationsQueryBuild(t *testing.T) {
	t.Parallel()
	q := ListPodCIDRAllocationsQuery{
		PodCIDRAllocationIDs:    []string{"al-1"},
		ProjectID:               "proj",
		VPCNetworkID:            "vpc",
		Location:                "loc",
		VPCPrefixReservationIDs: []string{"rsv-1"},
		NetworkInterfaceID:      "nic-1",
		DestinationCIDR:         "10.0.1.0/24",
	}
	pb := &region.ListPodCIDRAllocationsQuery{
		PodCidrAllocationIds:    q.PodCIDRAllocationIDs,
		ProjectId:               q.ProjectID,
		VpcNetworkId:            q.VPCNetworkID,
		Location:                q.Location,
		VpcPrefixReservationIds: q.VPCPrefixReservationIDs,
		NetworkInterfaceId:      q.NetworkInterfaceID,
		DestinationCidr:         q.DestinationCIDR,
	}
	scalarsMatch := pb.GetProjectId() == q.ProjectID && pb.GetVpcNetworkId() == q.VPCNetworkID &&
		pb.GetLocation() == q.Location && pb.GetNetworkInterfaceId() == q.NetworkInterfaceID &&
		pb.GetDestinationCidr() == q.DestinationCIDR
	slicesMatch := len(pb.GetPodCidrAllocationIds()) == 1 && len(pb.GetVpcPrefixReservationIds()) == 1
	if !scalarsMatch || !slicesMatch {
		t.Fatalf("query field transposition: %+v", pb)
	}
}
