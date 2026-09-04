package routes

import (
	"context"
	"errors"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	fakediscovery "k8s.io/client-go/discovery/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/cloud-provider/app"
)

// TestStartPodCIDRMirrorWrapper_ReturnsInitFunc verifies the constructor
// produces a non-nil InitFunc (the app framework requires one).
func TestStartPodCIDRMirrorWrapper_ReturnsInitFunc(t *testing.T) {
	t.Parallel()
	fn := StartPodCIDRMirrorWrapper(app.ControllerInitContext{}, nil, nil)
	if fn == nil {
		t.Fatalf("expected a non-nil InitFunc")
	}
}

func TestWaitForCiliumNodeCRD_ServedResourceReturns(t *testing.T) {
	t.Parallel()
	client := k8sfake.NewSimpleClientset()
	disco, ok := client.Discovery().(*fakediscovery.FakeDiscovery)
	if !ok {
		t.Fatalf("fake clientset discovery is not a FakeDiscovery")
	}
	disco.Resources = []*metav1.APIResourceList{{
		GroupVersion: "cilium.io/v2",
		APIResources: []metav1.APIResource{{Name: "ciliumnodes"}},
	}}

	if err := waitForCiliumNodeCRD(context.Background(), disco); err != nil {
		t.Fatalf("expected immediate success with the CRD served: %v", err)
	}
}

func TestWaitForCiliumNodeCRD_AbsentCRDWaitsUntilCancel(t *testing.T) {
	t.Parallel()
	client := k8sfake.NewSimpleClientset() // no cilium.io/v2 resources

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := waitForCiliumNodeCRD(ctx, client.Discovery())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded while the CRD is absent, got %v", err)
	}
}
