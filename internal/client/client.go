package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	HTTPClient      *http.Client
}

// NodePool represents a Kubernetes node pool from the Crusoe API.
type NodePool struct {
	ID              string
	Name            string
	InstanceGroupID string
	NodeLabels      map[string]string
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

// nodePoolAPIResponse represents the response from the nodepool list API.
type nodePoolAPIResponse struct {
	NodePools []nodePoolItem `json:"node_pools"`
}

type nodePoolItem struct {
	ID              string            `json:"id"`
	Name            string            `json:"name"`
	InstanceGroupID string            `json:"instance_group_id"`
	NodeLabels      map[string]string `json:"node_labels"`
}

func (a *APIClientImpl) ListNodePools(ctx context.Context, clusterID string) ([]NodePool, error) {
	projectID := os.Getenv(CrusoeProjectID)
	if projectID == "" {
		return nil, ErrProjectIDNotSet
	}

	apiEndpoint := os.Getenv(CrusoeAPIBaseURL)
	if apiEndpoint == "" {
		return nil, ErrAPIEndpointNotSet
	}

	url := fmt.Sprintf("%s/kubernetes/node-pools?cluster_ids=%s&project_ids=%s",
		strings.TrimSuffix(apiEndpoint, "/"), clusterID, projectID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := a.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to list node pools: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("failed to list node pools: status %d, body: %s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	var apiResp nodePoolAPIResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	nodePools := make([]NodePool, len(apiResp.NodePools))
	for i, np := range apiResp.NodePools {
		nodePools[i] = NodePool{
			ID:              np.ID,
			Name:            np.Name,
			InstanceGroupID: np.InstanceGroupID,
			NodeLabels:      np.NodeLabels,
		}
	}

	klog.V(4).Infof("ListNodePools: found %d node pools for cluster %s", len(nodePools), clusterID)

	return nodePools, nil
}
