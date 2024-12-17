package crusoe

import (
	"context"
	"errors"
	"sync"
	"time"

	crusoeapi "github.com/crusoecloud/client-go/swagger/v1alpha5"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/klog/v2"
)

const (
	FIVE           = 5
	ProviderPrefix = "crusoe://"
)

var ErrAssertTimeTypeFailed = errors.New("failed to assert type time.Time for firstSeen")

type Instances struct {
	CrusoeClient  *crusoeapi.APIClient
	nodeFirstSeen sync.Map
}

func (c *Instances) NodeAddresses(ctx context.Context, name types.NodeName) ([]v1.NodeAddress, error) {
	currInstance, err := getInstancebyName(ctx, c.CrusoeClient, string(name))
	if err != nil {
		return nil, err
	}

	return getNodeAddress(currInstance)
}

func (c *Instances) NodeAddressesByProviderID(ctx context.Context, providerID string) ([]v1.NodeAddress, error) {
	currInstance, responseBody, err := getInstanceByID(ctx, c.CrusoeClient, getInstanceIDFromProviderID(providerID))
	if responseBody != nil {
		defer responseBody.Body.Close()
	}
	if err != nil {
		return nil, err
	}
	address, err := getNodeAddress(currInstance)
	if err != nil {
		return nil, err
	}
	klog.Infof("NodeAddressesByProviderID(%v) and response address %v", providerID, address)

	return address, nil
}

func (c *Instances) InstanceID(ctx context.Context, nodeName types.NodeName) (string, error) {
	currInstance, err := getInstancebyName(ctx, c.CrusoeClient, string(nodeName))
	if err != nil {
		return "", err
	}

	return currInstance.Id, nil
}

func (c *Instances) InstanceType(ctx context.Context, name types.NodeName) (string, error) {
	currInstance, err := getInstancebyName(ctx, c.CrusoeClient, string(name))
	if err != nil {
		return "", err
	}
	klog.Infof("InstanceType(%v) is %v", name, currInstance.Type_)

	return currInstance.Type_, nil
}

func (c *Instances) InstanceTypeByProviderID(ctx context.Context, providerID string) (string, error) {
	currInstance, responseBody, err := getInstanceByID(ctx, c.CrusoeClient, getInstanceIDFromProviderID(providerID))
	if responseBody != nil {
		defer responseBody.Body.Close()
	}
	if err != nil {
		return "", err
	}
	klog.Infof("InstanceTypeByProviderID(%v) is %v", providerID, currInstance.Type_)

	return currInstance.Type_, nil
}

func (c *Instances) AddSSHKeyToAllInstances(_ context.Context, _ string, _ []byte) error {
	return cloudprovider.NotImplemented
}

func (c *Instances) CurrentNodeName(_ context.Context, hostname string) (types.NodeName, error) {
	return types.NodeName(hostname), nil
}

func (c *Instances) InstanceShutdownByProviderID(ctx context.Context, providerID string) (bool, error) {
	currInstance, responseBody, err := getInstanceByID(ctx, c.CrusoeClient, getInstanceIDFromProviderID(providerID))
	if responseBody != nil {
		defer responseBody.Body.Close()
	}
	if err != nil {
		return false, err
	}
	if currInstance == nil || currInstance.State == "STATE_SHUTOFF" || currInstance.State == "STATE_SHUTDOWN" {
		klog.Infof("Instance (%v) is Shutdown", providerID)

		return true, nil
	}

	return false, nil
}

func (c *Instances) InstanceShutdown(ctx context.Context, node *v1.Node) (bool, error) {
	providerID, err := getProviderID(ctx, node, c)
	if err != nil {
		return false, err
	}

	return c.InstanceShutdownByProviderID(ctx, providerID)
}

func (c *Instances) InstanceExistsByProviderID(ctx context.Context, providerID string) (bool, error) {
	_, responseBody, err := getInstanceByID(ctx, c.CrusoeClient, getInstanceIDFromProviderID(providerID))
	if responseBody != nil {
		defer responseBody.Body.Close()
	}
	if err != nil && responseBody != nil && responseBody.StatusCode != 404 {
		klog.Errorf("Error getting instance by ID: %v", err)

		return false, err
	}
	klog.Infof("InstanceExistsAPI Response(%v)", responseBody)
	currTime := time.Now()
	firstSeen, ok := c.nodeFirstSeen.Load(providerID)
	if !ok {
		c.nodeFirstSeen.Store(providerID, currTime)
		firstSeen = currTime
	}
	firstSeenTime, ok := firstSeen.(time.Time)
	if !ok {
		return false, ErrAssertTimeTypeFailed
	}
	timeDiff := currTime.Sub(firstSeenTime)
	if responseBody != nil && responseBody.StatusCode == 404 {
		if timeDiff < FIVE*time.Minute {
			klog.Infof("timediff: %v", timeDiff)
			klog.Infof("Node %v first seen less than 5 minute ago", providerID)

			return true, nil
		}
		klog.Infof("Node %v first seen more than 5 minute ago", providerID)

		return false, nil
	}
	c.nodeFirstSeen.Store(providerID, currTime)

	return true, nil
}

