package routes

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/crusoecloud/crusoe-cloud-controller-manager/internal/client"
)

// Environment variables that configure the route controller. In native mode the
// crusoe-ccm Deployment (addon-controller MR 57) sets exactly three routing
// vars: RoutingModeEnv, VPCIDEnv and VPCPrefixReservationIDsEnv. All three are
// set together in native mode and absent in overlay; a partial set is a
// misrendered config and must fail startup loudly.
const (
	// RoutingModeEnv selects the routing mode: "overlay" (default) or "native".
	RoutingModeEnv = "CRUSOE_ROUTING_MODE"
	// VPCIDEnv maps to PodCIDRAllocationContext.vpc_network_id.
	VPCIDEnv = "CRUSOE_VPC_ID"
	// VPCPrefixReservationIDsEnv is the comma-separated list of KM-provisioned
	// reservation ids in creation order: one at cluster create, more after a
	// pod-range expansion (the reservation API has no resize).
	VPCPrefixReservationIDsEnv = "CRUSOE_VPC_PREFIX_RESERVATION_IDS"

	// SDNEndpointEnv is the host:port of the region SDN gRPC server. Optional in
	// native mode: when absent the controller keeps the in-memory logging fake
	// (addon-controller MR 57 does not render it yet). Set + cert trio absent =
	// plaintext (KM local-dev parity); set + full cert trio = mTLS.
	SDNEndpointEnv = "CRUSOE_SDN_ENDPOINT"
	// SDNCertFileEnv, SDNKeyFileEnv and SDNCAFileEnv are the client mTLS material
	// for the SDN connection. All-or-none: a partial trio is a misrendered config
	// and fails startup loudly.
	SDNCertFileEnv = "CRUSOE_SDN_CERT_FILE"
	// SDNKeyFileEnv is the client mTLS private key path (see SDNCertFileEnv).
	SDNKeyFileEnv = "CRUSOE_SDN_KEY_FILE"
	// SDNCAFileEnv is the CA cert path used to verify the SDN server (see
	// SDNCertFileEnv).
	SDNCAFileEnv = "CRUSOE_SDN_CA_FILE"

	// RoutingModeOverlay is the default routing mode (controller not started).
	RoutingModeOverlay = "overlay"
	// RoutingModeNative enables VPC-native pod routing.
	RoutingModeNative = "native"
)

// Default timing/concurrency knobs.
const (
	defaultPollInterval   = 5 * time.Second
	defaultReaperInterval = 5 * time.Minute
	defaultReaperGrace    = 10 * time.Minute
	defaultWorkers        = 4
)

// Config holds the resolved route-controller configuration.
type Config struct {
	RoutingMode             string
	ProjectID               string   // from CRUSOE_PROJECT_ID; the instance client needs it too
	VPCID                   string   // context.vpc_network_id
	VPCPrefixReservationIDs []string // creation order; creates pick by cidr containment
	Location                string   // resolved fail-fast at startup from the cluster object (§5.1); then immutable

	// SDN gRPC wiring (native mode only; all plain strings so config.go stays
	// free of schemas imports — the seam boundary). SDNEndpoint empty means "no
	// SDN wiring rendered yet": the controller keeps the logging fake. Cert trio
	// empty + endpoint set = plaintext; full trio + endpoint set = mTLS.
	SDNEndpoint string
	SDNCertFile string
	SDNKeyFile  string
	SDNCAFile   string

	PollInterval   time.Duration // opTracker tick + AddAfter backstop
	ReaperInterval time.Duration
	ReaperGrace    time.Duration
	Workers        int
}

var (
	// ErrMissingConfig indicates a required native-mode env var is empty.
	ErrMissingConfig = errors.New("missing required environment variable for native routing mode")
	// ErrInconsistentConfig indicates a partial native-routing env set.
	ErrInconsistentConfig = errors.New(
		"partial native-routing env set (CRUSOE_ROUTING_MODE / CRUSOE_VPC_ID / " +
			"CRUSOE_VPC_PREFIX_RESERVATION_IDS must be all set or all absent)")
	// ErrInconsistentSDNConfig indicates a malformed SDN wiring env set: a partial
	// cert trio, or a cert var set without CRUSOE_SDN_ENDPOINT.
	ErrInconsistentSDNConfig = errors.New(
		"inconsistent SDN env set (CRUSOE_SDN_CERT_FILE / CRUSOE_SDN_KEY_FILE / " +
			"CRUSOE_SDN_CA_FILE must be all set or all absent, and require CRUSOE_SDN_ENDPOINT)")
)

