package instances_test

import (
	"context"
	"os"
	"testing"

	v1alpha5 "github.com/crusoecloud/client-go/swagger/v1alpha5"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	mock_client "github.com/crusoecloud/crusoe-cloud-controller-manager/internal/client/mock"
	"github.com/crusoecloud/crusoe-cloud-controller-manager/internal/instances"
)

const (
	TESTInstanceID   = "2480b2f8-d63a-401e-90ff-0d79b5b3e007"
	TESTNodeName     = "node1"
	TestInstanceType = "c1a.2x"
	TESTHostname     = "testhost-1"
	ProviderIDPrefix = "crusoe://"
	CrusoeProjectID  = "CRUSOE_PROJECT_ID"
	TestProjectID    = "1841af90-a4f6-4412-8b23-b7035a6c72ae"
)

func TestNodeAddresses(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	// Set the CRUSOE_PROJECT_ID environment variable
	os.Setenv(CrusoeProjectID, TestProjectID)

	mockClient := mock_client.NewMockApiClient(ctrl)
	instanceService := instances.NewCrusoeInstances(mockClient)

	mockClient.EXPECT().GetInstanceByName(gomock.Any(), "node1").Return(&v1alpha5.InstanceV1Alpha5{
		NetworkInterfaces: []v1alpha5.NetworkInterface{
			{
				Ips: []v1alpha5.IpAddresses{
					{
						PrivateIpv4: &v1alpha5.PrivateIpv4Address{Address: "10.0.0.1"},
						PublicIpv4:  &v1alpha5.PublicIpv4Address{Address: "192.168.0.1"},
					},
				},
			},
		},
		Name: "node1",
	}, nil)

	addresses, err := instanceService.NodeAddresses(context.Background(), types.NodeName(TESTNodeName))
	require.NoError(t, err)
	require.Len(t, addresses, 3)
	require.Equal(t, "10.0.0.1", addresses[0].Address)
	require.Equal(t, "192.168.0.1", addresses[1].Address)
	require.Equal(t, "node1", addresses[2].Address)
}

func TestInstanceID(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockClient := mock_client.NewMockApiClient(ctrl)
	instanceService := instances.NewCrusoeInstances(mockClient)
	mockClient.EXPECT().GetInstanceByName(gomock.Any(), TESTNodeName).Return(&v1alpha5.InstanceV1Alpha5{
		Id: TESTInstanceID,
	}, nil)

	instanceID, err := instanceService.InstanceID(context.Background(), types.NodeName(TESTNodeName))
	require.NoError(t, err)
	require.Equal(t, TESTInstanceID, instanceID)
}

func TestInstanceExistsByProviderID(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockClient := mock_client.NewMockApiClient(ctrl)
	instanceService := instances.NewCrusoeInstances(mockClient)

	mockClient.EXPECT().GetInstanceByID(gomock.Any(), TESTInstanceID).Return(&v1alpha5.InstanceV1Alpha5{}, nil, nil)

	exists, err := instanceService.InstanceExistsByProviderID(context.Background(), ProviderIDPrefix+TESTInstanceID)
	require.NoError(t, err)
	require.True(t, exists)
}

func TestInstanceShutdownByProviderID(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockClient := mock_client.NewMockApiClient(ctrl)
	instanceService := instances.NewCrusoeInstances(mockClient)

	mockClient.EXPECT().GetInstanceByID(gomock.Any(), TESTInstanceID).Return(&v1alpha5.InstanceV1Alpha5{
		State: "STATE_SHUTOFF",
	}, nil, nil)

	shutdown, err := instanceService.InstanceShutdownByProviderID(context.Background(), ProviderIDPrefix+TESTInstanceID)
	require.NoError(t, err)
	require.True(t, shutdown)
}

func TestInstanceMetadata(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockClient := mock_client.NewMockApiClient(ctrl)
	instanceService := instances.NewCrusoeInstances(mockClient)

	mockClient.EXPECT().GetInstanceByID(gomock.Any(), TESTInstanceID).Return(&v1alpha5.InstanceV1Alpha5{
		Id: TESTInstanceID,
		NetworkInterfaces: []v1alpha5.NetworkInterface{
			{
				Ips: []v1alpha5.IpAddresses{
					{
						PrivateIpv4: &v1alpha5.PrivateIpv4Address{Address: "10.0.0.1"},
						PublicIpv4:  &v1alpha5.PublicIpv4Address{Address: "192.168.0.1"},
					},
				},
			},
		},
		Name: TESTNodeName,
	}, nil, nil)

	node := &v1.Node{
		Spec: v1.NodeSpec{
			ProviderID: ProviderIDPrefix + TESTInstanceID,
		},
	}

	metadata, err := instanceService.InstanceMetadata(context.Background(), node)
	require.NoError(t, err)
	require.NotNil(t, metadata)
	require.Equal(t, ProviderIDPrefix+TESTInstanceID, metadata.ProviderID)
	require.Equal(t, TESTNodeName, metadata.NodeAddresses[2].Address)
}

