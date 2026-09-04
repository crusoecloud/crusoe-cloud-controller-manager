package routes

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	crusoeapi "github.com/crusoecloud/client-go/swagger/v1alpha5"
	mock_client "github.com/crusoecloud/crusoe-cloud-controller-manager/internal/client/mock"
	"github.com/crusoecloud/crusoe-cloud-controller-manager/internal/routes/sdn"
	"github.com/golang/mock/gomock"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	cloudprovider "k8s.io/cloud-provider"
)

const (
	rtCIDR  = "10.0.0.0/24"
	rtRsvID = "rsv-1"
)

// nodeListerFrom builds a NodeLister backed by an indexer seeded with nodes.
func nodeListerFrom(t *testing.T, nodes ...*v1.Node) corev1listers.NodeLister {
	t.Helper()
	idx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	for _, n := range nodes {
		if err := idx.Add(n); err != nil {
			t.Fatalf("seeding node lister: %v", err)
		}
	}

	return corev1listers.NewNodeLister(idx)
}

func rtNode(instanceID string) *v1.Node {
	n := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: nicNodeName}}
	n.Spec.ProviderID = "crusoe://" + instanceID

	return n
}

// allocsForCIDR lists allocations for cidr across the given reservations,
// failing the test on error.
func allocsForCIDR(t *testing.T, fake *sdn.LoggingFakeClient, cidr string, rsvIDs ...string) []sdn.PodCIDRAllocation {
	t.Helper()
	allocs, err := fake.ListPodCIDRAllocations(context.Background(),
		sdn.ListPodCIDRAllocationsQuery{
			ProjectID: nicProjectID, VPCNetworkID: nicVPCID,
			DestinationCIDR: cidr, VPCPrefixReservationIDs: rsvIDs,
		})
	if err != nil {
		t.Fatalf("listing allocations: %v", err)
	}

	return allocs
}

func rtContext() sdn.PodCIDRAllocationContext {
	return sdn.PodCIDRAllocationContext{ProjectID: nicProjectID, VPCNetworkID: nicVPCID, Location: nicLocation}
}

func rtConfig() *Config {
	return &Config{
		ProjectID:               nicProjectID,
		VPCID:                   nicVPCID,
		Location:                nicLocation,
		SDNLocation:             nicLocation, // the fake resolves locations to themselves
		VPCPrefixReservationIDs: []string{rtRsvID},
	}
}

// expectResolveNIC primes the mock to resolve nodeName to nicMatchingID via
// GetInstanceByID; call .Times(n) on the returned call to bound it.
func expectResolveNIC(m *mock_client.MockApiClient, instanceID string) *gomock.Call {
	inst := instanceWith([]crusoeapi.NetworkInterface{{Id: nicMatchingID, Network: nicVPCID}})
	inst.Id = instanceID

	return m.EXPECT().GetInstanceByID(gomock.Any(), instanceID).Return(inst, nil, nil)
}

func newCloudRoutes(cfg *Config, fake *sdn.LoggingFakeClient, m *mock_client.MockApiClient) *CloudRoutes {
	return NewCloudRoutes(cfg, fake, m)
}

// --- ListRoutes ---

func TestListRoutes_MapsAllocationToNode(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m := mock_client.NewMockApiClient(ctrl)
	expectResolveNIC(m, nicInstanceID).Times(1)

	fake := sdn.NewLoggingFakeClient()
	fake.SeedAllocation(&sdn.PodCIDRAllocation{
		ID: "al-1", NetworkInterfaceID: nicMatchingID, DestinationCIDR: rtCIDR,
		VPCPrefixReservationID: rtRsvID, Context: rtContext(),
	})

	r := newCloudRoutes(rtConfig(), fake, m)
	r.SetNodeLister(nodeListerFrom(t, rtNode(nicInstanceID)))

	routes, err := r.ListRoutes(context.Background(), "cluster")
	if err != nil {
		t.Fatalf("ListRoutes: %v", err)
	}
	if len(routes) != 1 {
		t.Fatalf("want 1 route, got %d", len(routes))
	}
	got := routes[0]
	if got.Name != "al-1" || string(got.TargetNode) != nicNodeName || got.DestinationCIDR != rtCIDR {
		t.Fatalf("unexpected route %+v", got)
	}
}

