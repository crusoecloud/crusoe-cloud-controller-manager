package sdn

import (
	"context"
	"crypto/tls"
	"fmt"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/anypb"

	component "gitlab.com/crusoeenergy/schemas/api/island/v2/component"
	region "gitlab.com/crusoeenergy/schemas/api/island/v2/region"
	"gitlab.com/crusoeenergy/schemas/utils/rpc"
	grpcutil "gitlab.com/crusoeenergy/schemas/utils/rpc/grpc"
)

// errorInfoDomain is the google.rpc.ErrorInfo domain the region SDN service
// stamps on its typed FAILED_PRECONDITION errors. Callers branch on the
// ErrorInfo reason within this domain, not on the bare status code.
const errorInfoDomain = "region.island.v2"

// reasonDestinationConflict is the google.rpc.ErrorInfo reason (within
// errorInfoDomain) that flags a destination_cidr held by a DIFFERENT interface.
const reasonDestinationConflict = "DESTINATION_ALLOCATED_TO_ANOTHER_INTERFACE"

// GRPCClient is the production PodCIDRAllocationClient. It is the ONLY file in
// this package that touches the generated pb/grpc types: every method builds a
// pb request from a seam struct, invokes the region client, maps the pb
// response back to seam structs, and routes every error through mapError. The
// state machine, tracker, reaper and tests stay pb-free.
type GRPCClient struct {
	conn         grpcutil.ClientConn
	alloc        region.PodCIDRAllocationManagementClient
	reservations region.VPCPrefixReservationManagementClient
}

var _ PodCIDRAllocationClient = (*GRPCClient)(nil)

// NewGRPCClient dials the region SDN endpoint following the kubernetes-manager
// convention: an empty cert/key/ca trio yields a plaintext connection (local
// dev), a full trio yields mTLS. The grpcutil conn factory supplies the shared
// KM client conventions for free (20MB max message size, otel interceptors,
// round_robin balancing, keepalive).
func NewGRPCClient(endpoint, certFile, keyFile, caFile string) (*GRPCClient, error) {
	var tlsConfig *tls.Config
	if certFile != "" || keyFile != "" || caFile != "" {
		cfg, err := rpc.SetupMTLS(rpc.AuthInfo{CertFile: certFile, KeyFile: keyFile, CAFile: caFile})
		if err != nil {
			return nil, fmt.Errorf("setting up SDN mTLS: %w", err)
		}
		tlsConfig = cfg
	}

	conn, err := grpcutil.NewClientConnFactory(tlsConfig).NewClientConn(endpoint)
	if err != nil {
		return nil, fmt.Errorf("dialing SDN endpoint %q: %w", endpoint, err)
	}

	return &GRPCClient{
		conn:         conn,
		alloc:        region.NewPodCIDRAllocationManagementClient(conn),
		reservations: region.NewVPCPrefixReservationManagementClient(conn),
	}, nil
}

// Close tears down the underlying gRPC connection.
func (c *GRPCClient) Close() error {
	if err := c.conn.Close(); err != nil {
		return fmt.Errorf("closing SDN connection: %w", err)
	}

	return nil
}

// CreatePodCIDRAllocations issues the batch create RPC and returns the covering
// operation (IN_PROGRESS on success).
func (c *GRPCClient) CreatePodCIDRAllocations(
	ctx context.Context, req CreatePodCIDRAllocationsRequest,
) (*Operation, error) {
	specs := make([]*region.PodCIDRAllocationSpec, 0, len(req.Allocations))
	for i := range req.Allocations {
		specs = append(specs, &region.PodCIDRAllocationSpec{
			VpcPrefixReservationId: req.Allocations[i].VPCPrefixReservationID,
			NetworkInterfaceId:     req.Allocations[i].NetworkInterfaceID,
			DestinationCidr:        req.Allocations[i].DestinationCIDR,
		})
	}

	resp, err := c.alloc.CreatePodCIDRAllocations(ctx, &region.CreatePodCIDRAllocationsRequest{
		Allocations: specs,
		Context:     pbContext(req.Context),
	})
	if err != nil {
		return nil, mapError(err)
	}

	return operationFromPB(resp.GetOperation()), nil
}

