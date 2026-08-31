package routes

import (
	"errors"
	"slices"
	"testing"
)

const (
	envRoutingMode  = "CRUSOE_ROUTING_MODE"
	envVPCID        = "CRUSOE_VPC_ID"
	envReservation  = "CRUSOE_VPC_PREFIX_RESERVATION_IDS"
	envProjectID    = "CRUSOE_PROJECT_ID"
	envSDNEndpoint  = "CRUSOE_SDN_ENDPOINT"
	envSDNCert      = "CRUSOE_SDN_CERT_FILE"
	envSDNKey       = "CRUSOE_SDN_KEY_FILE"
	envSDNCA        = "CRUSOE_SDN_CA_FILE"
	testVPCID       = "net-abc"
	testReservation = "rsv-pods"
	testProjectID   = "proj-123"
	testSDNEndpoint = "sdn.region.local:443"
	testCertFile    = "/c.pem"
	testKeyFile     = "/k.pem"
	testCAFile      = "/ca.pem"
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
	if cfg.VPCID != testVPCID || !slices.Equal(cfg.VPCPrefixReservationIDs, []string{testReservation}) ||
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
		{
			name: "native with multiple reservations",
			env: func() map[string]string {
				m := nativeEnv()
				m[envReservation] = " rsv-a, rsv-b ,rsv-c,"

				return m
			}(),
			check: func(t *testing.T, cfg *Config) {
				t.Helper()
				if !slices.Equal(cfg.VPCPrefixReservationIDs, []string{"rsv-a", "rsv-b", "rsv-c"}) {
					t.Fatalf("expected trimmed 3-id list, got %v", cfg.VPCPrefixReservationIDs)
				}
			},
		},
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

// nativeEnvWith returns nativeEnv overlaid with the given SDN vars.
func nativeEnvWith(extra map[string]string) map[string]string {
	m := nativeEnv()
	for k, v := range extra {
		m[k] = v
	}

	return m
}

// sdnConfigCases covers the SDN wiring env rules (kept separate from
// configCases to stay within funlen).
func sdnConfigCases() []configCase {
	fullTrio := map[string]string{
		envSDNEndpoint: testSDNEndpoint,
		envSDNCert:     testCertFile, envSDNKey: testKeyFile, envSDNCA: testCAFile,
	}

	return []configCase{
		{
			name:    "overlay with stray sdn endpoint",
			env:     map[string]string{envRoutingMode: RoutingModeOverlay, envSDNEndpoint: testSDNEndpoint},
			wantErr: ErrInconsistentSDNConfig,
		},
		{
			name:    "overlay with stray sdn cert",
			env:     map[string]string{envRoutingMode: RoutingModeOverlay, envSDNCert: testCertFile},
			wantErr: ErrInconsistentSDNConfig,
		},
		{
			name: "native endpoint absent keeps fake",
			env:  nativeEnv(),
			check: func(t *testing.T, cfg *Config) {
				t.Helper()
				if cfg.SDNEndpoint != "" {
					t.Fatalf("expected empty SDN endpoint, got %q", cfg.SDNEndpoint)
				}
			},
		},
		{
			name: "native endpoint only is plaintext",
			env:  nativeEnvWith(map[string]string{envSDNEndpoint: testSDNEndpoint}),
			check: func(t *testing.T, cfg *Config) {
				t.Helper()
				if cfg.SDNEndpoint != testSDNEndpoint || cfg.SDNCertFile != "" {
					t.Fatalf("expected plaintext endpoint config, got %+v", cfg)
				}
			},
		},
		{
			name: "native endpoint with full cert trio is mTLS",
			env:  nativeEnvWith(fullTrio),
			check: func(t *testing.T, cfg *Config) {
				t.Helper()
				if cfg.SDNCertFile != testCertFile || cfg.SDNKeyFile != testKeyFile || cfg.SDNCAFile != testCAFile {
					t.Fatalf("expected full cert trio, got %+v", cfg)
				}
			},
		},
		{
			name:    "native partial cert trio fails",
			env:     nativeEnvWith(map[string]string{envSDNEndpoint: testSDNEndpoint, envSDNCert: testCertFile}),
			wantErr: ErrInconsistentSDNConfig,
		},
		{
			name: "native cert trio without endpoint fails",
			env: nativeEnvWith(map[string]string{
				envSDNCert: testCertFile, envSDNKey: testKeyFile, envSDNCA: testCAFile,
			}),
			wantErr: ErrInconsistentSDNConfig,
		},
	}
}

func runConfigCase(t *testing.T, tc *configCase) {
	t.Helper()
	// Clear all relevant vars, then set the case's.
	for _, k := range []string{
		envRoutingMode, envVPCID, envReservation, envProjectID,
		envSDNEndpoint, envSDNCert, envSDNKey, envSDNCA,
	} {
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
	cases := append(configCases(), sdnConfigCases()...)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runConfigCase(t, &tc)
		})
	}
}
