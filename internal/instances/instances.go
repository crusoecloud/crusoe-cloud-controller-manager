package instances

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	crusoeapi "github.com/crusoecloud/client-go/swagger/v1alpha5"
	"github.com/crusoecloud/crusoe-cloud-controller-manager/internal/client"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/klog/v2"
)

const (
	InstanceNotFoundInterval = 2 * time.Minute
	ProviderPrefix           = "crusoe://"
)

var (
	ErrAssertTimeTypeFailed = errors.New("failed to assert type time.Time for firstSeen")
	ErrNoInstance           = errors.New("no instance returned by Crusoe Cloud")
	ErrNoNodeAddress        = errors.New("instance has no network interface with a private IPv4 address")
)

type Instances struct {
	nodeFirstSeen sync.Map
	nodeShutdown  sync.Map
	apiClient     client.APIClient
}

func (i *Instances) NodeAddresses(ctx context.Context, name types.NodeName) ([]v1.NodeAddress, error) {
	currInstance, err := i.apiClient.GetInstanceByName(ctx, string(name))
	if err != nil {
		return nil, fmt.Errorf("failed to get instance by name %s: %w", name, err)
	}

	return getNodeAddress(currInstance)
}

func (i *Instances) NodeAddressesByProviderID(ctx context.Context, providerID string) ([]v1.NodeAddress, error) {
	currInstance, responseBody, err := i.apiClient.GetInstanceByID(ctx, getInstanceIDFromProviderID(providerID))
	if responseBody != nil {
		defer responseBody.Body.Close()
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get instance by provider ID %s: %w", providerID, err)
	}
	address, err := getNodeAddress(currInstance)
	if err != nil {
		return nil, fmt.Errorf("failed to get node address for instance %s: %w", currInstance.Id, err)
	}
	klog.Infof("NodeAddressesByProviderID(%v) and response address %v", providerID, address)

	return address, nil
}

func (i *Instances) InstanceID(ctx context.Context, nodeName types.NodeName) (string, error) {
	currInstance, err := i.apiClient.GetInstanceByName(ctx, string(nodeName))
	if err != nil {
		return "", fmt.Errorf("failed to get instance by name %s: %w", nodeName, err)
	}

	return currInstance.Id, nil
}

func (i *Instances) InstanceType(ctx context.Context, name types.NodeName) (string, error) {
	currInstance, err := i.apiClient.GetInstanceByName(ctx, string(name))
	if err != nil {
		return "", fmt.Errorf("failed to get instance by name %s: %w", name, err)
	}
	klog.Infof("InstanceType(%v) is %v", name, currInstance.Type_)

	return currInstance.Type_, nil
}

func (i *Instances) InstanceTypeByProviderID(ctx context.Context, providerID string) (string, error) {
	currInstance, responseBody, err := i.apiClient.GetInstanceByID(ctx, getInstanceIDFromProviderID(providerID))
	if responseBody != nil {
		defer responseBody.Body.Close()
	}
	if err != nil {
		return "", fmt.Errorf("failed to get instance by provider ID %s: %w", providerID, err)
	}
	klog.Infof("InstanceTypeByProviderID(%v) is %v", providerID, currInstance.Type_)

	return currInstance.Type_, nil
}

func (i *Instances) AddSSHKeyToAllInstances(_ context.Context, _ string, _ []byte) error {
	return cloudprovider.NotImplemented
}

func (i *Instances) CurrentNodeName(_ context.Context, hostname string) (types.NodeName, error) {
	return types.NodeName(hostname), nil
}

func (i *Instances) InstanceShutdownByProviderID(ctx context.Context, providerID string) (bool, error) {
	currInstance, responseBody, err := i.apiClient.GetInstanceByID(ctx, getInstanceIDFromProviderID(providerID))
	if responseBody != nil {
		defer responseBody.Body.Close()
	}
	if err != nil {
		if errors.Is(err, client.ErrInstanceNotFound) {
			return i.handleInstanceNotFoundErr(providerID, err)
		}

		return false, fmt.Errorf("failed to get instance by provider ID %s: %w", providerID, err)
	}
	if currInstance == nil || currInstance.State == "STATE_SHUTOFF" || currInstance.State == "STATE_SHUTDOWN" {
		klog.Infof("Instance (%v) is Shutdown", providerID)

		return true, nil
	}

	return false, nil
}

func (i *Instances) InstanceShutdown(ctx context.Context, node *v1.Node) (bool, error) {
	providerID, err := getProviderID(ctx, node, i)
	if err != nil {
		return false, err
	}

	return i.InstanceShutdownByProviderID(ctx, providerID)
}

