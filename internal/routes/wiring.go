package routes

import (
	"fmt"

	"github.com/crusoecloud/crusoe-cloud-controller-manager/internal/routes/sdn"
	"k8s.io/klog/v2"
)

// BuildSDNClient chooses the SDN client for native mode. With the fake opted in
// (CRUSOE_SDN_USE_FAKE=true) it returns the in-memory logging fake and a nil
// close func. Otherwise it dials the real gRPC client at REGION_ENDPOINT
// (mTLS when the cert trio is set, plaintext otherwise) and returns its Close as
// the teardown. Config validation guarantees exactly one of the two is set.
func BuildSDNClient(cfg *Config) (sdn.PodCIDRAllocationClient, func() error, error) {
	if cfg.SDNUseFake {
		klog.Warning("crusoe native mode running with the LOGGING FAKE SDN client " +
			"(CRUSOE_SDN_USE_FAKE=true; SDN state is in-memory only)")

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
