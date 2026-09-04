package routes

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"

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

	// SDNEndpointEnv is the region coordinator gRPC address, host or host:port.
	// The name matches what addon-controller renders from the cmk-addon-catalog
	// ConfigMap's regionEndpoint key, which is a bare host (e.g. 10.71.1.1), so a
	// value without a port gets defaultRegionPort appended. Required in native
	// mode unless SDNUseFakeEnv opts in to the fake. Set + cert trio absent =
	// plaintext (KM local-dev parity); set + full cert trio = mTLS.
	SDNEndpointEnv = "REGION_ENDPOINT"
	// defaultRegionPort is the region coordinator's gRPC port, appended when
	// REGION_ENDPOINT carries no port.
	defaultRegionPort = "9090"
	// SDNUseFakeEnv ("true") runs the in-memory logging fake instead of dialing
	// an endpoint. Explicit opt-in for local dev only: SDN state lives in the
	// process and dies with it.
	SDNUseFakeEnv = "CRUSOE_SDN_USE_FAKE"
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

// Config holds the resolved route configuration.
type Config struct {
	RoutingMode             string
	ProjectID               string   // from CRUSOE_PROJECT_ID; the instance client needs it too
	VPCID                   string   // context.vpc_network_id
	VPCPrefixReservationIDs []string // creation order; creates pick by cidr containment
	// Location is the external location name, resolved fail-fast at startup
	// from the cluster object (section 5); then immutable.
	Location string
	// SDNLocation is the internal name for Location, resolved at startup via
	// the SDN (ResolveLocation); it is what every RPC context carries.
	SDNLocation string

	// SDN gRPC wiring (native mode only; all plain strings so config.go stays
	// free of schemas imports, the seam boundary). Exactly one of SDNEndpoint
	// and SDNUseFake is set. Cert trio empty + endpoint set = plaintext; full
	// trio + endpoint set = mTLS.
	SDNEndpoint string
	SDNCertFile string
	SDNKeyFile  string
	SDNCAFile   string
	SDNUseFake  bool // CRUSOE_SDN_USE_FAKE=true: run the in-memory logging fake
}

var (
	// ErrMissingConfig indicates a required native-mode env var is empty.
	ErrMissingConfig = errors.New("missing required environment variable for native routing mode")
	// ErrInconsistentConfig indicates a partial native-routing env set.
	ErrInconsistentConfig = errors.New(
		"partial native-routing env set (CRUSOE_ROUTING_MODE / CRUSOE_VPC_ID / " +
			"CRUSOE_VPC_PREFIX_RESERVATION_IDS must be all set or all absent)")
	// ErrInconsistentSDNConfig indicates a malformed SDN wiring env set: a partial
	// cert trio, a cert var set without REGION_ENDPOINT, or neither/both of
	// REGION_ENDPOINT and CRUSOE_SDN_USE_FAKE in native mode.
	ErrInconsistentSDNConfig = errors.New(
		"inconsistent SDN env set (native mode needs exactly one of REGION_ENDPOINT or " +
			"CRUSOE_SDN_USE_FAKE=true; CRUSOE_SDN_CERT_FILE / CRUSOE_SDN_KEY_FILE / " +
			"CRUSOE_SDN_CA_FILE must be all set or all absent, and require REGION_ENDPOINT)")
)

// LoadConfigFromEnv reads the route-controller configuration from the process
// environment. In overlay mode it returns a config with RoutingMode set; SDN
// vars are read but not validated (nothing uses them), and a stray VPC var
// returns ErrInconsistentConfig. In native mode it requires CRUSOE_VPC_ID,
// CRUSOE_VPC_PREFIX_RESERVATION_IDS and CRUSOE_PROJECT_ID to be non-empty.
// Location is always derived from platform metadata (section 5.1), never from env.
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
		SDNEndpoint:             withDefaultPort(os.Getenv(SDNEndpointEnv)),
		SDNUseFake:              os.Getenv(SDNUseFakeEnv) == "true",
		SDNCertFile:             os.Getenv(SDNCertFileEnv),
		SDNKeyFile:              os.Getenv(SDNKeyFileEnv),
		SDNCAFile:               os.Getenv(SDNCAFileEnv),
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

// validateOverlay enforces that no VPC wiring is set in overlay/non-native
// mode, a stray VPC var is a misrendered config. SDN vars are region-level
// wiring that the deployment may render regardless of routing mode; they are
// ignored here (nothing dials the SDN in overlay mode) and only validated in
// native mode.
func validateOverlay(cfg *Config) error {
	if cfg.VPCID != "" || len(cfg.VPCPrefixReservationIDs) > 0 {
		return ErrInconsistentConfig
	}

	return nil
}

// validateSDN enforces the SDN wiring rules for native mode: exactly one of an
// endpoint or the fake opt-in, the cert trio is all-or-none, and any cert var
// requires an endpoint.
func validateSDN(cfg *Config) error {
	if (cfg.SDNEndpoint != "") == cfg.SDNUseFake {
		return ErrInconsistentSDNConfig
	}

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

// withDefaultPort appends defaultRegionPort to an endpoint that has no port
// ("" and host:port values pass through unchanged).
func withDefaultPort(endpoint string) string {
	if endpoint == "" {
		return ""
	}
	if _, _, err := net.SplitHostPort(endpoint); err == nil {
		return endpoint
	}

	return net.JoinHostPort(endpoint, defaultRegionPort)
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
