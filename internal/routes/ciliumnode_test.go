package routes

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// NOTE: testdata/ciliumnode.json is a HAND-WRITTEN fixture matching the
// cilium.io/v2 CiliumNode CRD shape (metadata + spec.ipam.podCIDRs). Per §18 of
// the LLD it MUST be replaced with an object captured from a real CMK
// native-mode cluster (kubectl get ciliumnode <name> -o json) before this ships
// so the conversion is validated against the exact schema cilium emits.

func loadFixture(t *testing.T) *unstructured.Unstructured {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "ciliumnode.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	obj := map[string]any{}
	if unmarshalErr := json.Unmarshal(data, &obj); unmarshalErr != nil {
		t.Fatalf("unmarshal fixture: %v", unmarshalErr)
	}

	return &unstructured.Unstructured{Object: obj}
}

func TestCiliumNodeFromUnstructured_Fixture(t *testing.T) {
	t.Parallel()
	cn, err := ciliumNodeFromUnstructured(loadFixture(t))
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if cn.Name != "sriprod1-worker-0" {
		t.Fatalf("name mismatch: %q", cn.Name)
	}
	if len(cn.PodCIDRs) != 1 || cn.PodCIDRs[0] != "10.100.4.0/24" {
		t.Fatalf("podCIDRs mismatch: %v", cn.PodCIDRs)
	}
}

func TestCiliumNodeFromUnstructured_MissingIPAM(t *testing.T) {
	t.Parallel()
	// A CiliumNode before cilium has populated spec.ipam.podCIDRs.
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cilium.io/v2",
		"kind":       "CiliumNode",
		"metadata":   map[string]any{"name": "fresh-node"},
		"spec":       map[string]any{},
	}}
	cn, err := ciliumNodeFromUnstructured(u)
	if err != nil {
		t.Fatalf("missing ipam should not error: %v", err)
	}
	if len(cn.PodCIDRs) != 0 {
		t.Fatalf("expected empty podCIDRs, got %v", cn.PodCIDRs)
	}
}

func TestCiliumNodeFromUnstructured_Malformed(t *testing.T) {
	t.Parallel()
	// podCIDRs present but wrong type (a string, not a []string).
	u := &unstructured.Unstructured{Object: map[string]any{
		"kind":     "CiliumNode",
		"metadata": map[string]any{"name": "bad-node"},
		"spec": map[string]any{
			"ipam": map[string]any{"podCIDRs": "10.100.4.0/24"},
		},
	}}
	if _, err := ciliumNodeFromUnstructured(u); err == nil {
		t.Fatalf("expected error for malformed podCIDRs")
	}
}

func TestCiliumNodeFromUnstructured_WrongKind(t *testing.T) {
	t.Parallel()
	u := &unstructured.Unstructured{Object: map[string]any{
		"kind":     "Node",
		"metadata": map[string]any{"name": "n"},
	}}
	if _, err := ciliumNodeFromUnstructured(u); err == nil {
		t.Fatalf("expected ErrNotCiliumNode for wrong kind")
	}
}

func TestCiliumNodeFromUnstructured_Nil(t *testing.T) {
	t.Parallel()
	if _, err := ciliumNodeFromUnstructured(nil); err == nil {
		t.Fatalf("expected error for nil input")
	}
}
