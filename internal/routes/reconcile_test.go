package routes_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	crusoeapi "github.com/crusoecloud/client-go/swagger/v1alpha5"
	mock_client "github.com/crusoecloud/crusoe-cloud-controller-manager/internal/client/mock"
	"github.com/crusoecloud/crusoe-cloud-controller-manager/internal/routes"
	"github.com/crusoecloud/crusoe-cloud-controller-manager/internal/routes/sdn"
	"github.com/golang/mock/gomock"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

const (
	rcNode    = "worker-0"
	rcCIDR    = "10.100.4.0/24"
	rcVPC     = "net-abc"
	rcRsv     = "rsv-pods"
	rcProject = "proj-123"
	rcLoc     = "us-east1-a"
	rcNIC     = "nic-vpc"
)

func rcConfig() *routes.Config {
	return &routes.Config{
		ProjectID:              rcProject,
		VPCID:                  rcVPC,
		VPCPrefixReservationID: rcRsv,
		Location:               rcLoc,
		PollInterval:           5 * time.Second,
	}
}

func ciliumNodeObj(cidrs ...string) *unstructured.Unstructured {
	slice := make([]any, len(cidrs))
	for i, c := range cidrs {
		slice[i] = c
	}

	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cilium.io/v2",
		"kind":       "CiliumNode",
		"metadata":   map[string]any{"name": rcNode},
		"spec":       map[string]any{"ipam": map[string]any{"podCIDRs": slice}},
	}}
}

// taintedNode returns a Node registered with the pods-unroutable taint.
func taintedNode() *v1.Node {
	n := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: rcNode}}
	n.Spec.ProviderID = "crusoe://inst-" + rcNode
	n.Spec.Taints = []v1.Taint{{Key: routes.PodsUnroutableTaintKey, Effect: v1.TaintEffectNoSchedule}}

	return n
}

// seed adds a CiliumNode and (optionally) a Node to the harness, failing on
// error.
func seed(t *testing.T, h *routes.ReconcileHarness, cn *unstructured.Unstructured, node *v1.Node) {
	t.Helper()
	if err := h.AddCiliumNode(cn); err != nil {
		t.Fatalf("add cilium node: %v", err)
	}
	if node != nil {
		if err := h.AddNode(node); err != nil {
			t.Fatalf("add node: %v", err)
		}
	}
}

// listAll lists every allocation in the reservation, failing on error.
func listAll(t *testing.T, f *sdn.LoggingFakeClient) []sdn.PodCIDRAllocation {
	t.Helper()
	allocs, err := f.ListPodCIDRAllocations(context.Background(),
		sdn.ListPodCIDRAllocationsQuery{VPCPrefixReservationIDs: []string{rcRsv}})
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	return allocs
}

// drainPreseed polls a preseed op to terminal so its allocation is committed.
func drainPreseed(t *testing.T, f *sdn.LoggingFakeClient, opID string) {
	t.Helper()
	for range 3 {
		if _, err := f.ListPodCIDRAllocationOperations(context.Background(),
			sdn.ListPodCIDRAllocationOperationsQuery{OperationIDs: []string{opID}}); err != nil {
			t.Fatalf("drain preseed: %v", err)
		}
	}
}

func mockInstance(m *mock_client.MockApiClient) {
	inst := &crusoeapi.InstanceV1Alpha5{
		Id:                "inst",
		Location:          rcLoc,
		ProjectId:         rcProject,
		NetworkInterfaces: []crusoeapi.NetworkInterface{{Id: rcNIC, Network: rcVPC}},
	}
	m.EXPECT().GetInstanceByID(gomock.Any(), gomock.Any()).Return(inst, nil, nil).AnyTimes()
	m.EXPECT().GetInstanceByName(gomock.Any(), gomock.Any()).Return(inst, nil).AnyTimes()
}

