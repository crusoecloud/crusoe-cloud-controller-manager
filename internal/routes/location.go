package routes

import (
	"context"
	"fmt"

	"github.com/crusoecloud/crusoe-cloud-controller-manager/internal/client"
)

// resolveLocationFromCluster looks up the cluster by name and returns its
// location. It is the primary location source (§5.1), resolved once at startup
// before any node joins, using the --cluster-name flag the deployment already
// passes. Callers treat a non-nil error as non-fatal: reconciles fall back to
// instance-derived location and the reaper retries the lookup.
func resolveLocationFromCluster(ctx context.Context,
	apiClient client.APIClient, projectID, clusterName string,
) (string, error) {
	if projectID == "" || clusterName == "" {
		return "", fmt.Errorf("%w: project id and cluster name required for location lookup",
			client.ErrClusterNotFound)
	}

	cluster, err := apiClient.GetClusterByName(ctx, projectID, clusterName)
	if err != nil {
		return "", fmt.Errorf("failed to resolve cluster %s location: %w", clusterName, err)
	}

	return cluster.Location, nil
}