//nolint:cyclop // must perform all checks before returning instance does not exist
func (i *Instances) InstanceExistsByProviderID(ctx context.Context, providerID string) (bool, error) {
	inst, responseBody, err := i.apiClient.GetInstanceByID(ctx, getInstanceIDFromProviderID(providerID))
	if responseBody != nil {
		defer responseBody.Body.Close()
	}
	if err != nil && responseBody != nil && responseBody.StatusCode != 404 {
		klog.Errorf("Error getting instance by ID: %v", err)

		return false, fmt.Errorf("failed to get instance by ID %s: %w", providerID, err)
	}
	klog.Infof("InstanceExistsAPI Response(%v)", responseBody)
	currTime := time.Now()
	firstSeen, ok := i.nodeFirstSeen.Load(providerID)
	if !ok {
		i.nodeFirstSeen.Store(providerID, currTime)
		firstSeen = currTime
	}
	firstSeenTime, ok := firstSeen.(time.Time)
	if !ok {
		// update the in-memory state to current time so that we can process it in next iteration
		i.nodeFirstSeen.Store(providerID, currTime)
		firstSeenTime = currTime
	}
	timeDiff := currTime.Sub(firstSeenTime)
	if inst == nil || (responseBody != nil && responseBody.StatusCode == 404) {
		if timeDiff < InstanceNotFoundInterval {
			klog.Infof("Node %v last seen: %v", providerID, timeDiff)
			klog.Infof("Node %v not seen for less than 2 minutes", providerID)

			return true, nil
		}
		klog.Infof("Node %v not seen for more than 2 minutes", providerID)

		return false, nil
	}
	i.nodeFirstSeen.Store(providerID, currTime)

	return true, nil
}

func (i *Instances) InstanceExists(ctx context.Context, node *v1.Node) (bool, error) {
	providerID, err := getProviderID(ctx, node, i)
	if err != nil {
		return false, err
	}

	return i.InstanceExistsByProviderID(ctx, providerID)
}