func TestListRoutes_UnknownNICGetsEmptyTargetNode(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m := mock_client.NewMockApiClient(ctrl)
	expectResolveNIC(m, nicInstanceID).Times(1)

	fake := sdn.NewLoggingFakeClient()
	// Allocation on a NIC no live node owns -> orphan.
	fake.SeedAllocation(&sdn.PodCIDRAllocation{
		ID: "al-orphan", NetworkInterfaceID: "nic-dead", DestinationCIDR: rtCIDR,
		VPCPrefixReservationID: rtRsvID, Context: rtContext(),
	})

	r := newCloudRoutes(rtConfig(), fake, m)
	r.SetNodeLister(nodeListerFrom(t, rtNode(nicInstanceID)))

	routes, err := r.ListRoutes(context.Background(), "cluster")
	if err != nil {
		t.Fatalf("ListRoutes: %v", err)
	}
	if len(routes) != 1 || routes[0].TargetNode != "" {
		t.Fatalf("want orphan route with empty TargetNode, got %+v", routes)
	}
}

// expectResolveNICFails primes both lookups to fail, so resolveNIC errors.
func expectResolveNICFails(m *mock_client.MockApiClient) {
	m.EXPECT().GetInstanceByID(gomock.Any(), nicInstanceID).Return(nil, nil, errors.New("boom"))
	m.EXPECT().GetInstanceByName(gomock.Any(), nicNodeName).Return(nil, errors.New("boom"))
}

func TestListRoutes_UnresolvableNICDoesNotFailThePass(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m := mock_client.NewMockApiClient(ctrl)
	expectResolveNICFails(m)

	r := newCloudRoutes(rtConfig(), sdn.NewLoggingFakeClient(), m)
	r.SetNodeLister(nodeListerFrom(t, rtNode(nicInstanceID)))

	routes, err := r.ListRoutes(context.Background(), "cluster")
	if err != nil {
		t.Fatalf("ListRoutes must not fail on an unresolvable NIC: %v", err)
	}
	if len(routes) != 0 {
		t.Fatalf("want no routes, got %+v", routes)
	}
}

func TestListRoutes_UnresolvableNICAttributedByPodCIDR(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m := mock_client.NewMockApiClient(ctrl)
	expectResolveNICFails(m)

	fake := sdn.NewLoggingFakeClient()
	// The allocation's NIC is unknown (resolution failed), so only the node's
	// pod cidr can attribute it, which protects it from orphan GC.
	fake.SeedAllocation(&sdn.PodCIDRAllocation{
		ID: "al-1", NetworkInterfaceID: nicMatchingID, DestinationCIDR: rtCIDR,
		VPCPrefixReservationID: rtRsvID, Context: rtContext(),
	})

	node := rtNode(nicInstanceID)
	node.Spec.PodCIDRs = []string{rtCIDR}

	r := newCloudRoutes(rtConfig(), fake, m)
	r.SetNodeLister(nodeListerFrom(t, node))

	routes, err := r.ListRoutes(context.Background(), "cluster")
	if err != nil {
		t.Fatalf("ListRoutes: %v", err)
	}
	if len(routes) != 1 || string(routes[0].TargetNode) != nicNodeName {
		t.Fatalf("want the allocation attributed to %s, got %+v", nicNodeName, routes)
	}
}

func TestListRoutes_EvictsCacheForDeletedNodes(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m := mock_client.NewMockApiClient(ctrl)
	// Two resolves: the second pass re-resolves because the eviction in between
	// dropped the cache entry for the (briefly) absent node.
	expectResolveNIC(m, nicInstanceID).Times(2)

	r := newCloudRoutes(rtConfig(), sdn.NewLoggingFakeClient(), m)

	r.SetNodeLister(nodeListerFrom(t, rtNode(nicInstanceID)))
	if _, err := r.ListRoutes(context.Background(), "cluster"); err != nil {
		t.Fatalf("ListRoutes pass 1: %v", err)
	}

	r.SetNodeLister(nodeListerFrom(t))
	if _, err := r.ListRoutes(context.Background(), "cluster"); err != nil {
		t.Fatalf("ListRoutes pass 2: %v", err)
	}
	if len(r.nicByNode) != 0 {
		t.Fatalf("want the deleted node evicted, got %+v", r.nicByNode)
	}

	r.SetNodeLister(nodeListerFrom(t, rtNode(nicInstanceID)))
	if _, err := r.ListRoutes(context.Background(), "cluster"); err != nil {
		t.Fatalf("ListRoutes pass 3: %v", err)
	}
}

