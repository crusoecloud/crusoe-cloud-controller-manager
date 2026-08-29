package routes

import (
	"context"
	"fmt"
	"testing"
	"time"

	mock_client "github.com/crusoecloud/crusoe-cloud-controller-manager/internal/client/mock"
	"github.com/crusoecloud/crusoe-cloud-controller-manager/internal/routes/sdn"
	"github.com/golang/mock/gomock"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

func reaperConfig() *Config {
	cfg := rcConfig()
	cfg.ReaperGrace = 10 * time.Minute

	return cfg
}

func seedAlloc(f *sdn.LoggingFakeClient, id, cidr, nic string, age time.Duration) {
	f.SeedAllocation(&sdn.PodCIDRAllocation{
		ID:                     id,
		VPCPrefixReservationID: rcRsv,
		NetworkInterfaceID:     nic,
		DestinationCIDR:        cidr,
		Context:                sdn.PodCIDRAllocationContext{ProjectID: rcProject, VPCNetworkID: rcVPC, Location: rcLoc},
		CreatedAt:              time.Now().Add(-age),
	})
}

func TestReaper_OrphanPastGraceDeleted(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m := mock_client.NewMockApiClient(ctrl)
	mockInstance(m)

	fakeSDN := sdn.NewLoggingFakeClient()
	// Orphan: no CiliumNode desires this cidr, and it is older than the grace.
	seedAlloc(fakeSDN, "al-orphan", "10.200.0.0/24", "nic-dead", time.Hour)

	h := newReconcileHarness(reaperConfig(), k8sfake.NewSimpleClientset(), fakeSDN, m)
	// No CiliumNodes -> nothing desired.

	h.controller.reapOnce(context.Background())
	// The delete op is async; drive one poll cycle so it resolves.
	drainAllOps(t, fakeSDN)

	if len(listAll(t, fakeSDN)) != 0 {
		t.Fatalf("orphan past grace should be deleted")
	}
}

func TestReaper_YoungOrphanSpared(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m := mock_client.NewMockApiClient(ctrl)
	mockInstance(m)

	fakeSDN := sdn.NewLoggingFakeClient()
	// Young orphan within grace.
	seedAlloc(fakeSDN, "al-young", "10.200.0.0/24", "nic-dead", time.Minute)

	h := newReconcileHarness(reaperConfig(), k8sfake.NewSimpleClientset(), fakeSDN, m)
	h.controller.reapOnce(context.Background())
	drainAllOps(t, fakeSDN)

	if len(listAll(t, fakeSDN)) != 1 {
		t.Fatalf("young orphan within grace should be spared")
	}
}

func TestReaper_NICMismatchDeletedAndEnqueued(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m := mock_client.NewMockApiClient(ctrl)
	mockInstance(m)

	fakeSDN := sdn.NewLoggingFakeClient()
	// Allocation for our cidr but a stale NIC (node replaced, /24 reused).
	seedAlloc(fakeSDN, "al-stale", rcCIDR, "nic-stale", time.Hour)

	node := taintedNode()
	h := newReconcileHarness(reaperConfig(), k8sfake.NewSimpleClientset(node), fakeSDN, m)
	seed(t, h, ciliumNodeObj(rcCIDR), node)

	h.controller.reapOnce(context.Background())
	drainAllOps(t, fakeSDN)

	if len(listAll(t, fakeSDN)) != 0 {
		t.Fatalf("NIC-mismatched allocation should be deleted")
	}
}

func TestReaper_MissingEnqueued(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m := mock_client.NewMockApiClient(ctrl)
	mockInstance(m)

	fakeSDN := sdn.NewLoggingFakeClient()
	// Desired but no actual allocation -> the node should be enqueued and, when
	// reconciled, an allocation is created.
	node := taintedNode()
	h := newReconcileHarness(reaperConfig(), k8sfake.NewSimpleClientset(node), fakeSDN, m)
	seed(t, h, ciliumNodeObj(rcCIDR), node)

	h.controller.reapOnce(context.Background())

	// The reaper enqueued the node; reconcile it and confirm a create happened.
	if _, err := h.controller.reconcile(context.Background(), rcNode); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(listAll(t, fakeSDN)) != 1 {
		t.Fatalf("missing allocation should be created via the reconcile path")
	}
}

func TestReaper_ChunkingOver50(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m := mock_client.NewMockApiClient(ctrl)
	mockInstance(m)

	fakeSDN := sdn.NewLoggingFakeClient()
	// 120 orphans past grace -> must be deleted across chunks of <=50.
	for i := range 120 {
		seedAlloc(fakeSDN, fmt.Sprintf("al-%03d", i), fmt.Sprintf("10.201.%d.0/24", i), "nic-dead", time.Hour)
	}

	h := newReconcileHarness(reaperConfig(), k8sfake.NewSimpleClientset(), fakeSDN, m)
	h.controller.reapOnce(context.Background())
	drainAllOps(t, fakeSDN)

	if got := len(listAll(t, fakeSDN)); got != 0 {
		t.Fatalf("all orphans should be deleted across chunks, %d remain", got)
	}
}

// drainAllOps polls every operation to terminal so pending deletes/creates
// commit.
func drainAllOps(t *testing.T, f *sdn.LoggingFakeClient) {
	t.Helper()
	for range 5 {
		if _, err := f.ListPodCIDRAllocationOperations(context.Background(),
			sdn.ListPodCIDRAllocationOperationsQuery{OperationStates: []sdn.OperationState{
				sdn.OperationStateInProgress,
			}}); err != nil {
			t.Fatalf("drain ops: %v", err)
		}
	}
}
