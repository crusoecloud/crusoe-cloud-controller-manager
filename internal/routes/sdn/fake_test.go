package sdn_test

import (
	"context"
	"errors"
	"testing"

	"github.com/crusoecloud/crusoe-cloud-controller-manager/internal/routes/sdn"
)

const (
	testProject     = "proj-123"
	testVPC         = "net-abc"
	testLocation    = "us-east1-a"
	testReservation = "rsv-pods"
	testNIC         = "nic-1"
	testCIDR        = "10.100.4.0/24"
)

func testCtx() sdn.PodCIDRAllocationContext {
	return sdn.PodCIDRAllocationContext{ProjectID: testProject, VPCNetworkID: testVPC, Location: testLocation}
}

func createReq(nic, cidr string) sdn.CreatePodCIDRAllocationsRequest {
	return sdn.CreatePodCIDRAllocationsRequest{
		Allocations: []sdn.PodCIDRAllocationSpec{
			{VPCPrefixReservationID: testReservation, NetworkInterfaceID: nic, DestinationCIDR: cidr},
		},
		Context: testCtx(),
	}
}

// mustList lists allocations for the given query, failing the test on error.
//
//nolint:gocritic // q mirrors the value-receiver client interface signature
func mustList(t *testing.T, f *sdn.LoggingFakeClient, q sdn.ListPodCIDRAllocationsQuery) []sdn.PodCIDRAllocation {
	t.Helper()
	allocs, err := f.ListPodCIDRAllocations(context.Background(), q)
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	return allocs
}

// drainOp polls a single op to a terminal state (bounded loop).
func drainOp(t *testing.T, f *sdn.LoggingFakeClient, opID string) sdn.Operation {
	t.Helper()
	for range 10 {
		ops, err := f.ListPodCIDRAllocationOperations(context.Background(),
			sdn.ListPodCIDRAllocationOperationsQuery{OperationIDs: []string{opID}})
		if err != nil {
			t.Fatalf("list ops: %v", err)
		}
		if len(ops) != 1 {
			t.Fatalf("expected 1 op, got %d", len(ops))
		}
		if ops[0].State != sdn.OperationStateInProgress {
			return ops[0]
		}
	}
	t.Fatalf("op %s never resolved", opID)

	return sdn.Operation{}
}

func TestCreate_HappyPath(t *testing.T) {
	t.Parallel()
	f := sdn.NewLoggingFakeClient()
	op, err := f.CreatePodCIDRAllocations(context.Background(), createReq(testNIC, testCIDR))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if op.State != sdn.OperationStateInProgress {
		t.Fatalf("expected IN_PROGRESS, got %s", op.State)
	}
	resolved := drainOp(t, f, op.OperationID)
	if resolved.State != sdn.OperationStateSucceeded {
		t.Fatalf("expected SUCCEEDED, got %s", resolved.State)
	}
	allocs, err := f.ListPodCIDRAllocations(context.Background(),
		sdn.ListPodCIDRAllocationsQuery{DestinationCIDR: testCIDR})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(allocs) != 1 {
		t.Fatalf("expected 1 allocation, got %d", len(allocs))
	}
	if allocs[0].NextHopIP != "fake-next-hop" {
		t.Fatalf("expected fake next hop, got %q", allocs[0].NextHopIP)
	}
}

func TestCreate_InvalidBatchSize(t *testing.T) {
	t.Parallel()
	f := sdn.NewLoggingFakeClient()
	req := sdn.CreatePodCIDRAllocationsRequest{Context: testCtx()} // 0 specs
	if _, err := f.CreatePodCIDRAllocations(context.Background(), req); !errors.Is(err, sdn.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument for 0 specs, got %v", err)
	}
	req2 := sdn.CreatePodCIDRAllocationsRequest{
		Allocations: []sdn.PodCIDRAllocationSpec{
			{VPCPrefixReservationID: testReservation, NetworkInterfaceID: testNIC, DestinationCIDR: testCIDR},
			{VPCPrefixReservationID: testReservation, NetworkInterfaceID: "nic-2", DestinationCIDR: "10.100.5.0/24"},
		},
		Context: testCtx(),
	}
	if _, err := f.CreatePodCIDRAllocations(context.Background(), req2); !errors.Is(err, sdn.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument for 2 specs, got %v", err)
	}
}