func TestListRoutes_SteadyStateZeroAPICalls(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m := mock_client.NewMockApiClient(ctrl)
	// Exactly one resolve across two passes: the cache serves the second.
	expectResolveNIC(m, nicInstanceID).Times(1)

	fake := sdn.NewLoggingFakeClient()
	fake.SeedAllocation(&sdn.PodCIDRAllocation{
		ID: "al-1", NetworkInterfaceID: nicMatchingID, DestinationCIDR: rtCIDR,
		VPCPrefixReservationID: rtRsvID, Context: rtContext(),
	})

	r := newCloudRoutes(rtConfig(), fake, m)
	r.SetNodeLister(nodeListerFrom(t, rtNode(nicInstanceID)))

	for range 2 {
		if _, err := r.ListRoutes(context.Background(), "cluster"); err != nil {
			t.Fatalf("ListRoutes: %v", err)
		}
	}
}

func TestListRoutes_ReresolvesOnInstanceIDChange(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m := mock_client.NewMockApiClient(ctrl)
	otherInstanceID := "99999999-2222-3333-4444-555555555555"
	// First pass resolves the original instance; second pass sees a new instance
	// id under the same node name and re-resolves.
	gomock.InOrder(
		expectResolveNIC(m, nicInstanceID),
		expectResolveNIC(m, otherInstanceID),
	)

	fake := sdn.NewLoggingFakeClient()
	r := newCloudRoutes(rtConfig(), fake, m)

	r.SetNodeLister(nodeListerFrom(t, rtNode(nicInstanceID)))
	if _, err := r.ListRoutes(context.Background(), "cluster"); err != nil {
		t.Fatalf("ListRoutes pass 1: %v", err)
	}

	r.SetNodeLister(nodeListerFrom(t, rtNode(otherInstanceID)))
	if _, err := r.ListRoutes(context.Background(), "cluster"); err != nil {
		t.Fatalf("ListRoutes pass 2: %v", err)
	}
}

// --- CreateRoute ---

func TestCreateRoute_HappyPath(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m := mock_client.NewMockApiClient(ctrl)
	expectResolveNIC(m, nicInstanceID).Times(1)

	fake := sdn.NewLoggingFakeClient() // PendingPolls default 1 -> op SUCCEEDS on first poll
	r := newCloudRoutes(rtConfig(), fake, m)
	r.SetNodeLister(nodeListerFrom(t, rtNode(nicInstanceID)))

	route := &cloudprovider.Route{TargetNode: nicNodeName, DestinationCIDR: rtCIDR}
	if err := r.CreateRoute(context.Background(), "cluster", "hint", route); err != nil {
		t.Fatalf("CreateRoute: %v", err)
	}

	allocs := allocsForCIDR(t, fake, rtCIDR, rtRsvID)
	if len(allocs) != 1 || allocs[0].NetworkInterfaceID != nicMatchingID {
		t.Fatalf("want 1 allocation on our NIC, got %+v", allocs)
	}
}

func TestCreateRoute_AdoptsExistingOurs(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m := mock_client.NewMockApiClient(ctrl)
	expectResolveNIC(m, nicInstanceID).Times(1)

	fake := sdn.NewLoggingFakeClient()
	fake.SeedAllocation(&sdn.PodCIDRAllocation{
		ID: "al-pre", NetworkInterfaceID: nicMatchingID, DestinationCIDR: rtCIDR,
		VPCPrefixReservationID: rtRsvID, Context: rtContext(),
	})

	r := newCloudRoutes(rtConfig(), fake, m)
	r.SetNodeLister(nodeListerFrom(t, rtNode(nicInstanceID)))

	route := &cloudprovider.Route{TargetNode: nicNodeName, DestinationCIDR: rtCIDR}
	if err := r.CreateRoute(context.Background(), "cluster", "hint", route); err != nil {
		t.Fatalf("CreateRoute adopt: %v", err)
	}

	// Adopted without a create: still exactly one row, the pre-existing one.
	allocs := allocsForCIDR(t, fake, rtCIDR, rtRsvID)
	if len(allocs) != 1 || allocs[0].ID != "al-pre" {
		t.Fatalf("want adopted al-pre, got %+v", allocs)
	}
}

