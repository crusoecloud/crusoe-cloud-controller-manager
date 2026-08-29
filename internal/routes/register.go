package routes

import (
	"context"
	"fmt"
	"os"
	"time"

	cloudcontrollermanager "github.com/crusoecloud/crusoe-cloud-controller-manager/internal"
	auth "github.com/crusoecloud/crusoe-cloud-controller-manager/internal/auth"
	"github.com/crusoecloud/crusoe-cloud-controller-manager/internal/client"
	"github.com/crusoecloud/crusoe-cloud-controller-manager/internal/routes/sdn"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	clientset "k8s.io/client-go/kubernetes"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/cloud-provider/app"
	"k8s.io/cloud-provider/app/config"
	controllermanagerapp "k8s.io/controller-manager/app"
	"k8s.io/controller-manager/controller"
	"k8s.io/klog/v2"
)

const (
	userAgent = "crusoe-cloud-controller-manager/0.0.1"

	informerResync = 30 * time.Minute
)

//nolint:gochecknoglobals // GVR is effectively a constant
var ciliumNodeGVR = schema.GroupVersionResource{
	Group: "cilium.io", Version: "v2", Resource: "ciliumnodes",
}

// StartRouteControllerWrapper adapts startRouteController to the cloud-provider
// app InitFunc constructor signature (mirrors internal/node/utils.go).
func StartRouteControllerWrapper(initContext app.ControllerInitContext,
	completedConfig *config.CompletedConfig,
	cloud cloudprovider.Interface,
) app.InitFunc {
	return func(ctx context.Context,
		controllerContext controllermanagerapp.ControllerContext,
	) (controller.Interface, bool, error) {
		return startRouteController(ctx, initContext, controllerContext, completedConfig, cloud)
	}
}

//nolint:gocritic // must follow the upstream InitFunc signature
func startRouteController(ctx context.Context,
	_ app.ControllerInitContext,
	controllerContext controllermanagerapp.ControllerContext,
	completedConfig *config.CompletedConfig,
	_ cloudprovider.Interface,
) (controller.Interface, bool, error) {
	cfg, err := LoadConfigFromEnv()
	if err != nil {
		return nil, false, err // fatal: fail fast (catches partial-env)
	}

	if cfg.RoutingMode != RoutingModeNative {
		klog.Infof("crusoe-route-controller disabled: CRUSOE_ROUTING_MODE=%q (native routing not enabled)",
			cfg.RoutingMode)

		return nil, false, nil
	}

	kubeClient, dynClient, err := buildClients(completedConfig)
	if err != nil {
		return nil, false, err
	}
	apiClient := buildAPIClient()

	// Resolve location from the cluster object (§5.1); non-fatal on error.
	clusterName := completedConfig.ComponentConfig.KubeCloudShared.ClusterName
	if loc, locErr := resolveLocationFromCluster(ctx, apiClient, cfg.ProjectID, clusterName); locErr != nil {
		klog.ErrorS(locErr, "crusoe-route-controller: cluster location lookup failed; "+
			"falling back to instance-derived location", "cluster", clusterName)
	} else {
		cfg.Location = loc
	}

	sdnClient := sdn.NewLoggingFakeClient() // the single line swapped when the real client lands

	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(
		dynClient, informerResync, metav1.NamespaceAll, nil)
	ciliumNodeInformer := factory.ForResource(ciliumNodeGVR)
	nodeInformer := completedConfig.SharedInformers.Core().V1().Nodes()

	rc, err := NewRouteController(cfg, kubeClient, dynClient, sdnClient, apiClient, ciliumNodeInformer, nodeInformer)
	if err != nil {
		return nil, false, err
	}

	factory.Start(ctx.Done())
	go rc.Run(ctx, controllerContext.ControllerManagerMetrics)

	return nil, true, nil
}

// buildClients constructs the kube and dynamic clients from the CCM kubeconfig
// (same precedent as the node-lifecycle controller).
func buildClients(
	completedConfig *config.CompletedConfig,
) (kube clientset.Interface, dyn dynamic.Interface, err error) {
	kube, err = clientset.NewForConfig(completedConfig.Kubeconfig)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create clientset from ccm kubeconfig: %w", err)
	}
	dyn, err = dynamic.NewForConfig(completedConfig.Kubeconfig)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create dynamic client from ccm kubeconfig: %w", err)
	}

	return kube, dyn, nil
}

// buildAPIClient constructs the Crusoe API client the same way internal/cloud.go
// does (the cloud parameter does not expose its client).
func buildAPIClient() client.APIClient {
	cc := auth.NewCrusoeClient(
		os.Getenv(cloudcontrollermanager.APIEndpoint),
		os.Getenv(cloudcontrollermanager.AccessKey),
		os.Getenv(cloudcontrollermanager.SecretKey),
		userAgent)

	return &client.APIClientImpl{CrusoeAPIClient: cc}
}