// DeletePodCIDRAllocations issues the batch delete RPC. The server does not yet
// implement this RPC (codes.Unimplemented maps to a plain wrapped error); the
// reaper stays behind its in-code kill switch until it does.
func (c *GRPCClient) DeletePodCIDRAllocations(
	ctx context.Context, req DeletePodCIDRAllocationsRequest,
) (*Operation, error) {
	resp, err := c.alloc.DeletePodCIDRAllocations(ctx, &region.DeletePodCIDRAllocationsRequest{
		Ids:     req.IDs,
		Context: pbContext(req.Context),
	})
	if err != nil {
		return nil, mapError(err)
	}

	return operationFromPB(resp.GetOperation()), nil
}

// ListPodCIDRAllocations returns the allocations matching the query.
//
//nolint:gocritic // q is passed by value to match the client interface contract
func (c *GRPCClient) ListPodCIDRAllocations(
	ctx context.Context, q ListPodCIDRAllocationsQuery,
) ([]PodCIDRAllocation, error) {
	resp, err := c.alloc.ListPodCIDRAllocations(ctx, &region.ListPodCIDRAllocationsRequest{
		Query: &region.ListPodCIDRAllocationsQuery{
			PodCidrAllocationIds:    q.PodCIDRAllocationIDs,
			ProjectId:               q.ProjectID,
			VpcNetworkId:            q.VPCNetworkID,
			Location:                q.Location,
			VpcPrefixReservationIds: q.VPCPrefixReservationIDs,
			NetworkInterfaceId:      q.NetworkInterfaceID,
			DestinationCidr:         q.DestinationCIDR,
		},
	})
	if err != nil {
		return nil, mapError(err)
	}

	out := make([]PodCIDRAllocation, 0, len(resp.GetPodCidrAllocations()))
	for _, a := range resp.GetPodCidrAllocations() {
		out = append(out, allocationFromPB(a))
	}

	return out, nil
}

// ListPodCIDRAllocationOperations returns the operations matching the query.
//
//nolint:gocritic // q is passed by value to match the client interface contract
func (c *GRPCClient) ListPodCIDRAllocationOperations(
	ctx context.Context, q ListPodCIDRAllocationOperationsQuery,
) ([]Operation, error) {
	states := make([]component.OperationState, 0, len(q.OperationStates))
	for _, s := range q.OperationStates {
		states = append(states, pbOperationState(s))
	}

	resp, err := c.alloc.ListPodCIDRAllocationOperations(ctx, &region.ListPodCIDRAllocationOperationsRequest{
		Query: &region.ListPodCIDRAllocationOperationsQuery{
			OperationIds:        q.OperationIDs,
			ProjectIds:          q.ProjectIDs,
			PodCidrAllocationId: q.PodCIDRAllocationID,
			OperationStates:     states,
		},
	})
	if err != nil {
		return nil, mapError(err)
	}

	out := make([]Operation, 0, len(resp.GetOperations()))
	for _, op := range resp.GetOperations() {
		out = append(out, *operationFromPB(op))
	}

	return out, nil
}

// ListVPCPrefixReservations returns the reservations with the given ids.
func (c *GRPCClient) ListVPCPrefixReservations(
	ctx context.Context, ids []string,
) ([]VPCPrefixReservation, error) {
	resp, err := c.reservations.ListVPCPrefixReservations(ctx, &region.ListVPCPrefixReservationsRequest{
		Query: &region.ListVPCPrefixReservationsQuery{Ids: ids},
	})
	if err != nil {
		return nil, mapError(err)
	}

	out := make([]VPCPrefixReservation, 0, len(resp.GetVpcPrefixReservations()))
	for _, r := range resp.GetVpcPrefixReservations() {
		out = append(out, VPCPrefixReservation{ID: r.GetId(), Prefix: r.GetCidr()})
	}

	return out, nil
}

// pbContext converts a seam context to its pb form.
func pbContext(c PodCIDRAllocationContext) *component.PodCIDRAllocationContext {
	return &component.PodCIDRAllocationContext{
		ProjectId:    c.ProjectID,
		VpcNetworkId: c.VPCNetworkID,
		Location:     c.Location,
	}
}

// allocationFromPB maps a pb allocation to its seam form.
func allocationFromPB(a *component.PodCIDRAllocation) PodCIDRAllocation {
	out := PodCIDRAllocation{
		ID:                     a.GetId(),
		VPCPrefixReservationID: a.GetVpcPrefixReservationId(),
		NetworkInterfaceID:     a.GetNetworkInterfaceId(),
		DestinationCIDR:        a.GetDestinationCidr(),
		NextHopIP:              a.GetNextHopIp(),
	}
	if ctx := a.GetContext(); ctx != nil {
		out.Context = PodCIDRAllocationContext{
			ProjectID:    ctx.GetProjectId(),
			VPCNetworkID: ctx.GetVpcNetworkId(),
			Location:     ctx.GetLocation(),
		}
	}
	if ts := a.GetCreatedAt(); ts != nil {
		out.CreatedAt = ts.AsTime()
	}

	return out
}

