package routes

import (
	"testing"

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
