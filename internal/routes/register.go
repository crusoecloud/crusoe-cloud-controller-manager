package routes

import (
	"context"
	"fmt"
	"time"

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

const informerResync = 30 * time.Minute

//nolint:gochecknoglobals // GVR is effectively a constant
var ciliumNodeGVR = schema.GroupVersionResource{
	Group: "cilium.io", Version: "v2", Resource: "ciliumnodes",
}

// StartPodCIDRMirrorWrapper adapts startPodCIDRMirror to the cloud-provider app
// InitFunc constructor signature (mirrors internal/node/utils.go).
func StartPodCIDRMirrorWrapper(initContext app.ControllerInitContext,
	completedConfig *config.CompletedConfig,
	cloud cloudprovider.Interface,
) app.InitFunc {
	return func(ctx context.Context,
		controllerContext controllermanagerapp.ControllerContext,
	) (controller.Interface, bool, error) {
		return startPodCIDRMirror(ctx, initContext, controllerContext, completedConfig, cloud)
	}
}

//nolint:gocritic // must follow the upstream InitFunc signature
func startPodCIDRMirror(ctx context.Context,
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
		klog.Infof("crusoe-podcidr-mirror disabled: CRUSOE_ROUTING_MODE=%q (native routing not enabled)",
			cfg.RoutingMode)

		return nil, false, nil
	}

	kubeClient, err := clientset.NewForConfig(completedConfig.Kubeconfig)
	if err != nil {
		return nil, false, fmt.Errorf("failed to create clientset from ccm kubeconfig: %w", err)
	}
	dynClient, err := dynamic.NewForConfig(completedConfig.Kubeconfig)
	if err != nil {
		return nil, false, fmt.Errorf("failed to create dynamic client from ccm kubeconfig: %w", err)
	}

	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(
		dynClient, informerResync, metav1.NamespaceAll, nil)
	ciliumNodeInformer := factory.ForResource(ciliumNodeGVR)
	nodeInformer := completedConfig.SharedInformers.Core().V1().Nodes()

	mirror, err := NewPodCIDRMirror(
		kubeClient,
		ciliumNodeInformer.Informer(), ciliumNodeInformer.Lister(),
		nodeInformer.Informer(), nodeInformer.Lister(),
	)
	if err != nil {
		return nil, false, err
	}

	factory.Start(ctx.Done())
	go mirror.Run(ctx, controllerContext.ControllerManagerMetrics)

	return nil, true, nil
}