func getNode(t *testing.T, kube *k8sfake.Clientset) *v1.Node {
	t.Helper()
	n, err := kube.CoreV1().Nodes().Get(context.Background(), rcNode, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get node: %v", err)
	}

	return n
}

func hasUnroutableTaint(node *v1.Node) bool {
	for i := range node.Spec.Taints {
		if node.Spec.Taints[i].Key == routes.PodsUnroutableTaintKey {
			return true
		}
	}

	return false
}

// drainToReady runs reconcile + poll ticks until the node is finalized or the
// bound is hit, syncing the lister from the fake clientset after each step to
// emulate the informer.
func drainToReady(t *testing.T, h *routes.ReconcileHarness) {
	t.Helper()
	ctx := context.Background()
	for range 10 {
		if _, err := h.Reconcile(ctx, rcNode); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if err := h.SyncNodeFromClient(ctx, rcNode); err != nil {
			t.Fatalf("sync node: %v", err)
		}
		h.PollOpsOnce(ctx)
	}
}

func TestReconcile_HappyPath(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m := mock_client.NewMockApiClient(ctrl)
	mockInstance(m)

	node := taintedNode()
	kube := k8sfake.NewSimpleClientset(node)
	fakeSDN := sdn.NewLoggingFakeClient()

	h := routes.NewReconcileHarness(rcConfig(), kube, fakeSDN, m)
	if err := h.AddCiliumNode(ciliumNodeObj(rcCIDR)); err != nil {
		t.Fatalf("add cilium node: %v", err)
	}
	if err := h.AddNode(node); err != nil {
		t.Fatalf("add node: %v", err)
	}

	drainToReady(t, h)

	assertFinalized(t, getNode(t, kube))
}

// assertFinalized checks the invariants of a fully-reconciled node.
func assertFinalized(t *testing.T, final *v1.Node) {
	t.Helper()
	if hasUnroutableTaint(final) {
		t.Fatalf("taint should be removed after ready")
	}
	if final.Labels[routes.OpIDLabel] != "" {
		t.Fatalf("op-id label should be cleared, got %q", final.Labels[routes.OpIDLabel])
	}
	readyAt := final.Labels[routes.ReadyAtLabel]
	if readyAt == "" {
		t.Fatalf("ready-at label should be set")
	}
	if _, err := strconv.ParseInt(readyAt, 10, 64); err != nil {
		t.Fatalf("ready-at %q must parse as integer epoch seconds: %v", readyAt, err)
	}
	if errs := validation.IsValidLabelValue(readyAt); len(errs) != 0 {
		t.Fatalf("ready-at %q must be a valid label value: %v", readyAt, errs)
	}
	if !hasNetworkAvailableFalse(final) {
		t.Fatalf("NetworkUnavailable=False should be set")
	}
}

func hasNetworkAvailableFalse(node *v1.Node) bool {
	for _, cond := range node.Status.Conditions {
		if cond.Type == v1.NodeNetworkUnavailable {
			return cond.Status == v1.ConditionFalse
		}
	}

	return false
}

func TestReconcile_OpIDLabelWrittenBeforePoll(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m := mock_client.NewMockApiClient(ctrl)
	mockInstance(m)

	node := taintedNode()
	kube := k8sfake.NewSimpleClientset(node)
	fakeSDN := sdn.NewLoggingFakeClient()
	fakeSDN.PendingPolls = 3 // keep the op in flight

	h := routes.NewReconcileHarness(rcConfig(), kube, fakeSDN, m)
	seed(t, h, ciliumNodeObj(rcCIDR), node)

	// First reconcile: create + immediate op-id label patch, requeue.
	requeue, err := h.Reconcile(context.Background(), rcNode)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if requeue <= 0 {
		t.Fatalf("expected requeue while op in flight, got %v", requeue)
	}
	patched := getNode(t, kube)
	if patched.Labels[routes.OpIDLabel] == "" {
		t.Fatalf("op-id label must be patched immediately after create")
	}
	if !hasUnroutableTaint(patched) {
		t.Fatalf("taint must remain while op is in flight")
	}
}