func (c *Instances) InstanceExists(ctx context.Context, node *v1.Node) (bool, error) {
	providerID, err := getProviderID(ctx, node, c)
	if err != nil {
		return false, err
	}

	return c.InstanceExistsByProviderID(ctx, providerID)
}

func (c *Instances) InstanceMetadata(ctx context.Context,
	node *v1.Node,
) (*cloudprovider.InstanceMetadata, error) {
	klog.Infof("Get Instance Metadata for (%v)", node.Name)
	currInstance, responseBody, err := getInstanceByID(ctx, c.CrusoeClient,
		getInstanceIDFromProviderID(node.Spec.ProviderID))
	if responseBody != nil {
		defer responseBody.Body.Close()
	}
	if err != nil {
		return nil, err
	}
	klog.Infof("InstanceMetadata for (%v:%v)", node.Name, currInstance)
	nodeAddress, err := getNodeAddress(currInstance)
	if err != nil {
		return nil, err
	}
	additionalLabels := make(map[string]string)
	if len(currInstance.HostChannelAdapters) > 0 {
		ibPartition, err := getIBNetwork(ctx, c.CrusoeClient, currInstance.ProjectId,
			currInstance.HostChannelAdapters[0].IbPartitionId)
		klog.Infof("ibPartition: %v", ibPartition)
		if err != nil {
			return nil, err
		}
		if ibPartition != nil {
			additionalLabels["crusoe.ai/ib.partition.name"] = ibPartition.Name
			additionalLabels["crusoe.ai/ib.partition.id"] = ibPartition.Id
			additionalLabels["crusoe.ai/ib.partition.networkId"] = ibPartition.IbNetworkId
		}
	}
	additionalLabels["crusoe.ai/instance.id"] = currInstance.Id
	additionalLabels["crusoe.ai/instance.group.id"] = currInstance.InstanceGroupId
	additionalLabels["crusoe.ai/instance.template.id"] = currInstance.InstanceTemplateId
	additionalLabels["crusoe.ai/instance.state"] = currInstance.State
	metadata := cloudprovider.InstanceMetadata{
		ProviderID:       ProviderPrefix + currInstance.Id,
		InstanceType:     currInstance.Type_,
		Region:           currInstance.Location,
		AdditionalLabels: additionalLabels,
		NodeAddresses:    nodeAddress,
	}
	klog.Infof("InstanceMetadata for (%v:%v)", node.Name, metadata)

	return &metadata, nil
}

func NewCrusoeInstances(c *crusoeapi.APIClient) *Instances {
	return &Instances{
		CrusoeClient: c,
	}
}

func getProviderID(ctx context.Context, node *v1.Node, i *Instances) (string, error) {
	providerID := node.Spec.ProviderID
	if providerID == "" {
		currInstance, err := getInstancebyName(ctx, i.CrusoeClient, node.Name)
		if err != nil {
			return "", err
		}
		providerID = ProviderPrefix + currInstance.Id
	}

	return providerID, nil
}

func getInstanceIDFromProviderID(providerID string) string {
	return providerID[len(ProviderPrefix):]
}

func getNodeAddress(currInstance *crusoeapi.InstanceV1Alpha5) ([]v1.NodeAddress, error) {
	var nodeAddress []v1.NodeAddress
	nodeAddress = append(nodeAddress, v1.NodeAddress{
		Type:    v1.NodeInternalIP,
		Address: currInstance.NetworkInterfaces[0].Ips[0].PrivateIpv4.Address,
	}, v1.NodeAddress{
		Type:    v1.NodeExternalIP,
		Address: currInstance.NetworkInterfaces[0].Ips[0].PublicIpv4.Address,
	}, v1.NodeAddress{
		Type:    v1.NodeHostName,
		Address: currInstance.Name,
	},
	)

	return nodeAddress, nil
}