func TestCreate_IdenticalIsIdempotent(t *testing.T) {
	t.Parallel()
	f := sdn.NewLoggingFakeClient()
	op1, err := f.CreatePodCIDRAllocations(context.Background(), createReq(testNIC, testCIDR))
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	drainOp(t, f, op1.OperationID)

	// Identical create must not create a second row and must never return
	// ErrDestinationConflict / ALREADY_EXISTS.
	op2, err := f.CreatePodCIDRAllocations(context.Background(), createReq(testNIC, testCIDR))
	if err != nil {
		t.Fatalf("second (identical) create: %v", err)
	}
	if op2.State != sdn.OperationStateSucceeded {
		t.Fatalf("expected idempotent SUCCEEDED, got %s", op2.State)
	}
	allocs, err := f.ListPodCIDRAllocations(context.Background(),
		sdn.ListPodCIDRAllocationsQuery{DestinationCIDR: testCIDR})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(allocs) != 1 {
		t.Fatalf("idempotent create produced %d rows, want 1", len(allocs))
	}
	if op2.AllocationIDs[0] != op1.AllocationIDs[0] {
		t.Fatalf("idempotent create returned different id: %s vs %s", op2.AllocationIDs[0], op1.AllocationIDs[0])
	}
}

func TestCreate_ConflictingNIC(t *testing.T) {
	t.Parallel()
	f := sdn.NewLoggingFakeClient()
	op1, err := f.CreatePodCIDRAllocations(context.Background(), createReq(testNIC, testCIDR))
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	drainOp(t, f, op1.OperationID)

	// Same destination, different NIC -> synchronous conflict, nothing written.
	_, err = f.CreatePodCIDRAllocations(context.Background(), createReq("nic-other", testCIDR))
	if !errors.Is(err, sdn.ErrDestinationConflict) {
		t.Fatalf("expected ErrDestinationConflict, got %v", err)
	}
}

func TestCreate_ConflictCIDRKnob(t *testing.T) {
	t.Parallel()
	f := sdn.NewLoggingFakeClient()
	f.ConflictCIDRs[testCIDR] = true
	_, err := f.CreatePodCIDRAllocations(context.Background(), createReq(testNIC, testCIDR))
	if !errors.Is(err, sdn.ErrDestinationConflict) {
		t.Fatalf("expected ErrDestinationConflict from knob, got %v", err)
	}
	// Nothing written.
	allocs, err := f.ListPodCIDRAllocations(context.Background(),
		sdn.ListPodCIDRAllocationsQuery{DestinationCIDR: testCIDR})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(allocs) != 0 {
		t.Fatalf("conflict knob wrote %d rows, want 0", len(allocs))
	}
}

func TestCreate_FailNextKeepsRow(t *testing.T) {
	t.Parallel()
	f := sdn.NewLoggingFakeClient()
	f.FailNext = true
	op, err := f.CreatePodCIDRAllocations(context.Background(), createReq(testNIC, testCIDR))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	resolved := drainOp(t, f, op.OperationID)
	if resolved.State != sdn.OperationStateFailed {
		t.Fatalf("expected FAILED, got %s", resolved.State)
	}
	if resolved.Error == "" {
		t.Fatalf("expected error detail on FAILED op")
	}
	// The server does not roll back on OVN failure: the row survives a FAILED
	// create, so a retry converges on the same row (adopt, not re-create).
	allocs, err := f.ListPodCIDRAllocations(context.Background(),
		sdn.ListPodCIDRAllocationsQuery{DestinationCIDR: testCIDR})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(allocs) != 1 {
		t.Fatalf("FailNext left %d rows, want 1 (no rollback)", len(allocs))
	}
	// FailNext auto-clears.
	if f.FailNext {
		t.Fatalf("FailNext did not auto-clear")
	}
}