func TestCreateRoute_ConflictDifferentNIC(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m := mock_client.NewMockApiClient(ctrl)
	expectResolveNIC(m, nicInstanceID).Times(1)

	fake := sdn.NewLoggingFakeClient()
	fake.SeedAllocation(&sdn.PodCIDRAllocation{
		ID: "al-other", NetworkInterfaceID: "nic-someone-else", DestinationCIDR: rtCIDR,
		VPCPrefixReservationID: rtRsvID, Context: rtContext(),
	})

	r := newCloudRoutes(rtConfig(), fake, m)
	r.SetNodeLister(nodeListerFrom(t, rtNode(nicInstanceID)))

	route := &cloudprovider.Route{TargetNode: nicNodeName, DestinationCIDR: rtCIDR}
	err := r.CreateRoute(context.Background(), "cluster", "hint", route)
	if !errors.Is(err, sdn.ErrDestinationConflict) {
		t.Fatalf("want ErrDestinationConflict, got %v", err)
	}
}

func TestCreateRoute_FailNextThenAdopt(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m := mock_client.NewMockApiClient(ctrl)
	expectResolveNIC(m, nicInstanceID).Times(1) // cached across both calls

	fake := sdn.NewLoggingFakeClient()
	fake.FailNext = true // the create op resolves FAILED; the row survives

	r := newCloudRoutes(rtConfig(), fake, m)
	r.SetNodeLister(nodeListerFrom(t, rtNode(nicInstanceID)))
	route := &cloudprovider.Route{TargetNode: nicNodeName, DestinationCIDR: rtCIDR}

	if err := r.CreateRoute(context.Background(), "cluster", "hint", route); err == nil {
		t.Fatalf("want error on FAILED op")
	}

	// Next pass adopts the surviving row (same NIC), no error, no second row.
	if err := r.CreateRoute(context.Background(), "cluster", "hint", route); err != nil {
		t.Fatalf("second CreateRoute should adopt: %v", err)
	}
	allocs := allocsForCIDR(t, fake, rtCIDR, rtRsvID)
	if len(allocs) != 1 {
		t.Fatalf("want single surviving row, got %+v", allocs)
	}
}

func TestCreateRoute_ConflictKnob(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m := mock_client.NewMockApiClient(ctrl)
	expectResolveNIC(m, nicInstanceID).Times(1)

	fake := sdn.NewLoggingFakeClient()
	fake.ConflictCIDRs[rtCIDR] = true // create returns ErrDestinationConflict

	r := newCloudRoutes(rtConfig(), fake, m)
	r.SetNodeLister(nodeListerFrom(t, rtNode(nicInstanceID)))
	route := &cloudprovider.Route{TargetNode: nicNodeName, DestinationCIDR: rtCIDR}

	if err := r.CreateRoute(context.Background(), "cluster", "hint", route); !errors.Is(err, sdn.ErrDestinationConflict) {
		t.Fatalf("want ErrDestinationConflict from create, got %v", err)
	}
}

func TestCreateRoute_PollTimeout(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m := mock_client.NewMockApiClient(ctrl)
	expectResolveNIC(m, nicInstanceID).Times(1)

	fake := sdn.NewLoggingFakeClient()
	fake.PendingPolls = 1_000_000 // never resolves within our test ctx

	r := newCloudRoutes(rtConfig(), fake, m)
	r.SetNodeLister(nodeListerFrom(t, rtNode(nicInstanceID)))
	route := &cloudprovider.Route{TargetNode: nicNodeName, DestinationCIDR: rtCIDR}

	// Cancel the context to force the poll to bail without waiting out
	// createPollTimeout.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.CreateRoute(ctx, "cluster", "hint", route); err == nil {
		t.Fatalf("want error when poll cannot complete")
	}
}

