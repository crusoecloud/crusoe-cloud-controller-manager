package routes

import (
	"context"
	"fmt"

	"github.com/crusoecloud/crusoe-cloud-controller-manager/internal/client"
)

// ResolveLocationFromCluster looks up the cluster by id and returns its
// location. It is the sole location source (section 5.1), resolved once at startup
// before any node joins, using the --cluster-name flag the deployment already
// passes; that flag carries the Crusoe cluster UUID, not the display name.
// A non-nil error is fatal to controller startup: every SDN call carries the
// location, and failing here keeps Config immutable once workers start (the
// Deployment's crash-loop backoff is the retry).
func ResolveLocationFromCluster(ctx context.Context,
	apiClient client.APIClient, projectID, clusterID string,
) (string, error) {
	if projectID == "" || clusterID == "" {
		return "", fmt.Errorf("%w: project id and cluster id required for location lookup",
			client.ErrClusterNotFound)
	}

	cluster, err := apiClient.GetClusterByID(ctx, projectID, clusterID)
	if err != nil {
		return "", fmt.Errorf("failed to resolve cluster %s location: %w", clusterID, err)
	}

	return cluster.Location, nil
}

// ResolveClusterCIDRFromVPC returns the VPC network's CIDR, used to default
// the --cluster-cidr flag in native mode when the deployment does not set it.
// The upstream route controller requires a parseable cluster cidr once
// Routes() is non-nil and uses it only to scope deletions
// (isResponsibleForRoute); VPC prefix reservations are carved from the VPC
// range, so the VPC CIDR is always a correct covering supernet, including for
// reservations added after cluster creation.
func ResolveClusterCIDRFromVPC(ctx context.Context,
	apiClient client.APIClient, projectID, vpcID string,
) (string, error) {
	if projectID == "" || vpcID == "" {
		return "", fmt.Errorf("%w: project id and vpc id required for cluster cidr lookup",
			client.ErrVPCNotFound)
	}

	vpc, err := apiClient.GetVPCNetworkByID(ctx, projectID, vpcID)
	if err != nil {
		return "", fmt.Errorf("failed to resolve vpc %s cidr: %w", vpcID, err)
	}
	if vpc.Cidr == "" {
		return "", fmt.Errorf("%w: vpc %s has an empty cidr", client.ErrVPCNotFound, vpcID)
	}

	return vpc.Cidr, nil
}