func TestReconcile_EmptyPodCIDRsNoop(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m := mock_client.NewMockApiClient(ctrl)

	kube := k8sfake.NewSimpleClientset()
	h := routes.NewReconcileHarness(rcConfig(), kube, sdn.NewLoggingFakeClient(), m)
	seed(t, h, ciliumNodeObj(), nil) // no cidrs

	requeue, err := h.Reconcile(context.Background(), rcNode)
	if err != nil || requeue != 0 {
		t.Fatalf("expected no-op, got requeue=%v err=%v", requeue, err)
	}
}

func TestReconcile_CiliumNodeDeletedZeroSDNCalls(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m := mock_client.NewMockApiClient(ctrl)

	kube := k8sfake.NewSimpleClientset()
	// A recording SDN that fails the test if any method is called.
	strict := &strictNoCallSDN{t: t}
	h := routes.NewReconcileHarness(rcConfig(), kube, strict, m)
	// No CiliumNode added -> lister returns NotFound -> gone path.

	requeue, err := h.Reconcile(context.Background(), rcNode)
	if err != nil || requeue != 0 {
		t.Fatalf("deleted CiliumNode should be a clean no-op, got requeue=%v err=%v", requeue, err)
	}
}

func TestReconcile_MultiCIDRRoutesFirst(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m := mock_client.NewMockApiClient(ctrl)
	mockInstance(m)

	node := taintedNode()
	kube := k8sfake.NewSimpleClientset(node)
	fakeSDN := sdn.NewLoggingFakeClient()
	h := routes.NewReconcileHarness(rcConfig(), kube, fakeSDN, m)
	seed(t, h, ciliumNodeObj(rcCIDR, "10.100.5.0/24"), node)

	drainToReady(t, h)

	allocs, err := fakeSDN.ListPodCIDRAllocations(context.Background(),
		sdn.ListPodCIDRAllocationsQuery{VPCPrefixReservationIDs: []string{rcRsv}})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(allocs) != 1 || allocs[0].DestinationCIDR != rcCIDR {
		t.Fatalf("expected only first cidr routed, got %+v", allocs)
	}
}

func TestReconcile_FailNextClearsLabelAndErrors(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m := mock_client.NewMockApiClient(ctrl)
	mockInstance(m)

	node := taintedNode()
	kube := k8sfake.NewSimpleClientset(node)
	fakeSDN := sdn.NewLoggingFakeClient()
	fakeSDN.FailNext = true

	h := routes.NewReconcileHarness(rcConfig(), kube, fakeSDN, m)
	seed(t, h, ciliumNodeObj(rcCIDR), node)

	// Create (op-id patched), poll flips FAILED.
	if _, err := h.Reconcile(context.Background(), rcNode); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if err := h.SyncNodeFromClient(context.Background(), rcNode); err != nil {
		t.Fatalf("sync: %v", err)
	}
	h.PollOpsOnce(context.Background())

	// Next reconcile observes FAILED: clears label, returns error.
	if _, err := h.Reconcile(context.Background(), rcNode); err == nil {
		t.Fatalf("expected error on FAILED op")
	}
	got := getNode(t, kube)
	if got.Labels[routes.OpIDLabel] != "" {
		t.Fatalf("op-id label must be cleared after FAILED op")
	}
	if !hasUnroutableTaint(got) {
		t.Fatalf("taint must remain after FAILED op")
	}
}

func TestReconcile_ConflictKeepsTaint(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m := mock_client.NewMockApiClient(ctrl)
	mockInstance(m)

	node := taintedNode()
	kube := k8sfake.NewSimpleClientset(node)
	fakeSDN := sdn.NewLoggingFakeClient()
	fakeSDN.ConflictCIDRs[rcCIDR] = true

	h := routes.NewReconcileHarness(rcConfig(), kube, fakeSDN, m)
	seed(t, h, ciliumNodeObj(rcCIDR), node)

	if _, err := h.Reconcile(context.Background(), rcNode); err == nil {
		t.Fatalf("expected conflict error")
	}
	got := getNode(t, kube)
	if !hasUnroutableTaint(got) {
		t.Fatalf("taint must remain on conflict")
	}
	if got.Labels[routes.ReadyAtLabel] != "" {
		t.Fatalf("ready-at must not be set on conflict")
	}
}

