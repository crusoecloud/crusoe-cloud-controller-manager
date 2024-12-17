package crusoe

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/antihax/optional"
	crusoeapi "github.com/crusoecloud/client-go/swagger/v1alpha5"
	"k8s.io/klog/v2"
)

const (
	CrusoeProjectID = "CRUSOE_PROJECT_ID"
)

var (
	ErrInstanceNotFound = errors.New("instance not found")
	ErrProjectIDNotSet  = errors.New("CRUSOE_PROJECT_ID environment variable is not set")
)

// getInstancebyName retrieves an instance by its name from the Crusoe API.
func getInstancebyName(ctx context.Context, client *crusoeapi.APIClient, nodeName string,
) (*crusoeapi.InstanceV1Alpha5, error) {
	projectID := os.Getenv(CrusoeProjectID)
	if projectID == "" {
		return nil, ErrProjectIDNotSet
	}
	instanceName := strings.Split(nodeName, ".")[0]

	listVMOpts := &crusoeapi.VMsApiListInstancesOpts{
		Names: optional.NewString(instanceName),
	}
	instances, instancesHTTPResp, instancesErr := client.VMsApi.ListInstances(ctx, projectID, listVMOpts)
	if instancesHTTPResp != nil {
		defer instancesHTTPResp.Body.Close()
	}
	if instancesErr != nil {
		return nil, fmt.Errorf("failed to list instances: %w", instancesErr)
	}

	if len(instances.Items) == 0 {
		return nil, ErrInstanceNotFound
	}
	klog.Infof("getInstancebyName: %v", instances.Items[0])

	return &instances.Items[0], nil
}

func getIBNetwork(ctx context.Context, client *crusoeapi.APIClient,
	projectID, ibPartitionID string,
) (*crusoeapi.IbPartition, error) {
	ibPartition, response, err := client.IBPartitionsApi.GetIBPartition(ctx, projectID, ibPartitionID)
	if err != nil {
		return nil, fmt.Errorf("failed to list instances: %w", err)
	}
	if response != nil {
		defer response.Body.Close()
	}
	klog.Infof("getIBNetwork: %v", ibPartition)

	return &ibPartition, nil
}

func getInstanceByID(ctx context.Context, client *crusoeapi.APIClient,
	providerID string,
) (*crusoeapi.InstanceV1Alpha5, *http.Response, error) {
	projectID := os.Getenv(CrusoeProjectID)
	if projectID == "" {
		return nil, nil, ErrProjectIDNotSet
	}

	klog.Infof("getInstanceByID: %s", providerID)
	instance, response, err := client.VMsApi.GetInstance(ctx, projectID, providerID)
	if err != nil {
		return nil, response, fmt.Errorf("failed to list instances: %w", err)
	}
	if response != nil {
		defer response.Body.Close()
	}
	klog.Infof("getInstanceByID: %v", instance)

	return &instance, response, nil
}
