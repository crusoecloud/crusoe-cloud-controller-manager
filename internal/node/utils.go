package node

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/cloud-provider/app"
	"k8s.io/cloud-provider/app/config"
	controllermanagerapp "k8s.io/controller-manager/app"
	"k8s.io/controller-manager/controller"
	"k8s.io/klog/v2"
)

// StartCloudNodeLifecycleControllerWrapper is used to take cloud config as input
// and start cloud node lifecycle controller.
func StartCloudNodeLifecycleControllerWrapper(initContext app.ControllerInitContext,
	completedConfig *config.CompletedConfig,
	cloud cloudprovider.Interface,
) app.InitFunc {
	return func(ctx context.Context,
		controllerContext controllermanagerapp.ControllerContext,
	) (controller.Interface, bool, error) {
		return startCloudNodeLifecycleController(ctx, initContext, controllerContext, completedConfig, cloud)
	}
}

//nolint:gocritic // need to follow upstream function signature
func startCloudNodeLifecycleController(ctx context.Context,
	initContext app.ControllerInitContext,
	controlexContext controllermanagerapp.ControllerContext,
	completedConfig *config.CompletedConfig,
	cloud cloudprovider.Interface,
) (controller.Interface, bool, error) {
	// Start the cloudNodeLifecycleController
	cloudNodeLifecycleController, err := NewCloudNodeLifecycleController(
		completedConfig.SharedInformers.Core().V1().Nodes(),
		// cloud node lifecycle controller uses existing cluster role from node-controller
		completedConfig.ClientBuilder.ClientOrDie(initContext.ClientName),
		cloud,
		completedConfig.ComponentConfig.KubeCloudShared.NodeMonitorPeriod.Duration,
	)
	if err != nil {
		klog.Warningf("failed to start cloud node lifecycle controller: %s", err)

		return nil, false, nil
	}

	go cloudNodeLifecycleController.Run(ctx, controlexContext.ControllerManagerMetrics)

	return nil, true, nil
}

func CleanUpVolumeAttachmentsForNode(ctx context.Context, kubeClient clientset.Interface, nodeName string) error {
	volumeAttachments, listErr := kubeClient.StorageV1().VolumeAttachments().List(ctx,
		metav1.ListOptions{FieldSelector: fmt.Sprintf("spec.nodeName=%s", nodeName)})

	if listErr != nil {
		return fmt.Errorf("failed to list volume attachments for node %s: %w", nodeName, listErr)
	}

	for index := range len(volumeAttachments.Items) {
		volumeAttachment := volumeAttachments.Items[index]
		deleteErr := kubeClient.StorageV1().VolumeAttachments().Delete(ctx, volumeAttachment.Name, metav1.DeleteOptions{})
		if deleteErr != nil {
			klog.Errorf("failed to delete volume attachment %s for node %s: %v",
				volumeAttachment.Name, nodeName, deleteErr)
		}
	}

	return nil
}
