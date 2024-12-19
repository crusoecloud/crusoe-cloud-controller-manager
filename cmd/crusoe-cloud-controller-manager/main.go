package main

import (
	"fmt"
	"os"

	"k8s.io/apimachinery/pkg/util/wait"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/cloud-provider/app"
	"k8s.io/cloud-provider/app/config"
	"k8s.io/cloud-provider/options"
	"k8s.io/component-base/cli/flag"
	"k8s.io/component-base/logs"
	_ "k8s.io/component-base/metrics/prometheus/clientgo"
	_ "k8s.io/component-base/metrics/prometheus/version"
	"k8s.io/klog/v2"

	cloudcontrollermanager "github.com/crusoecloud/crusoe-cloud-controller-manager/internal"
)

const ProviderName = "crusoe"

func main() {
	opts, err := options.NewCloudControllerManagerOptions()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to construct options: %v\n", err)
		os.Exit(1)
	}
	opts.KubeCloudShared.CloudProvider.Name = ProviderName
	opts.Authentication.SkipInClusterLookup = true

	cloudcontrollermanager.RegisterCloudProvider()

	command := app.NewCloudControllerManagerCommand(
		opts,
		doInitializer,
		app.DefaultInitFuncConstructors,
		map[string]string{},
		flag.NamedFlagSets{},
		wait.NeverStop,
	)

	logs.InitLogs()

	if err := command.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		logs.FlushLogs() // Ensure logs are flushed before exiting
		os.Exit(1)
	}
}

func doInitializer(cfg *config.CompletedConfig) cloudprovider.Interface {
	cloudConfig := cfg.ComponentConfig.KubeCloudShared.CloudProvider
	// initialize cloud provider with the cloud provider name and config file provided
	cloud, err := cloudprovider.InitCloudProvider(cloudConfig.Name, cloudConfig.CloudConfigFile)
	if err != nil {
		klog.Fatalf("Cloud provider could not be initialized: %v", err)
	}
	if cloud == nil {
		klog.Fatalf("Cloud provider is nil")
	}

	return cloud
}
