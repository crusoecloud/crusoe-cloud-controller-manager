package routes

import (
	"errors"
	"testing"
)

const (
	envRoutingMode  = "CRUSOE_ROUTING_MODE"
	envVPCID        = "CRUSOE_VPC_ID"
	envReservation  = "CRUSOE_VPC_PREFIX_RESERVATION_ID"
	envProjectID    = "CRUSOE_PROJECT_ID"
	testVPCID       = "net-abc"
	testReservation = "rsv-pods"
	testProjectID   = "proj-123"
)

type configCase struct {
	name    string
	env     map[string]string
	wantErr error
	check   func(t *testing.T, cfg *Config)
}

func checkOverlay(t *testing.T, cfg *Config) {
	t.Helper()
	if cfg.RoutingMode != RoutingModeOverlay {
		t.Fatalf("expected overlay, got %q", cfg.RoutingMode)
	}
}

func checkNative(t *testing.T, cfg *Config) {
	t.Helper()
	if cfg.VPCID != testVPCID || cfg.VPCPrefixReservationID != testReservation ||
		cfg.ProjectID != testProjectID {

		t.Fatalf("native config not populated: %+v", cfg)
	}
	if cfg.Location != "" {
		t.Fatalf("location must not come from env, got %q", cfg.Location)
	}
	if cfg.Workers != 4 {
		t.Fatalf("expected default 4 workers, got %d", cfg.Workers)
	}
}

func nativeEnv() map[string]string {
	return map[string]string{
		envRoutingMode: RoutingModeNative,
		envVPCID:       testVPCID,
		envReservation: testReservation,
		envProjectID:   testProjectID,
	}
}

// withoutKey returns a copy of nativeEnv with one key removed.
func withoutKey(k string) map[string]string {
	m := nativeEnv()
	delete(m, k)

	return m
}

func configCases() []configCase {
	return []configCase{
		{name: "overlay default when unset", env: map[string]string{}, check: checkOverlay},
		{
			name:  "overlay explicit",
			env:   map[string]string{envRoutingMode: RoutingModeOverlay},
			check: checkOverlay,
		},
		{name: "native with all vars", env: nativeEnv(), check: checkNative},
		{name: "native missing vpc id", env: withoutKey(envVPCID), wantErr: ErrMissingConfig},
		{name: "native missing reservation", env: withoutKey(envReservation), wantErr: ErrMissingConfig},
		{name: "native missing project id", env: withoutKey(envProjectID), wantErr: ErrMissingConfig},
		{
			name:    "overlay with stray vpc id",
			env:     map[string]string{envRoutingMode: RoutingModeOverlay, envVPCID: testVPCID},
			wantErr: ErrInconsistentConfig,
		},
		{
			name:    "overlay with stray reservation",
			env:     map[string]string{envReservation: testReservation},
			wantErr: ErrInconsistentConfig,
		},
	}
}

func runConfigCase(t *testing.T, tc *configCase) {
	t.Helper()
	// Clear all relevant vars, then set the case's.
	for _, k := range []string{envRoutingMode, envVPCID, envReservation, envProjectID} {
		t.Setenv(k, "")
	}
	for k, v := range tc.env {
		t.Setenv(k, v)
	}

	cfg, err := LoadConfigFromEnv()
	if tc.wantErr != nil {
		if !errors.Is(err, tc.wantErr) {
			t.Fatalf("expected error %v, got %v", tc.wantErr, err)
		}

		return
	}
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tc.check != nil {
		tc.check(t, cfg)
	}
}

// TestLoadConfigFromEnv is not parallel: each case mutates process env vars via
// t.Setenv, which is incompatible with t.Parallel.
//
//nolint:paralleltest // env-var mutation cannot run in parallel
func TestLoadConfigFromEnv(t *testing.T) {
	for _, tc := range configCases() {
		t.Run(tc.name, func(t *testing.T) {
			runConfigCase(t, &tc)
		})
	}
}
