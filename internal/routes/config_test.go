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
	envSDNEndpoint  = "REGION_ENDPOINT"
	envSDNCert      = "CRUSOE_SDN_CERT_FILE"
	envSDNKey       = "CRUSOE_SDN_KEY_FILE"
	envSDNCA        = "CRUSOE_SDN_CA_FILE"
	envSDNUseFake   = "CRUSOE_SDN_USE_FAKE"
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
}

// nativeEnv is a valid native-mode env: the SDN client is the explicitly
// opted-in fake, so cases that care about the endpoint use nativeSDNEnv.
func nativeEnv() map[string]string {
	return map[string]string{
		envRoutingMode: RoutingModeNative,
		envVPCID:       testVPCID,
		envReservation: testReservation,
		envProjectID:   testProjectID,
		envSDNUseFake:  "true",
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

// nativeSDNEnv returns nativeEnv with the fake opt-in dropped and the given SDN
// vars set, for the cases that exercise real endpoint wiring.
func nativeSDNEnv(extra map[string]string) map[string]string {
	m := withoutKey(envSDNUseFake)
	for k, v := range extra {
		m[k] = v
	}

	return m
}

// sdnFakeConfigCases covers the fake opt-in rules (split from sdnConfigCases
// to stay within funlen).
func sdnFakeConfigCases() []configCase {
	return []configCase{
		{
			name:  "overlay ignores sdn fake opt-in",
			env:   map[string]string{envRoutingMode: RoutingModeOverlay, envSDNUseFake: "true"},
			check: checkOverlay,
		},
		{
			name: "native fake opt-in without endpoint",
			env:  nativeEnv(),
			check: func(t *testing.T, cfg *Config) {
				t.Helper()
				if !cfg.SDNUseFake || cfg.SDNEndpoint != "" {
					t.Fatalf("expected the fake opted in with no endpoint, got %+v", cfg)
				}
			},
		},
		{
			name:    "native without endpoint or fake opt-in fails",
			env:     nativeSDNEnv(nil),
			wantErr: ErrInconsistentSDNConfig,
		},
		{
			name: "native endpoint plus fake opt-in fails",
			env: func() map[string]string {
				m := nativeEnv()
				m[envSDNEndpoint] = testSDNEndpoint

				return m
			}(),
			wantErr: ErrInconsistentSDNConfig,
		},
	}
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
			// SDN vars are region-level wiring rendered regardless of routing
			// mode; overlay must not fail on them (even a partial cert trio).
			name: "overlay ignores sdn endpoint and partial cert trio",
			env: map[string]string{
				envRoutingMode: RoutingModeOverlay, envSDNEndpoint: testSDNEndpoint, envSDNCert: testCertFile,
			},
			check: checkOverlay,
		},
		{
			name: "native endpoint only is plaintext",
			env:  nativeSDNEnv(map[string]string{envSDNEndpoint: testSDNEndpoint}),
			check: func(t *testing.T, cfg *Config) {
				t.Helper()
				if cfg.SDNEndpoint != testSDNEndpoint || cfg.SDNCertFile != "" {
					t.Fatalf("expected plaintext endpoint config, got %+v", cfg)
				}
			},
		},
		{
			// The catalog's regionEndpoint is a bare host; the CCM supplies the port.
			name: "native bare-host endpoint gets the default region port",
			env:  nativeSDNEnv(map[string]string{envSDNEndpoint: "10.71.1.1"}),
			check: func(t *testing.T, cfg *Config) {
				t.Helper()
				if cfg.SDNEndpoint != "10.71.1.1:"+defaultRegionPort {
					t.Fatalf("expected default port appended, got %q", cfg.SDNEndpoint)
				}
			},
		},
		{
			name: "native endpoint with full cert trio is mTLS",
			env:  nativeSDNEnv(fullTrio),
			check: func(t *testing.T, cfg *Config) {
				t.Helper()
				if cfg.SDNCertFile != testCertFile || cfg.SDNKeyFile != testKeyFile || cfg.SDNCAFile != testCAFile {
					t.Fatalf("expected full cert trio, got %+v", cfg)
				}
			},
		},
		{
			name:    "native partial cert trio fails",
			env:     nativeSDNEnv(map[string]string{envSDNEndpoint: testSDNEndpoint, envSDNCert: testCertFile}),
			wantErr: ErrInconsistentSDNConfig,
		},
		{
			name: "native cert trio without endpoint fails",
			env: nativeSDNEnv(map[string]string{
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
		envSDNEndpoint, envSDNCert, envSDNKey, envSDNCA, envSDNUseFake,
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
	cases = append(cases, sdnFakeConfigCases()...)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runConfigCase(t, &tc)
		})
	}
}
