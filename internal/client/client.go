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
	CrusoeProjectID  = "CRUSOE_PROJECT_ID"
	CrusoeClusterID  = "CRUSOE_CLUSTER_ID"
	CrusoeAPIBaseURL = "CRUSOE_API_ENDPOINT"
)

var (
	ErrInstanceNotFound  = errors.New("instance not found")
	ErrProjectIDNotSet   = errors.New("CRUSOE_PROJECT_ID environment variable is not set")
	ErrClusterIDNotSet   = errors.New("CRUSOE_CLUSTER_ID environment variable is not set")
	ErrAPIEndpointNotSet = errors.New("CRUSOE_API_ENDPOINT environment variable is not set")
)

type APIClientImpl struct {
	CrusoeAPIClient *crusoeapi.APIClient
}

// NodePool represents a Kubernetes node pool from the Crusoe API.
type NodePool struct {
	ID         string
	Name       string
	NodeLabels map[string]string
}

type APIClient interface {
	GetInstanceByName(ctx context.Context, nodeName string) (*crusoeapi.InstanceV1Alpha5, error)
	GetIBNetwork(ctx context.Context, projectID, ibPartitionID string) (*crusoeapi.IbPartition, error)
	GetInstanceByID(ctx context.Context, instanceID string) (*crusoeapi.InstanceV1Alpha5, *http.Response, error)
	ListNodePools(ctx context.Context, clusterID string) ([]NodePool, error)
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

func (a *APIClientImpl) ListNodePools(ctx context.Context, clusterID string) ([]NodePool, error) {
	projectID := os.Getenv(CrusoeProjectID)
	if projectID == "" {
		return nil, ErrProjectIDNotSet
	}

	opts := &crusoeapi.KubernetesNodePoolsApiListNodePoolsOpts{
		ClusterId: optional.NewString(clusterID),
	}

	resp, httpResp, err := a.CrusoeAPIClient.KubernetesNodePoolsApi.ListNodePools(ctx, projectID, opts)
	if httpResp != nil {
		defer httpResp.Body.Close()
	}
	if err != nil {
		return nil, fmt.Errorf("failed to list node pools: %w", err)
	}

	nodePools := make([]NodePool, len(resp.Items))
	for i, np := range resp.Items {
		nodePools[i] = NodePool{
			ID:         np.Id,
			Name:       np.Name,
			NodeLabels: np.NodeLabels,
		}
	}

	klog.V(4).Infof("ListNodePools: found %d node pools for cluster %s", len(nodePools), clusterID)

	return nodePools, nil
}
