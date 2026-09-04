package routes

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/cloud-provider/app"
	"k8s.io/cloud-provider/app/config"
	controllermanagerapp "k8s.io/controller-manager/app"
	"k8s.io/controller-manager/controller"
	"k8s.io/klog/v2"
)

const (
	informerResync = 30 * time.Minute
	// crdPollInterval is how often the mirror re-checks discovery for the
	// CiliumNode CRD before starting its informer.
	crdPollInterval = 30 * time.Second
)

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
	initContext app.ControllerInitContext,
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

	// Both clients come from the ClientBuilder so they carry this controller's
	// ClientName (its service account and user agent), set in main.go.
	kubeClient := controllerContext.ClientBuilder.ClientOrDie(initContext.ClientName)
	dynClient, err := dynamic.NewForConfig(controllerContext.ClientBuilder.ConfigOrDie(initContext.ClientName))
	if err != nil {
		return nil, false, fmt.Errorf("failed to create dynamic client for %s: %w", initContext.ClientName, err)
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

	go func() {
		// A brand-new cluster has neither nodes nor cilium, so the CiliumNode
		// CRD may not exist yet; starting the dynamic informer then makes its
		// reflector spam list/watch errors until cilium is installed. Gate the
		// start on the CRD being served (self-heals once cilium lands).
		if err := waitForCiliumNodeCRD(ctx, kubeClient.Discovery()); err != nil {
			klog.ErrorS(err, "crusoe-podcidr-mirror: stopped before the CiliumNode CRD was served")

			return
		}
		factory.Start(ctx.Done())
		mirror.Run(ctx, controllerContext.ControllerManagerMetrics)
	}()

	return nil, true, nil
}

// waitForCiliumNodeCRD blocks until the cilium.io/v2 ciliumnodes resource is
// served by the API server, polling discovery every crdPollInterval. It returns
// nil once the resource exists, or ctx's error on cancellation.
func waitForCiliumNodeCRD(ctx context.Context, disco discovery.DiscoveryInterface) error {
	logged := false

	//nolint:wrapcheck // ctx cancellation is the only error path, wrapping adds nothing
	return wait.PollUntilContextCancel(ctx, crdPollInterval, true,
		func(context.Context) (bool, error) {
			rl, err := disco.ServerResourcesForGroupVersion(ciliumNodeGVR.GroupVersion().String())
			if err != nil {
				// NotFound until the CRD is installed; anything else gets the
				// same patient retry.
				if !logged {
					klog.Infof("crusoe-podcidr-mirror: waiting for the %s CRD to be served: %v",
						ciliumNodeGVR.GroupVersion(), err)
					logged = true
				}

				return false, nil
			}
			for i := range rl.APIResources {
				if rl.APIResources[i].Name == ciliumNodeGVR.Resource {
					return true, nil
				}
			}

			return false, nil
		})
}
