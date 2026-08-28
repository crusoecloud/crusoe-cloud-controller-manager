package routes_test

import (
	"testing"

	"github.com/crusoecloud/crusoe-cloud-controller-manager/internal/routes"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
)

func nodeWithTaint() *v1.Node {
	n := &v1.Node{}
	n.Name = "n"
	n.Spec.Taints = []v1.Taint{{Key: routes.PodsUnroutableTaintKey, Effect: v1.TaintEffectNoSchedule}}

	return n
}

func nodeReady() *v1.Node {
	n := &v1.Node{}
	n.Name = "n"
	n.Status.Conditions = []v1.NodeCondition{
		{Type: v1.NodeNetworkUnavailable, Status: v1.ConditionFalse},
	}

	return n
}

func TestNodeNeedsWork_Taint(t *testing.T) {
	t.Parallel()
	if !routes.NodeNeedsWork(nodeWithTaint()) {
		t.Fatalf("node with pods-unroutable taint needs work")
	}
}

func TestNodeNeedsWork_OpIDLabel(t *testing.T) {
	t.Parallel()
	n := nodeReady()
	n.Labels = map[string]string{routes.OpIDLabel: "op-1"}
	if !routes.NodeNeedsWork(n) {
		t.Fatalf("node with in-flight op-id label needs work")
	}
}

func TestNodeNeedsWork_MissingCondition(t *testing.T) {
	t.Parallel()
	n := &v1.Node{}
	n.Name = "n"
	if !routes.NodeNeedsWork(n) {
		t.Fatalf("node lacking NetworkUnavailable=False needs work")
	}
}

func TestNodeNeedsWork_Ready(t *testing.T) {
	t.Parallel()
	if routes.NodeNeedsWork(nodeReady()) {
		t.Fatalf("ready node (no taint, condition set, no op-id) needs no work")
	}
}

func TestMetaName_Object(t *testing.T) {
	t.Parallel()
	obj := &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Name: "worker-0"}}
	name, ok := routes.MetaName(obj)
	if !ok || name != "worker-0" {
		t.Fatalf("expected worker-0, got %q ok=%v", name, ok)
	}
}

func TestMetaName_Tombstone(t *testing.T) {
	t.Parallel()
	obj := &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}}
	tombstone := cache.DeletedFinalStateUnknown{Key: "worker-1", Obj: obj}
	name, ok := routes.MetaName(tombstone)
	if !ok || name != "worker-1" {
		t.Fatalf("expected worker-1 from tombstone, got %q ok=%v", name, ok)
	}
}

func TestMetaName_Unknown(t *testing.T) {
	t.Parallel()
	if _, ok := routes.MetaName("not-an-object"); ok {
		t.Fatalf("expected false for non-object")
	}
}
