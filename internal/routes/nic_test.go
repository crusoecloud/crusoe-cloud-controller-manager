package routes

import (
	"context"
	"errors"
	"net/http"
	"testing"

	crusoeapi "github.com/crusoecloud/client-go/swagger/v1alpha5"
	mock_client "github.com/crusoecloud/crusoe-cloud-controller-manager/internal/client/mock"
	"github.com/golang/mock/gomock"
	v1 "k8s.io/api/core/v1"
)

const (
	nicNodeName    = "worker-0"
	nicVPCID       = "net-abc"
	nicInstanceID  = "11111111-2222-3333-4444-555555555555"
	nicMatchingID  = "nic-vpc"
	nicOtherID     = "nic-other"
	nicProjectID   = "proj-123"
	nicLocation    = "us-east1-a"
	nicClusterName = "sriprod1"
)

func nicConfig() *Config {
	return &Config{
		ProjectID: nicProjectID,
		VPCID:     nicVPCID,
	}
}

func instanceWith(nics []crusoeapi.NetworkInterface) *crusoeapi.InstanceV1Alpha5 {
	return &crusoeapi.InstanceV1Alpha5{
		Id:                nicInstanceID,
		Location:          nicLocation,
		ProjectId:         nicProjectID,
		NetworkInterfaces: nics,
	}
}

func TestResolveNIC_ProviderIDPath(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m := mock_client.NewMockApiClient(ctrl)
	inst := instanceWith([]crusoeapi.NetworkInterface{
		{Id: nicOtherID, Network: "net-other"},
		{Id: nicMatchingID, Network: nicVPCID},
	})
	m.EXPECT().GetInstanceByID(gomock.Any(), nicInstanceID).Return(inst, &http.Response{}, nil)

	c := newTestController(nicConfig(), m)
	node := &v1.Node{}
	node.Spec.ProviderID = "crusoe://" + nicInstanceID

	got, err := c.resolveNIC(context.Background(), nicNodeName, node)
	if err != nil {
		t.Fatalf("resolveNIC: %v", err)
	}
	if got != nicMatchingID {
		t.Fatalf("expected %s, got %s", nicMatchingID, got)
	}
}

func TestResolveNIC_SystemUUIDPath(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m := mock_client.NewMockApiClient(ctrl)
	inst := instanceWith([]crusoeapi.NetworkInterface{{Id: nicMatchingID, Network: nicVPCID}})
	// providerID differs from SystemUUID -> SystemUUID wins.
	m.EXPECT().GetInstanceByID(gomock.Any(), nicInstanceID).Return(inst, &http.Response{}, nil)

	c := newTestController(nicConfig(), m)
	node := &v1.Node{}
	node.Spec.ProviderID = "crusoe://stale-id"
	node.Status.NodeInfo.SystemUUID = nicInstanceID

	got, err := c.resolveNIC(context.Background(), nicNodeName, node)
	if err != nil {
		t.Fatalf("resolveNIC: %v", err)
	}
	if got != nicMatchingID {
		t.Fatalf("expected %s, got %s", nicMatchingID, got)
	}
}

func TestResolveNIC_NameFallback(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m := mock_client.NewMockApiClient(ctrl)
	inst := instanceWith([]crusoeapi.NetworkInterface{{Id: nicMatchingID, Network: nicVPCID}})
	// Node is nil -> straight to name lookup.
	m.EXPECT().GetInstanceByName(gomock.Any(), nicNodeName).Return(inst, nil)

	c := newTestController(nicConfig(), m)
	got, err := c.resolveNIC(context.Background(), nicNodeName, nil)
	if err != nil {
		t.Fatalf("resolveNIC: %v", err)
	}
	if got != nicMatchingID {
		t.Fatalf("expected %s, got %s", nicMatchingID, got)
	}
}

func TestResolveNIC_GetByIDFailsFallsBackToName(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m := mock_client.NewMockApiClient(ctrl)
	inst := instanceWith([]crusoeapi.NetworkInterface{{Id: nicMatchingID, Network: nicVPCID}})
	m.EXPECT().GetInstanceByID(gomock.Any(), nicInstanceID).Return(nil, nil, errors.New("boom"))
	m.EXPECT().GetInstanceByName(gomock.Any(), nicNodeName).Return(inst, nil)

	c := newTestController(nicConfig(), m)
	node := &v1.Node{}
	node.Spec.ProviderID = "crusoe://" + nicInstanceID

	got, err := c.resolveNIC(context.Background(), nicNodeName, node)
	if err != nil {
		t.Fatalf("resolveNIC: %v", err)
	}
	if got != nicMatchingID {
		t.Fatalf("expected %s, got %s", nicMatchingID, got)
	}
}