func TestPendingPolls(t *testing.T) {
	t.Parallel()
	f := sdn.NewLoggingFakeClient()
	f.PendingPolls = 3
	op, err := f.CreatePodCIDRAllocations(context.Background(), createReq(testNIC, testCIDR))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for i := range 2 {
		ops, listErr := f.ListPodCIDRAllocationOperations(context.Background(),
			sdn.ListPodCIDRAllocationOperationsQuery{OperationIDs: []string{op.OperationID}})
		if listErr != nil {
			t.Fatalf("list ops: %v", listErr)
		}
		if ops[0].State != sdn.OperationStateInProgress {
			t.Fatalf("poll %d: expected IN_PROGRESS, got %s", i, ops[0].State)
		}
	}
	// Third poll flips terminal.
	ops, err := f.ListPodCIDRAllocationOperations(context.Background(),
		sdn.ListPodCIDRAllocationOperationsQuery{OperationIDs: []string{op.OperationID}})
	if err != nil {
		t.Fatalf("list ops: %v", err)
	}
	if ops[0].State != sdn.OperationStateSucceeded {
		t.Fatalf("expected SUCCEEDED on third poll, got %s", ops[0].State)
	}
}

func TestDelete_UnknownIDsSucceed(t *testing.T) {
	t.Parallel()
	f := sdn.NewLoggingFakeClient()
	op, err := f.DeletePodCIDRAllocations(context.Background(),
		sdn.DeletePodCIDRAllocationsRequest{IDs: []string{"al-nonexistent"}, Context: testCtx()})
	if err != nil {
		t.Fatalf("delete unknown: %v", err)
	}
	resolved := drainOp(t, f, op.OperationID)
	if resolved.State != sdn.OperationStateSucceeded {
		t.Fatalf("delete of unknown id should succeed, got %s", resolved.State)
	}
}

func TestDelete_RemovesRowOnSuccess(t *testing.T) {
	t.Parallel()
	f := sdn.NewLoggingFakeClient()
	cop, err := f.CreatePodCIDRAllocations(context.Background(), createReq(testNIC, testCIDR))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	created := drainOp(t, f, cop.OperationID)
	id := created.AllocationIDs[0]

	dop, err := f.DeletePodCIDRAllocations(context.Background(),
		sdn.DeletePodCIDRAllocationsRequest{IDs: []string{id}, Context: testCtx()})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	// Row listed until delete resolves.
	allocs := mustList(t, f, sdn.ListPodCIDRAllocationsQuery{PodCIDRAllocationIDs: []string{id}})
	if len(allocs) != 1 {
		t.Fatalf("row should be listed until delete resolves, got %d", len(allocs))
	}
	drainOp(t, f, dop.OperationID)
	allocs = mustList(t, f, sdn.ListPodCIDRAllocationsQuery{PodCIDRAllocationIDs: []string{id}})
	if len(allocs) != 0 {
		t.Fatalf("row should be gone after delete resolves, got %d", len(allocs))
	}
}

func TestDelete_TooManyIDs(t *testing.T) {
	t.Parallel()
	f := sdn.NewLoggingFakeClient()
	ids := make([]string, 51)
	for i := range ids {
		ids[i] = "al-x"
	}
	_, err := f.DeletePodCIDRAllocations(context.Background(),
		sdn.DeletePodCIDRAllocationsRequest{IDs: ids, Context: testCtx()})
	if !errors.Is(err, sdn.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument for >50 ids, got %v", err)
	}
}

func TestDelete_FailNextRetainsRows(t *testing.T) {
	t.Parallel()
	f := sdn.NewLoggingFakeClient()
	cop, err := f.CreatePodCIDRAllocations(context.Background(), createReq(testNIC, testCIDR))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	created := drainOp(t, f, cop.OperationID)
	id := created.AllocationIDs[0]

	f.FailNext = true
	dop, err := f.DeletePodCIDRAllocations(context.Background(),
		sdn.DeletePodCIDRAllocationsRequest{IDs: []string{id}, Context: testCtx()})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	resolved := drainOp(t, f, dop.OperationID)
	if resolved.State != sdn.OperationStateFailed {
		t.Fatalf("expected FAILED delete, got %s", resolved.State)
	}
	allocs := mustList(t, f, sdn.ListPodCIDRAllocationsQuery{PodCIDRAllocationIDs: []string{id}})
	if len(allocs) != 1 {
		t.Fatalf("failed delete should retain row, got %d", len(allocs))
	}
}