func TestReconcile_AdoptExistingAllocationNoCreate(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m := mock_client.NewMockApiClient(ctrl)
	mockInstance(m)

	node := taintedNode()
	kube := k8sfake.NewSimpleClientset(node)
	fakeSDN := sdn.NewLoggingFakeClient()

	// Pre-seed an allocation with OUR nic (create succeeded before a crash).
	op, err := fakeSDN.CreatePodCIDRAllocations(context.Background(), sdn.CreatePodCIDRAllocationsRequest{
		Allocations: []sdn.PodCIDRAllocationSpec{
			{VPCPrefixReservationID: rcRsv, NetworkInterfaceID: rcNIC, DestinationCIDR: rcCIDR},
		},
		Context: sdn.PodCIDRAllocationContext{ProjectID: rcProject, VPCNetworkID: rcVPC, Location: rcLoc},
	})
	if err != nil {
		t.Fatalf("preseed: %v", err)
	}
	drainPreseed(t, fakeSDN, op.OperationID)

	h := routes.NewReconcileHarness(rcConfig(), kube, fakeSDN, m)
	seed(t, h, ciliumNodeObj(rcCIDR), node)

	// No op-id label -> C7 List-before-create adopts, then finalizes.
	if _, err := h.Reconcile(context.Background(), rcNode); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if hasUnroutableTaint(getNode(t, kube)) {
		t.Fatalf("adopted allocation should finalize the node")
	}
	if len(listAll(t, fakeSDN)) != 1 {
		t.Fatalf("adopt must not create a second allocation")
	}
}

func TestReconcile_NodeAbsentDefersFinalize(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m := mock_client.NewMockApiClient(ctrl)
	mockInstance(m)

	kube := k8sfake.NewSimpleClientset() // no Node object
	fakeSDN := sdn.NewLoggingFakeClient()
	h := routes.NewReconcileHarness(rcConfig(), kube, fakeSDN, m)
	seed(t, h, ciliumNodeObj(rcCIDR), nil) // No Node.

	// Create proceeds; finalize is deferred (node==nil).
	if _, err := h.Reconcile(context.Background(), rcNode); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	h.PollOpsOnce(context.Background())
	if _, err := h.Reconcile(context.Background(), rcNode); err != nil {
		t.Fatalf("reconcile2: %v", err)
	}

	if len(listAll(t, fakeSDN)) != 1 {
		t.Fatalf("allocation should be created even without a Node")
	}
}

func TestReconcile_RecoverOpIDLabelSucceeded(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m := mock_client.NewMockApiClient(ctrl)
	mockInstance(m)

	node := taintedNode()
	fakeSDN := sdn.NewLoggingFakeClient()
	fakeSDN.PendingPolls = 1

	kube1 := k8sfake.NewSimpleClientset(node)
	h := routes.NewReconcileHarness(rcConfig(), kube1, fakeSDN, m)
	seed(t, h, ciliumNodeObj(rcCIDR), node)

	// First reconcile creates and registers the op.
	if _, err := h.Reconcile(context.Background(), rcNode); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(h.Tracker().PendingIDs()) != 1 {
		t.Fatalf("expected one tracked op, got %d", len(h.Tracker().PendingIDs()))
	}

	// Simulate a NEW leader: fresh tracker that has never seen the op, but the
	// op-id label is durable on the Node.
	patched := getNode(t, kube1)
	if patched.Labels[routes.OpIDLabel] == "" {
		t.Fatalf("op-id label must be present for recovery")
	}
	kube2 := k8sfake.NewSimpleClientset(patched)
	h2 := routes.NewReconcileHarness(rcConfig(), kube2, fakeSDN, m)
	seed(t, h2, ciliumNodeObj(rcCIDR), patched)

	// New leader: op-id present, tracker unaware -> re-track, then resolves.
	if _, err := h2.Reconcile(context.Background(), rcNode); err != nil {
		t.Fatalf("recovery reconcile: %v", err)
	}
	drainToReady(t, h2)
	assertFinalized(t, getNode(t, kube2))
}