// LoadConfigFromEnv reads the route-controller configuration from the process
// environment. In overlay mode it returns a config with RoutingMode set but no
// SDN wiring; if a stray VPC var is set in overlay mode it returns
// ErrInconsistentConfig. In native mode it requires CRUSOE_VPC_ID,
// CRUSOE_VPC_PREFIX_RESERVATION_IDS and CRUSOE_PROJECT_ID to be non-empty.
// Location is always derived from platform metadata (§5.1), never from env.
func LoadConfigFromEnv() (*Config, error) {
	mode := os.Getenv(RoutingModeEnv)
	if mode == "" {
		mode = RoutingModeOverlay
	}

	cfg := &Config{
		RoutingMode:             mode,
		ProjectID:               os.Getenv(client.CrusoeProjectID),
		VPCID:                   os.Getenv(VPCIDEnv),
		VPCPrefixReservationIDs: splitCommaList(os.Getenv(VPCPrefixReservationIDsEnv)),
		SDNEndpoint:             os.Getenv(SDNEndpointEnv),
		SDNCertFile:             os.Getenv(SDNCertFileEnv),
		SDNKeyFile:              os.Getenv(SDNKeyFileEnv),
		SDNCAFile:               os.Getenv(SDNCAFileEnv),
		PollInterval:            defaultPollInterval,
		ReaperInterval:          defaultReaperInterval,
		ReaperGrace:             defaultReaperGrace,
		Workers:                 defaultWorkers,
	}

	if mode == RoutingModeNative {
		if err := validateNative(cfg); err != nil {
			return nil, err
		}
		if err := validateSDN(cfg); err != nil {
			return nil, err
		}

		return cfg, nil
	}

	if err := validateOverlay(cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}

// validateOverlay enforces that no native-mode wiring (VPC or SDN vars) is set
// in overlay/non-native mode — a stray var is a misrendered config.
func validateOverlay(cfg *Config) error {
	if cfg.VPCID != "" || len(cfg.VPCPrefixReservationIDs) > 0 {
		return ErrInconsistentConfig
	}
	if cfg.SDNEndpoint != "" || cfg.SDNCertFile != "" || cfg.SDNKeyFile != "" || cfg.SDNCAFile != "" {
		return ErrInconsistentSDNConfig
	}

	return nil
}

// validateSDN enforces the SDN wiring rules for native mode: the cert trio is
// all-or-none, and any cert var requires an endpoint. An absent endpoint (with
// no cert vars) is valid — the controller keeps the logging fake.
func validateSDN(cfg *Config) error {
	certsSet := 0
	for _, v := range []string{cfg.SDNCertFile, cfg.SDNKeyFile, cfg.SDNCAFile} {
		if v != "" {
			certsSet++
		}
	}
	// Partial cert trio, or certs without an endpoint, is a misrender.
	if certsSet != 0 && certsSet != 3 {
		return ErrInconsistentSDNConfig
	}
	if certsSet == 3 && cfg.SDNEndpoint == "" {
		return ErrInconsistentSDNConfig
	}

	return nil
}

// splitCommaList splits a comma-separated env value, trimming whitespace and
// dropping empty elements ("" -> nil).
func splitCommaList(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}

	return out
}

// validateNative enforces that all required native-mode vars are present.
func validateNative(cfg *Config) error {
	required := []struct {
		name  string
		value string
	}{
		{VPCIDEnv, cfg.VPCID},
		{VPCPrefixReservationIDsEnv, strings.Join(cfg.VPCPrefixReservationIDs, ",")},
		{client.CrusoeProjectID, cfg.ProjectID},
	}
	for _, r := range required {
		if r.value == "" {
			return fmt.Errorf("%w: %s", ErrMissingConfig, r.name)
		}
	}

	return nil
}