func TestList_RequiresBoundingFilter(t *testing.T) {
	t.Parallel()
	f := sdn.NewLoggingFakeClient()
	// No filter -> invalid.
	if _, err := f.ListPodCIDRAllocations(context.Background(),
		sdn.ListPodCIDRAllocationsQuery{}); !errors.Is(err, sdn.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument for empty query, got %v", err)
	}
	// Location alone does not count.
	if _, err := f.ListPodCIDRAllocations(context.Background(),
		sdn.ListPodCIDRAllocationsQuery{Location: testLocation}); !errors.Is(err, sdn.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument for location-only query, got %v", err)
	}
}

func TestList_FilterIntersectionAndOrdering(t *testing.T) {
	t.Parallel()
	f := sdn.NewLoggingFakeClient()
	// Insert three allocations with different destinations.
	for _, c := range []struct {
		nic, cidr string
	}{
		{"nic-a", "10.100.3.0/24"},
		{"nic-b", "10.100.1.0/24"},
		{"nic-c", "10.100.2.0/24"},
	} {
		op, err := f.CreatePodCIDRAllocations(context.Background(), createReq(c.nic, c.cidr))
		if err != nil {
			t.Fatalf("create %s: %v", c.cidr, err)
		}
		drainOp(t, f, op.OperationID)
	}

	all, err := f.ListPodCIDRAllocations(context.Background(),
		sdn.ListPodCIDRAllocationsQuery{VPCPrefixReservationIDs: []string{testReservation}})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("expected 3, got %d", len(all))
	}
	// Ordered by destination cidr.
	want := []string{"10.100.1.0/24", "10.100.2.0/24", "10.100.3.0/24"}
	for i, a := range all {
		if a.DestinationCIDR != want[i] {
			t.Fatalf("order mismatch at %d: got %s want %s", i, a.DestinationCIDR, want[i])
		}
	}

	// Intersecting filter narrows to one.
	one, err := f.ListPodCIDRAllocations(context.Background(),
		sdn.ListPodCIDRAllocationsQuery{VPCPrefixReservationIDs: []string{testReservation}, NetworkInterfaceID: "nic-b"})
	if err != nil {
		t.Fatalf("list filtered: %v", err)
	}
	if len(one) != 1 || one[0].NetworkInterfaceID != "nic-b" {
		t.Fatalf("expected single nic-b match, got %+v", one)
	}
}

func TestListOps_BatchAndEmpty(t *testing.T) {
	t.Parallel()
	f := sdn.NewLoggingFakeClient()
	f.PendingPolls = 5 // keep them in-progress
	op1, err := f.CreatePodCIDRAllocations(context.Background(), createReq("nic-a", "10.0.1.0/24"))
	if err != nil {
		t.Fatalf("create op1: %v", err)
	}
	op2, err := f.CreatePodCIDRAllocations(context.Background(), createReq("nic-b", "10.0.2.0/24"))
	if err != nil {
		t.Fatalf("create op2: %v", err)
	}

	ops, err := f.ListPodCIDRAllocationOperations(context.Background(),
		sdn.ListPodCIDRAllocationOperationsQuery{OperationIDs: []string{op1.OperationID, op2.OperationID}})
	if err != nil {
		t.Fatalf("list ops: %v", err)
	}
	if len(ops) != 2 {
		t.Fatalf("expected 2 ops in batch, got %d", len(ops))
	}

	// Query for an unknown op id returns empty, no error.
	none, err := f.ListPodCIDRAllocationOperations(context.Background(),
		sdn.ListPodCIDRAllocationOperationsQuery{OperationIDs: []string{"op-unknown"}})
	if err != nil {
		t.Fatalf("list ops unknown: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("expected 0 ops for unknown id, got %d", len(none))
	}
}

func TestInterfaceSatisfied(t *testing.T) {
	t.Parallel()
	var _ sdn.PodCIDRAllocationClient = sdn.NewLoggingFakeClient()
}