func TestResolveNIC_NICZeroFallback(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m := mock_client.NewMockApiClient(ctrl)
	// No NIC matches the VPC -> NICs[0].
	inst := instanceWith([]crusoeapi.NetworkInterface{
		{Id: "nic-first", Network: "net-x"},
		{Id: "nic-second", Network: "net-y"},
	})
	m.EXPECT().GetInstanceByName(gomock.Any(), nicNodeName).Return(inst, nil)

	c := newTestController(nicConfig(), m)
	got, err := c.resolveNIC(context.Background(), nicNodeName, nil)
	if err != nil {
		t.Fatalf("resolveNIC: %v", err)
	}
	if got != "nic-first" {
		t.Fatalf("expected nic-first fallback, got %s", got)
	}
}

func TestResolveNIC_ZeroNICsErrors(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m := mock_client.NewMockApiClient(ctrl)
	m.EXPECT().GetInstanceByName(gomock.Any(), nicNodeName).Return(instanceWith(nil), nil)

	c := newTestController(nicConfig(), m)
	if _, err := c.resolveNIC(context.Background(), nicNodeName, nil); err == nil {
		t.Fatalf("expected error for zero NICs")
	}
}

func TestResolveNIC_DoesNotMutateConfig(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m := mock_client.NewMockApiClient(ctrl)
	inst := instanceWith([]crusoeapi.NetworkInterface{{Id: nicMatchingID, Network: nicVPCID}})
	m.EXPECT().GetInstanceByName(gomock.Any(), nicNodeName).Return(inst, nil)

	// cfg is immutable after startup (§5.1): a mismatching instance only warns,
	// it never overwrites the startup-resolved values.
	cfg := nicConfig()
	cfg.Location = "other-location"
	cfg.ProjectID = "other-project"
	c := newTestController(cfg, m)
	if _, err := c.resolveNIC(context.Background(), nicNodeName, nil); err != nil {
		t.Fatalf("resolveNIC: %v", err)
	}
	if c.cfg.Location != "other-location" || c.cfg.ProjectID != "other-project" {
		t.Fatalf("resolveNIC must not mutate config, got location %q project %q",
			c.cfg.Location, c.cfg.ProjectID)
	}
}

func TestResolveNIC_CachesNICID(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m := mock_client.NewMockApiClient(ctrl)
	inst := instanceWith([]crusoeapi.NetworkInterface{{Id: nicMatchingID, Network: nicVPCID}})
	// Exactly one call expected despite two ResolveNIC invocations.
	m.EXPECT().GetInstanceByName(gomock.Any(), nicNodeName).Return(inst, nil).Times(1)

	c := newTestController(nicConfig(), m)
	for range 2 {
		if _, err := c.resolveNIC(context.Background(), nicNodeName, nil); err != nil {
			t.Fatalf("resolveNIC: %v", err)
		}
	}
}

func TestResolveLocationFromCluster(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m := mock_client.NewMockApiClient(ctrl)
	m.EXPECT().GetClusterByName(gomock.Any(), nicProjectID, nicClusterName).
		Return(&crusoeapi.KubernetesCluster{Name: nicClusterName, Location: nicLocation}, nil)

	loc, err := resolveLocationFromCluster(context.Background(), m, nicProjectID, nicClusterName)
	if err != nil {
		t.Fatalf("resolveLocationFromCluster: %v", err)
	}
	if loc != nicLocation {
		t.Fatalf("expected %s, got %s", nicLocation, loc)
	}
}

func TestResolveLocationFromCluster_LookupFailurePropagates(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m := mock_client.NewMockApiClient(ctrl)
	m.EXPECT().GetClusterByName(gomock.Any(), nicProjectID, nicClusterName).
		Return(nil, errors.New("api down"))

	// The error propagates; startRouteController treats it as fatal (§5.1).
	if _, err := resolveLocationFromCluster(context.Background(), m, nicProjectID, nicClusterName); err == nil {
		t.Fatalf("expected error to propagate")
	}
}
