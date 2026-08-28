package sdn

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	// defaultPendingPolls is how many ListPodCIDRAllocationOperations
	// observations an operation stays IN_PROGRESS before flipping terminal.
	defaultPendingPolls = 1
	// maxDeleteBatch is the contract cap on delete ids per call.
	maxDeleteBatch = 50
	// createCap is the current cap on allocations per create call.
	createCap = 1
	// fakeNextHop is the placeholder next-hop the fake records on new
	// allocations.
	fakeNextHop = "fake-next-hop"
)

// fakeOp is an in-flight or terminal operation tracked by the fake.
type fakeOp struct {
	op        Operation
	remaining int  // polls left before flipping terminal; <=0 means terminal
	fail      bool // resolve FAILED (and roll back the mutation) at terminal
	// createdIDs are allocation ids inserted by a create op, rolled back if it
	// resolves FAILED.
	createdIDs []string
	// deleteIDs are allocation ids a delete op removes when it resolves
	// SUCCEEDED.
	deleteIDs []string
}

// LoggingFakeClient is an in-memory PodCIDRAllocationClient that logs every
// would-be RPC and honors the merged contract's intent-based semantics. It is
// the client that ships this drop; the real gRPC client replaces it later.
type LoggingFakeClient struct {
	mu     sync.Mutex
	allocs map[string]PodCIDRAllocation // allocation id -> allocation
	ops    map[string]*fakeOp           // op id -> op + remaining polls

	// PendingPolls is the number of ListPodCIDRAllocationOperations
	// observations an operation stays IN_PROGRESS before flipping terminal.
	// Default 1. Test knob.
	PendingPolls int
	// FailNext, if true, makes the NEXT Create/Delete's operation resolve
	// FAILED (and the mutation is rolled back); the flag auto-clears. Test knob.
	FailNext bool
	// ConflictCIDRs holds destinations for which CreatePodCIDRAllocations
	// returns ErrDestinationConflict, simulating a stale allocation held by a
	// dead VM's NIC. Test knob for the /24-reuse race.
	ConflictCIDRs map[string]bool
}

// NewLoggingFakeClient returns an empty LoggingFakeClient with default knobs.
func NewLoggingFakeClient() *LoggingFakeClient {
	return &LoggingFakeClient{
		allocs:        make(map[string]PodCIDRAllocation),
		ops:           make(map[string]*fakeOp),
		PendingPolls:  defaultPendingPolls,
		ConflictCIDRs: make(map[string]bool),
	}
}

var _ PodCIDRAllocationClient = (*LoggingFakeClient)(nil)

// SeedAllocation inserts a fully-formed allocation directly into the fake's
// table, bypassing the async create op. It exists for tests that need control
// over fields such as CreatedAt (e.g. exercising the reaper grace period).
func (f *LoggingFakeClient) SeedAllocation(a *PodCIDRAllocation) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.allocs[a.ID] = *a
}

func (f *LoggingFakeClient) pendingPolls() int {
	if f.PendingPolls > 0 {
		return f.PendingPolls
	}

	return defaultPendingPolls
}

// findByDestination returns the existing allocation with the given destination
// cidr, if any.
func (f *LoggingFakeClient) findByDestination(cidr string) (PodCIDRAllocation, bool) {
	for id := range f.allocs {
		if f.allocs[id].DestinationCIDR == cidr {
			return f.allocs[id], true
		}
	}

	return PodCIDRAllocation{}, false
}

// CreatePodCIDRAllocations inserts one allocation (current cap) and returns an
// IN_PROGRESS operation, honoring idempotency and conflict semantics.
func (f *LoggingFakeClient) CreatePodCIDRAllocations(
	_ context.Context, req CreatePodCIDRAllocationsRequest,
) (*Operation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if len(req.Allocations) != createCap {
		return nil, ErrInvalidArgument
	}
	spec := req.Allocations[0]

	opID := "op-" + uuid.NewString()
	f.logCreate(req.Context, spec, opID)

	if f.ConflictCIDRs[spec.DestinationCIDR] {
		return nil, ErrDestinationConflict
	}

	if existing, ok := f.findByDestination(spec.DestinationCIDR); ok {
		if existing.NetworkInterfaceID != spec.NetworkInterfaceID {
			// Held by a different interface: synchronous conflict, nothing
			// written.
			return nil, ErrDestinationConflict
		}

		// Identical spec already allocated: idempotent success referencing the
		// existing id, no second row.
		return f.resolvedOp(opID, []string{existing.ID}), nil
	}

	allocID := "al-" + uuid.NewString()
	alloc := PodCIDRAllocation{
		ID:                     allocID,
		Context:                req.Context,
		VPCPrefixReservationID: spec.VPCPrefixReservationID,
		NetworkInterfaceID:     spec.NetworkInterfaceID,
		DestinationCIDR:        spec.DestinationCIDR,
		NextHopIP:              fakeNextHop,
		CreatedAt:              time.Now(),
	}
	f.allocs[allocID] = alloc

	fail := f.consumeFailNext()
	f.ops[opID] = &fakeOp{
		op:         Operation{OperationID: opID, State: OperationStateInProgress, AllocationIDs: []string{allocID}},
		remaining:  f.pendingPolls(),
		fail:       fail,
		createdIDs: []string{allocID},
	}

	return &Operation{OperationID: opID, State: OperationStateInProgress, AllocationIDs: []string{allocID}}, nil
}

