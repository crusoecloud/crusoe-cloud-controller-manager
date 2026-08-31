package routes

import (
	"context"
	"encoding/json"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	corev1listers "k8s.io/client-go/listers/core/v1"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
)

// mirrorHarness wires a PodCIDRMirror to a fake clientset and indexer-backed
// listers for driving reconcile directly (no informers).
type mirrorHarness struct {
	mirror        *PodCIDRMirror
	kube          *k8sfake.Clientset
	ciliumIndexer cache.Indexer
	nodeIndexer   cache.Indexer
}

func newMirrorHarness() *mirrorHarness {
	kube := k8sfake.NewSimpleClientset()
	ciliumIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	nodeIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})

	m := &PodCIDRMirror{
		kubeClient: kube,
		ciliumNodeLister: cache.NewGenericLister(
			ciliumIndexer, schema.GroupResource{Group: "cilium.io", Resource: "ciliumnodes"}),
		nodeLister: corev1listers.NewNodeLister(nodeIndexer),
	}

	return &mirrorHarness{mirror: m, kube: kube, ciliumIndexer: ciliumIndexer, nodeIndexer: nodeIndexer}
}

// seedNode adds the node to both the lister (what reconcile reads) and the fake
// clientset (what the patch targets).
func (h *mirrorHarness) seedNode(t *testing.T, node *v1.Node) {
	t.Helper()
	if err := h.nodeIndexer.Add(node); err != nil {
		t.Fatalf("seeding node lister: %v", err)
	}
	if _, err := h.kube.CoreV1().Nodes().Create(context.Background(), node, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seeding node clientset: %v", err)
	}
}

// seedCiliumNode adds a CiliumNode (named mirrorNode) to the generic lister.
func (h *mirrorHarness) seedCiliumNode(t *testing.T, podCIDRs ...string) {
	t.Helper()
	if err := h.ciliumIndexer.Add(mirrorCiliumNode(mirrorNode, podCIDRs...)); err != nil {
		t.Fatalf("seeding cilium lister: %v", err)
	}
}

func mirrorCiliumNode(name string, podCIDRs ...string) *unstructured.Unstructured {
	cidrs := make([]any, len(podCIDRs))
	for i, c := range podCIDRs {
		cidrs[i] = c
	}

	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cilium.io/v2",
		"kind":       "CiliumNode",
		"metadata":   map[string]any{"name": name},
		"spec":       map[string]any{"ipam": map[string]any{"podCIDRs": cidrs}},
	}}
}

func nodeObj(podCIDR string) *v1.Node {
	n := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: mirrorNode}}
	n.Spec.PodCIDR = podCIDR

	return n
}

const mirrorNode = "worker-0"

// patchActions returns the node patch actions recorded on the fake clientset.
func (h *mirrorHarness) patchActions(t *testing.T) []clienttesting.PatchAction {
	t.Helper()
	var out []clienttesting.PatchAction
	for _, a := range h.kube.Actions() {
		if p, ok := a.(clienttesting.PatchAction); ok && p.GetResource().Resource == "nodes" {
			out = append(out, p)
		}
	}

	return out
}

func TestMirror_WritesV4CIDR(t *testing.T) {
	t.Parallel()
	h := newMirrorHarness()
	h.seedNode(t, nodeObj(""))
	h.seedCiliumNode(t, "10.100.4.0/24")

	if err := h.mirror.reconcile(context.Background(), mirrorNode); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	patches := h.patchActions(t)
	if len(patches) != 1 {
		t.Fatalf("want 1 patch, got %d", len(patches))
	}
	assertPatchSetsCIDR(t, patches[0], "10.100.4.0/24")
}

func TestMirror_SkipsWhenPodCIDRSet(t *testing.T) {
	t.Parallel()
	h := newMirrorHarness()
	h.seedNode(t, nodeObj("10.100.4.0/24")) // already set
	h.seedCiliumNode(t, "10.100.4.0/24")

	if err := h.mirror.reconcile(context.Background(), mirrorNode); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := len(h.patchActions(t)); got != 0 {
		t.Fatalf("want zero patches when podCIDR already set, got %d", got)
	}
}

func TestMirror_PicksV4FromV6First(t *testing.T) {
	t.Parallel()
	h := newMirrorHarness()
	h.seedNode(t, nodeObj(""))
	h.seedCiliumNode(t, "fd00::/64", "10.100.4.0/24")

	if err := h.mirror.reconcile(context.Background(), mirrorNode); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	patches := h.patchActions(t)
	if len(patches) != 1 {
		t.Fatalf("want 1 patch, got %d", len(patches))
	}
	assertPatchSetsCIDR(t, patches[0], "10.100.4.0/24")
}

func TestMirror_NoV4NoOp(t *testing.T) {
	t.Parallel()
	h := newMirrorHarness()
	h.seedNode(t, nodeObj(""))
	h.seedCiliumNode(t, "fd00::/64")

	if err := h.mirror.reconcile(context.Background(), mirrorNode); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := len(h.patchActions(t)); got != 0 {
		t.Fatalf("want zero patches when no v4 prefix, got %d", got)
	}
}

func TestMirror_EmptyPodCIDRsNoOp(t *testing.T) {
	t.Parallel()
	h := newMirrorHarness()
	h.seedNode(t, nodeObj(""))
	h.seedCiliumNode(t) // no podCIDRs yet

	if err := h.mirror.reconcile(context.Background(), mirrorNode); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := len(h.patchActions(t)); got != 0 {
		t.Fatalf("want zero patches when podCIDRs empty, got %d", got)
	}
}

func TestMirror_NodeAbsentNoOp(t *testing.T) {
	t.Parallel()
	h := newMirrorHarness()
	// No Node in the lister; CiliumNode present.
	h.seedCiliumNode(t, "10.100.4.0/24")

	if err := h.mirror.reconcile(context.Background(), mirrorNode); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := len(h.patchActions(t)); got != 0 {
		t.Fatalf("want zero patches when node absent, got %d", got)
	}
}

// assertPatchSetsCIDR verifies the strategic-merge patch sets podCIDR == cidr
// and podCIDRs == [cidr].
func assertPatchSetsCIDR(t *testing.T, p clienttesting.PatchAction, cidr string) {
	t.Helper()
	body := map[string]any{}
	if err := json.Unmarshal(p.GetPatch(), &body); err != nil {
		t.Fatalf("unmarshal patch: %v", err)
	}
	spec, ok := body["spec"].(map[string]any)
	if !ok {
		t.Fatalf("patch has no spec object: %s", p.GetPatch())
	}
	if spec["podCIDR"] != cidr {
		t.Fatalf("podCIDR = %v, want %q", spec["podCIDR"], cidr)
	}
	cidrs, ok := spec["podCIDRs"].([]any)
	if !ok || len(cidrs) != 1 || cidrs[0] != cidr {
		t.Fatalf("podCIDRs = %v, want [%q]", cidrs, cidr)
	}
}
