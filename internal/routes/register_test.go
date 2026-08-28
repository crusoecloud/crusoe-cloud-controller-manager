package routes_test

import (
	"errors"
	"testing"

	"github.com/crusoecloud/crusoe-cloud-controller-manager/internal/routes"
	"k8s.io/cloud-provider/app"
)

// TestStartRouteControllerWrapper_ReturnsInitFunc verifies the constructor
// produces a non-nil InitFunc (the app framework requires one).
func TestStartRouteControllerWrapper_ReturnsInitFunc(t *testing.T) {
	t.Parallel()
	fn := routes.StartRouteControllerWrapper(app.ControllerInitContext{}, nil, nil)
	if fn == nil {
		t.Fatalf("expected a non-nil InitFunc")
	}
}

type modeGateCase struct {
	name        string
	env         map[string]string
	wantErr     error
	wantEnabled bool
}

func modeGateCases() []modeGateCase {
	return []modeGateCase{
		{name: "overlay default is a no-op", env: map[string]string{}, wantEnabled: false},
		{
			name: "native with all vars enabled",
			env: map[string]string{
				envRoutingMode: routes.RoutingModeNative,
				envVPCID:       testVPCID,
				envReservation: testReservation,
				envProjectID:   testProjectID,
			},
			wantEnabled: true,
		},
		{
			name: "native missing reservation fails fast",
			env: map[string]string{
				envRoutingMode: routes.RoutingModeNative,
				envVPCID:       testVPCID,
				envProjectID:   testProjectID,
			},
			wantErr: routes.ErrMissingConfig,
		},
		{
			name: "overlay with stray vpc id fails fast",
			env: map[string]string{
				envRoutingMode: routes.RoutingModeOverlay,
				envVPCID:       testVPCID,
			},
			wantErr: routes.ErrInconsistentConfig,
		},
	}
}

// TestModeGate_FailFast checks the fail-fast / no-op decisions that
// startRouteController makes from LoadConfigFromEnv (partial/missing native env
// must error; overlay must be a clean no-op; native must be enabled). It is not
// parallel: each case mutates process env via t.Setenv.
func TestModeGate_FailFast(t *testing.T) {
	for _, tc := range modeGateCases() {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range []string{envRoutingMode, envVPCID, envReservation, envProjectID} {
				t.Setenv(k, "")
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			cfg, err := routes.LoadConfigFromEnv()
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("expected %v, got %v", tc.wantErr, err)
				}

				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if routes.NativeModeEnabled(cfg) != tc.wantEnabled {
				t.Fatalf("enabled=%v, want %v", routes.NativeModeEnabled(cfg), tc.wantEnabled)
			}
		})
	}
}