func (i *Instances) InstanceMetadata(ctx context.Context, node *v1.Node) (*cloudprovider.InstanceMetadata, error) {
	klog.Infof("Get Instance Metadata for (%v)", node.Name)
	prefixedProviderID, err := getProviderID(ctx, node, i)
	if err != nil {
		klog.Errorf("could not get provider ID from Crusoe Cloud %v", err)

		return nil, err
	}
	providerID := getInstanceIDFromProviderID(prefixedProviderID)
	currInstance, responseBody, err := i.apiClient.GetInstanceByID(ctx, providerID)
	if responseBody != nil {
		defer responseBody.Body.Close()
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get instance by ID %s: %w", providerID, err)
	}
	klog.Infof("InstanceMetadata for (%v:%v)", node.Name, currInstance)
	nodeAddress, err := getNodeAddress(currInstance)
	if err != nil {
		return nil, fmt.Errorf("failed to get node address for instance %s: %w", currInstance.Id, err)
	}
	additionalLabels := make(map[string]string)
	if len(currInstance.HostChannelAdapters) > 0 {
		ibPartition, err := i.apiClient.GetIBNetwork(ctx, currInstance.ProjectId,
			currInstance.HostChannelAdapters[0].IbPartitionId)
		klog.Infof("ibPartition: %v", ibPartition)
		if err != nil {
			return nil, fmt.Errorf("failed to get IB network for instance %s: %w", currInstance.Id, err)
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
	additionalLabels["crusoe.ai/pod.id"] = currInstance.PodId
	addUnitAndWaypointLabels(currInstance, additionalLabels)
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

func NewCrusoeInstances(c client.APIClient) *Instances {
	return &Instances{
		apiClient: c,
	}
}

func getProviderID(ctx context.Context, node *v1.Node, i *Instances) (string, error) {
	providerID := node.Spec.ProviderID
	// While kubelet does not update the node.spec or the node.status.addresses or metadata fields when
	// node information changes it is still able to update the nodeInfo field in node.status. Kubelet updates
	// node.Status.NodeInfo.SystemUUID to /sys/class/dmi/id/product_uuid which is the correct VM ID in Crusoe Cloud
	if node.Status.NodeInfo.SystemUUID != "" &&
		getInstanceIDFromProviderID(node.Spec.ProviderID) != node.Status.NodeInfo.SystemUUID {

		klog.Warningf("ProviderID and SystemUUID do not match; providerID: "+
			"%s; systemUUID: %s. Fetching UUID from Crusoe Cloud directly.",
			providerID, node.Status.NodeInfo.SystemUUID)
		providerID = ""
	}
	if providerID == "" {
		currInstance, err := i.apiClient.GetInstanceByName(ctx, node.Name)
		if err != nil {
			return "", fmt.Errorf("failed to get instance by Name %s: %w", node.Name, err)
		}
		providerID = ProviderPrefix + currInstance.Id
	}

	return providerID, nil
}

// addUnitAndWaypointLabels adds the rack unit and leaf switch (waypoint) IDs reported by the
// VM API, if present. Both fields are optional: older API versions omit them entirely, and the
// region can serve them empty when the underlying node labels have not been written yet.
// A waypoint ID is one UUID (36 chars) so it always fits a label value, but the IDs must not be
// joined into a single value since two would exceed the 63 char limit for label values.
//
// Note that these labels are only ever applied while the node still carries the uninitialized
// cloud provider taint, and cloud-provider discards a key that is already present on the node.
// A value that changes after provisioning, for example a re-cabled node reporting a different
// waypoint, will therefore not be reflected. That is a property of AdditionalLabels shared by
// every label set here, not specific to these two fields.
func addUnitAndWaypointLabels(currInstance *crusoeapi.InstanceV1Alpha5, additionalLabels map[string]string) {
	if unitID := strings.TrimSpace(currInstance.UnitId); unitID != "" {
		additionalLabels["crusoe.ai/unit.id"] = unitID
	}
	waypointIdx := 1
	for _, waypointIDs := range currInstance.WaypointIds {
		// The API models this as one ID per element, but the node label the values originate from
		// holds them comma separated and that split happens region side, so a raw pass-through
		// would silently produce one over-long, and therefore rejected, label value. Splitting
		// here keeps both shapes correct for the cost of one loop.
		for _, waypointID := range strings.Split(waypointIDs, ",") {
			if waypointID = strings.TrimSpace(waypointID); waypointID != "" {
				additionalLabels[fmt.Sprintf("crusoe.ai/waypoint.%d.id", waypointIdx)] = waypointID
				waypointIdx++
			}
		}
	}
}

func getInstanceIDFromProviderID(providerID string) string {
	return strings.TrimPrefix(providerID, ProviderPrefix)
}

// getNodeAddress builds the node address list from the first network interface of the instance.
// A stopped instance is served with its network interfaces detached, so every index has to be
// checked: the API can return an instance with no interfaces, with no IPs on the first interface,
// or with no public IPv4 at all for instances that were never given one.
// Missing IPs are reported as an error rather than returned as a partial list, because the node
// status update overwrites node.Status.Addresses wholesale and dropping the internal IP of a
// stopped node breaks everything that routes to it. On an error the node controller logs and
// leaves the addresses it already has in place.
func getNodeAddress(currInstance *crusoeapi.InstanceV1Alpha5) ([]v1.NodeAddress, error) {
	if currInstance == nil {
		return nil, ErrNoInstance
	}
	if len(currInstance.NetworkInterfaces) == 0 || len(currInstance.NetworkInterfaces[0].Ips) == 0 {
		return nil, fmt.Errorf("%w: instance %s is in state %s", ErrNoNodeAddress, currInstance.Id, currInstance.State)
	}
	ips := currInstance.NetworkInterfaces[0].Ips[0]
	if ips.PrivateIpv4 == nil || ips.PrivateIpv4.Address == "" {
		return nil, fmt.Errorf("%w: instance %s is in state %s", ErrNoNodeAddress, currInstance.Id, currInstance.State)
	}
	nodeAddress := []v1.NodeAddress{{
		Type:    v1.NodeInternalIP,
		Address: ips.PrivateIpv4.Address,
	}}
	// A dynamically allocated public IP is released while the instance is stopped, so the
	// address object can be present with an empty address. Publishing that as an ExternalIP
	// would put a blank address on the node, so only a real address is reported.
	if ips.PublicIpv4 != nil && ips.PublicIpv4.Address != "" {
		nodeAddress = append(nodeAddress, v1.NodeAddress{
			Type:    v1.NodeExternalIP,
			Address: ips.PublicIpv4.Address,
		})
	}

	return append(nodeAddress, v1.NodeAddress{
		Type:    v1.NodeHostName,
		Address: fmt.Sprintf("%s.%s.compute.internal", currInstance.Name, currInstance.Location),
	}), nil
}

func (i *Instances) handleInstanceNotFoundErr(providerID string, orignalErr error) (instanceShutdown bool, err error) {
	attempt, ok := i.nodeShutdown.Load(providerID)
	if !ok {
		i.nodeShutdown.Store(providerID, 1)

		return false, orignalErr
	}
	intAttempt, ok := attempt.(int)
	if !ok {
		// store correct data to avoid conversion issues in next iteration
		i.nodeShutdown.Store(providerID, 1)

		return false, orignalErr
	}
	if intAttempt >= 3 {
		return true, nil
	}
	i.nodeShutdown.Store(providerID, (intAttempt + 1))

	return false, orignalErr
}