// operationFromPB maps a pb operation to its seam form. AllocationIDs comes from
// the operation's BulkOperationMetadata.resource_ids (the batch these ops act
// on); the failure message comes from the error status detail.
func operationFromPB(op *component.Operation) *Operation {
	if op == nil {
		return nil
	}
	out := &Operation{
		OperationID:   op.GetOperationId(),
		State:         seamOperationState(op.GetOperationState()),
		AllocationIDs: allocationIDsFromMetadata(op.GetMetadata()),
	}
	if st := op.GetError(); st != nil {
		out.Error = st.GetMessage()
	}

	return out
}

// allocationIDsFromMetadata unpacks the operation's Any metadata to recover the
// allocation ids the operation touches. It tolerates absent/unexpected metadata
// (returns nil) since the reconcile path only reads ids on SUCCEEDED.
func allocationIDsFromMetadata(meta *anypb.Any) []string {
	if meta == nil {
		return nil
	}
	var bulk component.BulkOperationMetadata
	if err := meta.UnmarshalTo(&bulk); err == nil && len(bulk.GetResourceIds()) > 0 {
		return bulk.GetResourceIds()
	}
	var single component.OperationMetadata
	if err := meta.UnmarshalTo(&single); err == nil && single.GetResourceId() != "" {
		return []string{single.GetResourceId()}
	}

	return nil
}

// seamOperationState maps a pb OperationState to the seam enum. UNSPECIFIED and
// any unknown value collapse to IN_PROGRESS (not-yet-terminal is the safe
// default for the tracker).
func seamOperationState(s component.OperationState) OperationState {
	switch s {
	case component.OperationState_OPERATION_STATE_SUCCEEDED:
		return OperationStateSucceeded
	case component.OperationState_OPERATION_STATE_FAILED:
		return OperationStateFailed
	case component.OperationState_OPERATION_STATE_IN_PROGRESS,
		component.OperationState_OPERATION_STATE_UNSPECIFIED:
		return OperationStateInProgress
	default:
		return OperationStateInProgress
	}
}

// pbOperationState maps a seam OperationState to its pb enum.
func pbOperationState(s OperationState) component.OperationState {
	switch s {
	case OperationStateSucceeded:
		return component.OperationState_OPERATION_STATE_SUCCEEDED
	case OperationStateFailed:
		return component.OperationState_OPERATION_STATE_FAILED
	case OperationStateInProgress:
		return component.OperationState_OPERATION_STATE_IN_PROGRESS
	default:
		return component.OperationState_OPERATION_STATE_UNSPECIFIED
	}
}

// mapError translates a gRPC status error into a seam sentinel. Callers branch
// on FAILED_PRECONDITION by the google.rpc.ErrorInfo reason, not the bare code:
// the code is also the unclassified bucket (e.g. location not ACTIVE carries no
// ErrorInfo and must NOT map to a conflict).
func mapError(err error) error {
	if err == nil {
		return nil
	}

	st := status.Convert(err)
	//nolint:exhaustive // the default arm covers every unlisted code intentionally
	switch st.Code() {
	case codes.FailedPrecondition:
		if isDestinationConflict(st) {
			return fmt.Errorf("%w: %s", ErrDestinationConflict, st.Message())
		}

		return fmt.Errorf("SDN failed precondition: %w", err)
	case codes.InvalidArgument:
		return fmt.Errorf("%w: %s", ErrInvalidArgument, st.Message())
	case codes.NotFound:
		return fmt.Errorf("%w: %s", ErrNotFound, st.Message())
	case codes.Unavailable, codes.Aborted:
		return fmt.Errorf("%w: %s", ErrUnavailable, st.Message())
	case codes.Unimplemented:
		return fmt.Errorf("SDN RPC not yet served by the region server: %w", err)
	default:
		return fmt.Errorf("SDN RPC failed: %w", err)
	}
}

// isDestinationConflict reports whether the status carries the region service's
// DESTINATION_ALLOCATED_TO_ANOTHER_INTERFACE ErrorInfo.
func isDestinationConflict(st *status.Status) bool {
	for _, d := range st.Details() {
		info, ok := d.(*errdetails.ErrorInfo)
		if ok && info.GetReason() == reasonDestinationConflict && info.GetDomain() == errorInfoDomain {
			return true
		}
	}

	return false
}