// resolvedOp records and returns an already-SUCCEEDED operation (idempotent
// create hit).
func (f *LoggingFakeClient) resolvedOp(opID string, allocIDs []string) *Operation {
	op := Operation{OperationID: opID, State: OperationStateSucceeded, AllocationIDs: allocIDs}
	f.ops[opID] = &fakeOp{op: op, remaining: 0}

	return &op
}

// DeletePodCIDRAllocations removes the given ids when its operation resolves
// SUCCEEDED. Unknown ids are skipped (success).
func (f *LoggingFakeClient) DeletePodCIDRAllocations(
	_ context.Context, req DeletePodCIDRAllocationsRequest,
) (*Operation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if len(req.IDs) > maxDeleteBatch {
		return nil, ErrInvalidArgument
	}

	opID := "op-" + uuid.NewString()
	f.logDelete(req.Context, req.IDs, opID)

	fail := f.consumeFailNext()
	f.ops[opID] = &fakeOp{
		op:        Operation{OperationID: opID, State: OperationStateInProgress, AllocationIDs: req.IDs},
		remaining: f.pendingPolls(),
		fail:      fail,
		deleteIDs: req.IDs,
	}

	return &Operation{OperationID: opID, State: OperationStateInProgress, AllocationIDs: req.IDs}, nil
}

// ListPodCIDRAllocations returns allocations matching every set filter, sorted
// by destination cidr.
//
//nolint:gocritic // q is passed by value to match the client interface contract
func (f *LoggingFakeClient) ListPodCIDRAllocations(
	_ context.Context, q ListPodCIDRAllocationsQuery,
) ([]PodCIDRAllocation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if !hasBoundingFilter(&q) {
		return nil, ErrInvalidArgument
	}

	out := make([]PodCIDRAllocation, 0, len(f.allocs))
	for id := range f.allocs {
		a := f.allocs[id]
		if matchesQuery(&a, &q) {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DestinationCIDR < out[j].DestinationCIDR })

	f.logList(&q, len(out))

	return out, nil
}

// ListPodCIDRAllocationOperations returns matching operations, advancing the
// poll counter of each matched IN_PROGRESS op (flipping it terminal at 0).
//
//nolint:gocritic // q is passed by value to match the client interface contract
func (f *LoggingFakeClient) ListPodCIDRAllocationOperations(
	_ context.Context, q ListPodCIDRAllocationOperationsQuery,
) ([]Operation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]Operation, 0, len(f.ops))
	for _, fo := range f.ops {
		if !matchesOpQuery(&fo.op, &q) {
			continue
		}
		f.advance(fo)
		out = append(out, fo.op)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OperationID < out[j].OperationID })

	f.logListOps(&q, out)

	return out, nil
}

// advance decrements a pending op's remaining counter and flips it terminal at
// zero, applying or rolling back its mutation.
func (f *LoggingFakeClient) advance(fo *fakeOp) {
	if fo.op.State != OperationStateInProgress {
		return
	}
	fo.remaining--
	if fo.remaining > 0 {
		return
	}

	if fo.fail {
		fo.op.State = OperationStateFailed
		fo.op.Error = "simulated OVN/DB failure"
		// Roll back a failed create; retain rows for a failed delete.
		for _, id := range fo.createdIDs {
			delete(f.allocs, id)
		}

		return
	}

	fo.op.State = OperationStateSucceeded
	for _, id := range fo.deleteIDs {
		delete(f.allocs, id)
	}
}

// consumeFailNext returns and clears the FailNext knob.
func (f *LoggingFakeClient) consumeFailNext() bool {
	if !f.FailNext {
		return false
	}
	f.FailNext = false

	return true
}

func hasBoundingFilter(q *ListPodCIDRAllocationsQuery) bool {
	return len(q.PodCIDRAllocationIDs) > 0 ||
		q.ProjectID != "" ||
		q.VPCNetworkID != "" ||
		len(q.VPCPrefixReservationIDs) > 0 ||
		q.NetworkInterfaceID != "" ||
		q.DestinationCIDR != ""
}

//nolint:cyclop // flat intersection of independent optional filters
func matchesQuery(a *PodCIDRAllocation, q *ListPodCIDRAllocationsQuery) bool {
	if len(q.PodCIDRAllocationIDs) > 0 && !containsString(q.PodCIDRAllocationIDs, a.ID) {
		return false
	}
	if q.ProjectID != "" && a.Context.ProjectID != q.ProjectID {
		return false
	}
	if q.VPCNetworkID != "" && a.Context.VPCNetworkID != q.VPCNetworkID {
		return false
	}
	if q.Location != "" && a.Context.Location != q.Location {
		return false
	}
	if len(q.VPCPrefixReservationIDs) > 0 && !containsString(q.VPCPrefixReservationIDs, a.VPCPrefixReservationID) {
		return false
	}
	if q.NetworkInterfaceID != "" && a.NetworkInterfaceID != q.NetworkInterfaceID {
		return false
	}
	if q.DestinationCIDR != "" && a.DestinationCIDR != q.DestinationCIDR {
		return false
	}

	return true
}

func matchesOpQuery(op *Operation, q *ListPodCIDRAllocationOperationsQuery) bool {
	if len(q.OperationIDs) > 0 && !containsString(q.OperationIDs, op.OperationID) {
		return false
	}
	if len(q.OperationStates) > 0 && !containsState(q.OperationStates, op.State) {
		return false
	}
	if q.PodCIDRAllocationID != "" && !containsString(op.AllocationIDs, q.PodCIDRAllocationID) {
		return false
	}

	return true
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}

	return false
}

func containsState(haystack []OperationState, needle OperationState) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}

	return false
}
