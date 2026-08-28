package routes

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/crusoecloud/crusoe-cloud-controller-manager/internal/client"
)

// Environment variables that configure the route controller. In native mode the
// crusoe-ccm Deployment (addon-controller MR 57) sets exactly three routing
// vars: RoutingModeEnv, VPCIDEnv and VPCPrefixReservationIDEnv. All three are
// set together in native mode and absent in overlay; a partial set is a
// misrendered config and must fail startup loudly.
const (
	// RoutingModeEnv selects the routing mode: "overlay" (default) or "native".
	RoutingModeEnv = "CRUSOE_ROUTING_MODE"
	// VPCIDEnv maps to PodCIDRAllocationContext.vpc_network_id.
	VPCIDEnv = "CRUSOE_VPC_ID"
	// VPCPrefixReservationIDEnv is the KM-provisioned reservation id.
	VPCPrefixReservationIDEnv = "CRUSOE_VPC_PREFIX_RESERVATION_ID"

	// The following are defined but unused this drop (real gRPC client wiring).

	// SDNEndpointEnv is the SDN service endpoint (unused this drop).
	SDNEndpointEnv = "CRUSOE_SDN_ENDPOINT"
	// SDNClientCertPathEnv is the mTLS client cert path (unused this drop).
	SDNClientCertPathEnv = "CRUSOE_SDN_CLIENT_CERT_PATH"
	// SDNClientKeyPathEnv is the mTLS client key path (unused this drop).
	SDNClientKeyPathEnv = "CRUSOE_SDN_CLIENT_KEY_PATH"
	// SDNCACertPathEnv is the mTLS CA cert path (unused this drop).
	SDNCACertPathEnv = "CRUSOE_SDN_CA_CERT_PATH"

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
	RoutingMode            string
	ProjectID              string // from CRUSOE_PROJECT_ID; the instance client needs it too
	VPCID                  string // context.vpc_network_id
	VPCPrefixReservationID string
	Location               string // resolved at startup from the cluster object; instance-derived fallback (§5.1)

	SDNEndpoint       string // unused this drop
	SDNClientCertPath string // unused this drop
	SDNClientKeyPath  string // unused this drop
	SDNCACertPath     string // unused this drop

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
			"CRUSOE_VPC_PREFIX_RESERVATION_ID must be all set or all absent)")
)

// LoadConfigFromEnv reads the route-controller configuration from the process
// environment. In overlay mode it returns a config with RoutingMode set but no
// SDN wiring; if a stray VPC var is set in overlay mode it returns
// ErrInconsistentConfig. In native mode it requires CRUSOE_VPC_ID,
// CRUSOE_VPC_PREFIX_RESERVATION_ID and CRUSOE_PROJECT_ID to be non-empty.
// Location is always derived from platform metadata (§5.1), never from env.
func LoadConfigFromEnv() (*Config, error) {
	mode := os.Getenv(RoutingModeEnv)
	if mode == "" {
		mode = RoutingModeOverlay
	}

	cfg := &Config{
		RoutingMode:            mode,
		ProjectID:              os.Getenv(client.CrusoeProjectID),
		VPCID:                  os.Getenv(VPCIDEnv),
		VPCPrefixReservationID: os.Getenv(VPCPrefixReservationIDEnv),
		SDNEndpoint:            os.Getenv(SDNEndpointEnv),
		SDNClientCertPath:      os.Getenv(SDNClientCertPathEnv),
		SDNClientKeyPath:       os.Getenv(SDNClientKeyPathEnv),
		SDNCACertPath:          os.Getenv(SDNCACertPathEnv),
		PollInterval:           defaultPollInterval,
		ReaperInterval:         defaultReaperInterval,
		ReaperGrace:            defaultReaperGrace,
		Workers:                defaultWorkers,
	}

	if mode == RoutingModeNative {
		if err := validateNative(cfg); err != nil {
			return nil, err
		}

		return cfg, nil
	}

	// Overlay (or any non-native mode): the VPC vars must not be set.
	if cfg.VPCID != "" || cfg.VPCPrefixReservationID != "" {
		return nil, ErrInconsistentConfig
	}

	return cfg, nil
}

// validateNative enforces that all required native-mode vars are present.
func validateNative(cfg *Config) error {
	required := []struct {
		name  string
		value string
	}{
		{VPCIDEnv, cfg.VPCID},
		{VPCPrefixReservationIDEnv, cfg.VPCPrefixReservationID},
		{client.CrusoeProjectID, cfg.ProjectID},
	}
	for _, r := range required {
		if r.value == "" {
			return fmt.Errorf("%w: %s", ErrMissingConfig, r.name)
		}
	}

	return nil
}
