package client

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

type APIClientImpl struct {
	CrusoeAPIClient *crusoeapi.APIClient
}

type APIClient interface {
	GetInstanceByName(ctx context.Context, nodeName string) (*crusoeapi.InstanceV1Alpha5, error)
	GetIBNetwork(ctx context.Context, projectID, ibPartitionID string) (*crusoeapi.IbPartition, error)
	GetInstanceByID(ctx context.Context, instanceID string) (*crusoeapi.InstanceV1Alpha5, *http.Response, error)
}

func (a *APIClientImpl) GetInstanceByName(ctx context.Context, nodeName string,
) (*crusoeapi.InstanceV1Alpha5, error) {
	projectID := os.Getenv(CrusoeProjectID)
	if projectID == "" {
		return nil, ErrProjectIDNotSet
	}
	instanceName := strings.Split(nodeName, ".")[0]

	listVMOpts := &crusoeapi.VMsApiListInstancesOpts{
		Names: optional.NewString(instanceName),
	}
	instances, instancesHTTPResp, instancesErr := a.CrusoeAPIClient.VMsApi.ListInstances(ctx, projectID, listVMOpts)
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

func (a *APIClientImpl) GetIBNetwork(ctx context.Context,
	projectID, ibPartitionID string,
) (*crusoeapi.IbPartition, error) {
	ibPartition, response, err := a.CrusoeAPIClient.IBPartitionsApi.GetIBPartition(ctx, projectID, ibPartitionID)
	if err != nil {
		return nil, fmt.Errorf("failed to list instances: %w", err)
	}
	if response != nil {
		defer response.Body.Close()
	}
	klog.Infof("getIBNetwork: %v", ibPartition)

	return &ibPartition, nil
}

func (a *APIClientImpl) GetInstanceByID(ctx context.Context,
	instanceID string,
) (*crusoeapi.InstanceV1Alpha5, *http.Response, error) {
	projectID := os.Getenv(CrusoeProjectID)
	if projectID == "" {
		return nil, nil, ErrProjectIDNotSet
	}

	klog.Infof("getInstanceByID: %s", instanceID)
	listVMOpts := &crusoeapi.VMsApiListInstancesOpts{
		Ids: optional.NewString(instanceID),
	}
	instances, response, err := a.CrusoeAPIClient.VMsApi.ListInstances(ctx, projectID, listVMOpts)
	if err != nil {
		return nil, response, fmt.Errorf("failed to list instances: %w", err)
	}
	if response != nil {
		defer response.Body.Close()
	}
	klog.Infof("getInstanceByID: %v", instances)
	if len(instances.Items) == 0 {
		return nil, nil, ErrInstanceNotFound
	}

	return &instances.Items[0], response, nil
}
