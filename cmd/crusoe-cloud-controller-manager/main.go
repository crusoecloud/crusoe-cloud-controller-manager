package main

import (
	"fmt"
	"os"

	cloudcontrollermanager "github.com/crusoecloud/crusoe-cloud-controller-manager/internal"
	"github.com/crusoecloud/crusoe-cloud-controller-manager/internal/auth"
	"github.com/crusoecloud/crusoe-cloud-controller-manager/internal/client"
	"github.com/crusoecloud/crusoe-cloud-controller-manager/internal/node"
	"k8s.io/apimachinery/pkg/util/wait"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/cloud-provider/app"
	"k8s.io/cloud-provider/app/config"
	"k8s.io/cloud-provider/names"
	"k8s.io/cloud-provider/options"
	"k8s.io/component-base/cli/flag"
	"k8s.io/component-base/logs"
	_ "k8s.io/component-base/metrics/prometheus/clientgo"
	_ "k8s.io/component-base/metrics/prometheus/version"
	"k8s.io/klog/v2"
)

const (
	ProviderName            = "crusoe"
	NodeTaintControllerName = "node-taint-controller"
)

func main() {
	opts, err := options.NewCloudControllerManagerOptions()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to construct options: %v\n", err)
		os.Exit(1)
	}
	opts.KubeCloudShared.CloudProvider.Name = ProviderName
	opts.Authentication.SkipInClusterLookup = true

	cloudcontrollermanager.RegisterCloudProvider()

	// Create API client for NodeTaintController
	apiClient := createAPIClient()

	// Set the default CloudNodeLifecycleController to our custom implementation
	app.DefaultInitFuncConstructors[names.CloudNodeLifecycleController] = app.ControllerInitFuncConstructor{
		InitContext: app.ControllerInitContext{
			ClientName: "node-controller",
		},
		Constructor: node.StartCloudNodeLifecycleControllerWrapper,
	}

	// Register the NodeTaintController
	app.DefaultInitFuncConstructors[NodeTaintControllerName] = app.ControllerInitFuncConstructor{
		InitContext: app.ControllerInitContext{
			ClientName: NodeTaintControllerName,
		},
		Constructor: func(initContext app.ControllerInitContext,
			completedConfig *config.CompletedConfig,
			cloud cloudprovider.Interface,
		) app.InitFunc {
			return node.StartNodeTaintControllerWrapper(apiClient, initContext, completedConfig, cloud)
		},
	}

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

func createAPIClient() client.APIClient {
	apiEndPoint := os.Getenv("CRUSOE_API_ENDPOINT")
	apiAccessKey := os.Getenv("CRUSOE_ACCESS_KEY")
	apiSecretKey := os.Getenv("CRUSOE_SECRET_KEY")

	cc := auth.NewCrusoeClient(apiEndPoint, apiAccessKey, apiSecretKey,
		"crusoe-cloud-controller-manager/0.0.1")

	return &client.APIClientImpl{
		CrusoeAPIClient: cc,
	}
}
