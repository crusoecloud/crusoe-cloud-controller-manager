package routes

import (
	"fmt"

	"github.com/crusoecloud/crusoe-cloud-controller-manager/internal/routes/sdn"
	"k8s.io/klog/v2"
)

// BuildSDNClient chooses the SDN client for native mode. With no endpoint
// rendered it keeps the in-memory logging fake (rollout compatibility with
// addon-controller MR 57, which does not render the SDN vars yet) and returns a
// nil close func. With an endpoint it dials the real gRPC client (mTLS when the
// cert trio is set, plaintext otherwise) and returns its Close as the teardown.
func BuildSDNClient(cfg *Config) (sdn.PodCIDRAllocationClient, func() error, error) {
	if cfg.SDNEndpoint == "" {
		klog.Warning("crusoe native mode running with the LOGGING FAKE SDN client " +
			"(no CRUSOE_SDN_ENDPOINT rendered; SDN state is in-memory only)")

		return sdn.NewLoggingFakeClient(), nil, nil
	}

	mode := "plaintext"
	if cfg.SDNCertFile != "" {
		mode = "mTLS"
	}
	klog.InfoS("dialing SDN endpoint", "endpoint", cfg.SDNEndpoint, "mode", mode)

	gc, err := sdn.NewGRPCClient(cfg.SDNEndpoint, cfg.SDNCertFile, cfg.SDNKeyFile, cfg.SDNCAFile)
	if err != nil {
		return nil, nil, fmt.Errorf("building SDN gRPC client: %w", err)
	}

	return gc, gc.Close, nil
}