func TestNodeAddressesByProviderID(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	os.Setenv(CrusoeProjectID, TestProjectID)

	mockClient := mock_client.NewMockApiClient(ctrl)
	instanceService := instances.NewCrusoeInstances(mockClient)

	mockClient.EXPECT().GetInstanceByID(gomock.Any(), TESTInstanceID).Return(&v1alpha5.InstanceV1Alpha5{
		NetworkInterfaces: []v1alpha5.NetworkInterface{
			{
				Ips: []v1alpha5.IpAddresses{
					{
						PrivateIpv4: &v1alpha5.PrivateIpv4Address{Address: "10.0.0.1"},
						PublicIpv4:  &v1alpha5.PublicIpv4Address{Address: "192.168.0.1"},
					},
				},
			},
		},
		Name: TESTNodeName,
	}, nil, nil)

	addresses, err := instanceService.NodeAddressesByProviderID(context.Background(), ProviderIDPrefix+TESTInstanceID)
	require.NoError(t, err)
	require.Len(t, addresses, 3)
	require.Equal(t, "10.0.0.1", addresses[0].Address)
	require.Equal(t, "192.168.0.1", addresses[1].Address)
	require.Equal(t, TESTNodeName, addresses[2].Address)
}

func TestGetInstanceType(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockClient := mock_client.NewMockApiClient(ctrl)
	instanceService := instances.NewCrusoeInstances(mockClient)

	mockClient.EXPECT().GetInstanceByName(gomock.Any(), TESTNodeName).Return(&v1alpha5.InstanceV1Alpha5{
		Type_: TestInstanceType,
	}, nil)

	instanceType, err := instanceService.InstanceType(context.Background(), types.NodeName(TESTNodeName))
	require.NoError(t, err)
	require.Equal(t, TestInstanceType, instanceType)
}

func TestGetInstanceTypeByProviderID(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockClient := mock_client.NewMockApiClient(ctrl)
	instanceService := instances.NewCrusoeInstances(mockClient)

	mockClient.EXPECT().GetInstanceByID(gomock.Any(), TESTInstanceID).Return(&v1alpha5.InstanceV1Alpha5{
		Type_: TestInstanceType,
	}, nil, nil)

	instanceType, err := instanceService.InstanceTypeByProviderID(context.Background(), ProviderIDPrefix+TESTInstanceID)
	require.NoError(t, err)
	require.Equal(t, TestInstanceType, instanceType)
}

func TestCurrentNodeName(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockClient := mock_client.NewMockApiClient(ctrl)
	instanceService := instances.NewCrusoeInstances(mockClient)

	nodeName, err := instanceService.CurrentNodeName(context.Background(), TESTHostname)
	require.NoError(t, err)
	require.Equal(t, types.NodeName(TESTHostname), nodeName)
}

func TestInstanceShutdown(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockClient := mock_client.NewMockApiClient(ctrl)
	instanceService := instances.NewCrusoeInstances(mockClient)

	mockClient.EXPECT().GetInstanceByName(gomock.Any(), TESTNodeName).Return(&v1alpha5.InstanceV1Alpha5{
		Id: TESTInstanceID,
	}, nil)

	mockClient.EXPECT().GetInstanceByID(gomock.Any(), TESTInstanceID).Return(&v1alpha5.InstanceV1Alpha5{
		State: "STATE_SHUTOFF",
	}, nil, nil)

	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: TESTNodeName,
		},
	}

	shutdown, err := instanceService.InstanceShutdown(context.Background(), node)
	require.NoError(t, err)
	require.True(t, shutdown)
}

func TestInstanceExists(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockClient := mock_client.NewMockApiClient(ctrl)
	instanceService := instances.NewCrusoeInstances(mockClient)

	mockClient.EXPECT().GetInstanceByName(gomock.Any(), TESTNodeName).Return(&v1alpha5.InstanceV1Alpha5{
		Id: TESTInstanceID,
	}, nil)

	mockClient.EXPECT().GetInstanceByID(gomock.Any(), TESTInstanceID).Return(&v1alpha5.InstanceV1Alpha5{}, nil, nil)

	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: TESTNodeName,
		},
	}

	exists, err := instanceService.InstanceExists(context.Background(), node)
	require.NoError(t, err)
	require.True(t, exists)
}