func TestReconcile_RecoverOpIDLabelExpiredAdopts(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m := mock_client.NewMockApiClient(ctrl)
	mockInstance(m)

	fakeSDN := sdn.NewLoggingFakeClient()
	// Pre-seed an allocation with our NIC (the create succeeded; op history lost).
	op, err := fakeSDN.CreatePodCIDRAllocations(context.Background(), sdn.CreatePodCIDRAllocationsRequest{
		Allocations: []sdn.PodCIDRAllocationSpec{
			{VPCPrefixReservationID: rcRsv, NetworkInterfaceID: rcNIC, DestinationCIDR: rcCIDR},
		},
		Context: sdn.PodCIDRAllocationContext{ProjectID: rcProject, VPCNetworkID: rcVPC, Location: rcLoc},
	})
	if err != nil {
		t.Fatalf("preseed: %v", err)
	}
	drainPreseed(t, fakeSDN, op.OperationID)

	// Node carries an op-id label for an op the SDN no longer knows.
	node := taintedNode()
	node.Labels = map[string]string{routes.OpIDLabel: "op-expired-unknown"}
	kube := k8sfake.NewSimpleClientset(node)
	h := routes.NewReconcileHarness(rcConfig(), kube, fakeSDN, m)
	seed(t, h, ciliumNodeObj(rcCIDR), node)

	// First reconcile registers the unknown op (tracker had never seen it) and
	// requeues; the poll will not find it.
	if _, err := h.Reconcile(context.Background(), rcNode); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	// Drive misses until the tracker drops it, then reconcile adopts via List.
	for range routes.MaxOpMisses + 2 {
		h.PollOpsOnce(context.Background())
		if _, err := h.Reconcile(context.Background(), rcNode); err != nil {
			t.Fatalf("reconcile loop: %v", err)
		}
		if err := h.SyncNodeFromClient(context.Background(), rcNode); err != nil {
			t.Fatalf("sync: %v", err)
		}
	}

	assertFinalized(t, getNode(t, kube))
	if len(listAll(t, fakeSDN)) != 1 {
		t.Fatalf("recovery must adopt, not duplicate")
	}
}

// strictNoCallSDN fails the test if any SDN method is invoked.
type strictNoCallSDN struct{ t *testing.T }

func (s *strictNoCallSDN) CreatePodCIDRAllocations(
	context.Context, sdn.CreatePodCIDRAllocationsRequest,
) (*sdn.Operation, error) {
	s.t.Fatalf("unexpected CreatePodCIDRAllocations call")

	return nil, nil
}

func (s *strictNoCallSDN) DeletePodCIDRAllocations(
	context.Context, sdn.DeletePodCIDRAllocationsRequest,
) (*sdn.Operation, error) {
	s.t.Fatalf("unexpected DeletePodCIDRAllocations call")

	return nil, nil
}

func (s *strictNoCallSDN) ListPodCIDRAllocations(
	context.Context, sdn.ListPodCIDRAllocationsQuery,
) ([]sdn.PodCIDRAllocation, error) {
	s.t.Fatalf("unexpected ListPodCIDRAllocations call")

	return nil, nil
}

func (s *strictNoCallSDN) ListPodCIDRAllocationOperations(
	context.Context, sdn.ListPodCIDRAllocationOperationsQuery,
) ([]sdn.Operation, error) {
	s.t.Fatalf("unexpected ListPodCIDRAllocationOperations call")

	return nil, nil
}