// countingReservationClient wraps the fake to count reservation list RPCs. The
// upstream controller fans creates out on goroutines, so the counter is guarded.
type countingReservationClient struct {
	*sdn.LoggingFakeClient

	mu    sync.Mutex
	calls int
}

func (c *countingReservationClient) ListVPCPrefixReservations(
	ctx context.Context, ids []string,
) ([]sdn.VPCPrefixReservation, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()

	rsvs, err := c.LoggingFakeClient.ListVPCPrefixReservations(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("counting fake: %w", err)
	}

	return rsvs, nil
}

func TestCreateRoute_MultiReservationContainment(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m := mock_client.NewMockApiClient(ctrl)
	expectResolveNIC(m, nicInstanceID).Times(1)

	fake := sdn.NewLoggingFakeClient()
	fake.Reservations = []sdn.VPCPrefixReservation{
		{ID: "rsv-a", Prefix: "10.0.0.0/16"},
		{ID: "rsv-b", Prefix: "10.1.0.0/16"}, // both create cidrs are contained here
	}
	counting := &countingReservationClient{LoggingFakeClient: fake}

	cfg := rtConfig()
	cfg.VPCPrefixReservationIDs = []string{"rsv-a", "rsv-b"}
	r := newCloudRoutes(cfg, fake, m)
	r.sdn = counting
	r.SetNodeLister(nodeListerFrom(t, rtNode(nicInstanceID)))

	for _, cidr := range []string{"10.1.2.0/24", "10.1.3.0/24"} {
		route := &cloudprovider.Route{TargetNode: nicNodeName, DestinationCIDR: cidr}
		if err := r.CreateRoute(context.Background(), "cluster", "hint", route); err != nil {
			t.Fatalf("CreateRoute multi-reservation (%s): %v", cidr, err)
		}
		allocs := allocsForCIDR(t, fake, cidr, "rsv-a", "rsv-b")
		if len(allocs) != 1 || allocs[0].VPCPrefixReservationID != "rsv-b" {
			t.Fatalf("want allocation for %s carved from rsv-b, got %+v", cidr, allocs)
		}
	}

	// Reservations are immutable: one list RPC serves both creates.
	if counting.calls != 1 {
		t.Fatalf("want 1 reservation list across two creates, got %d", counting.calls)
	}
}

// --- DeleteRoute ---

func TestDeleteRoute_IDPassthrough(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m := mock_client.NewMockApiClient(ctrl)

	fake := sdn.NewLoggingFakeClient()
	fake.SeedAllocation(&sdn.PodCIDRAllocation{
		ID: "al-del", NetworkInterfaceID: nicMatchingID, DestinationCIDR: rtCIDR,
		VPCPrefixReservationID: rtRsvID, Context: rtContext(),
	})

	r := newCloudRoutes(rtConfig(), fake, m)
	// Fake's Delete succeeds (returns op), id passthrough verified by the op
	// targeting al-del; no polling done by DeleteRoute.
	if err := r.DeleteRoute(context.Background(), "cluster",
		&cloudprovider.Route{Name: "al-del", DestinationCIDR: rtCIDR}); err != nil {
		t.Fatalf("DeleteRoute: %v", err)
	}
}

// errDeleteClient wraps the fake to make DeletePodCIDRAllocations return an
// Unimplemented-style error, mirroring the server kill switch (section 6.3).
type errDeleteClient struct {
	*sdn.LoggingFakeClient
}

func (e errDeleteClient) DeletePodCIDRAllocations(
	context.Context, sdn.DeletePodCIDRAllocationsRequest,
) (*sdn.Operation, error) {
	return nil, errors.New("rpc error: code = Unimplemented")
}

func TestDeleteRoute_UnimplementedSurfaces(t *testing.T) {
	t.Parallel()
	r := newCloudRoutes(rtConfig(), sdn.NewLoggingFakeClient(), nil)
	r.sdn = errDeleteClient{LoggingFakeClient: sdn.NewLoggingFakeClient()}

	if err := r.DeleteRoute(context.Background(), "cluster",
		&cloudprovider.Route{Name: "al-x"}); err == nil {
		t.Fatalf("want the server's Unimplemented error surfaced")
	}
}
