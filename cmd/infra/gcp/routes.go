package gcp

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/util/wait"

	gcputil "github.com/openshift/hypershift/cmd/infra/gcp/util"
)

// RouteManager handles network route operations for GCP
type RouteManager struct {
	clients   *gcputil.GCPClients
	projectID string
	region    string
	logger    logr.Logger
}

// RouteDetails represents route information for output
type RouteDetails struct {
	Name              string            `json:"name"`
	VPCName           string            `json:"vpcName"`
	DestinationRange  string            `json:"destinationRange"`
	NextHopGateway    string            `json:"nextHopGateway,omitempty"`
	NextHopInstance   string            `json:"nextHopInstance,omitempty"`
	NextHopVpnTunnel  string            `json:"nextHopVpnTunnel,omitempty"`
	Priority          int64             `json:"priority"`
	Tags              []string          `json:"tags,omitempty"`
	Description       string            `json:"description"`
	Labels            map[string]string `json:"labels"`
	CreationTimestamp string            `json:"creationTimestamp"`
}

// NewRouteManager creates a new route manager
func NewRouteManager(clients *gcputil.GCPClients, projectID, region string, logger logr.Logger) *RouteManager {
	return &RouteManager{
		clients:   clients,
		projectID: projectID,
		region:    region,
		logger:    logger,
	}
}

// CreateRoutes creates any custom routes needed for the cluster
func (rm *RouteManager) CreateRoutes(ctx context.Context, infraID, vpcName string, labels map[string]string) ([]*RouteDetails, error) {
	rm.logger.Info("Creating custom routes", "infraID", infraID, "vpc", vpcName)

	var routes []*RouteDetails

	// In GCP, most routing is handled automatically:
	// - Default internet gateway route for public subnets (automatically created)
	// - NAT gateway routes for private subnets (automatically managed by Cloud NAT)
	// - Internal VPC routes (automatically created)

	// For HyperShift, we might need some custom routes for specific use cases
	// Let's create a custom route for internal cluster communication if needed

	// Create a high-priority route for internal cluster traffic
	// This ensures cluster traffic stays within the VPC even if there are conflicting routes
	internalRoute, err := rm.createInternalClusterRoute(ctx, infraID, vpcName, labels)
	if err != nil {
		return nil, fmt.Errorf("failed to create internal cluster route: %w", err)
	}
	if internalRoute != nil {
		routes = append(routes, internalRoute)
	}

	// Note: Additional custom routes can be added here as needed for specific cluster requirements
	// Examples:
	// - Routes for service mesh (if using Istio)
	// - Routes for multi-cluster networking
	// - Routes for hybrid connectivity (VPN, Interconnect)

	rm.logger.Info("Successfully created custom routes", "count", len(routes))
	return routes, nil
}

// createInternalClusterRoute creates a route for internal cluster communication
// This is optional and mainly for demonstration - GCP handles internal routing automatically
func (rm *RouteManager) createInternalClusterRoute(ctx context.Context, infraID, vpcName string, labels map[string]string) (*RouteDetails, error) {
	// In most cases, we don't need explicit internal routes since GCP handles them automatically
	// This is mainly a placeholder for cases where custom internal routing might be needed

	rm.logger.Info("Internal cluster routes are handled automatically by GCP - no custom routes needed")
	return nil, nil

}

// DeleteRoutes deletes all custom routes for a cluster
func (rm *RouteManager) DeleteRoutes(ctx context.Context, infraID string) error {
	rm.logger.Info("Deleting custom routes", "infraID", infraID)

	// Since we're not creating custom routes in the current implementation,
	// there's nothing to delete. This is mainly a placeholder for future custom routes.

	customRouteNames := []string{
		fmt.Sprintf("%s-internal-route", infraID),
		// Add other custom route names here as needed
	}

	deletedCount := 0
	for _, routeName := range customRouteNames {
		if err := rm.deleteRoute(ctx, routeName); err != nil {
			// Don't fail the entire operation if route deletion fails - it might not exist
			rm.logger.Error(err, "Failed to delete route, continuing", "route", routeName)
		} else {
			rm.logger.Info("Successfully deleted route", "name", routeName)
			deletedCount++
		}
	}

	if deletedCount == 0 {
		rm.logger.Info("No custom routes to delete - GCP manages default routes automatically")
	} else {
		rm.logger.Info("Successfully deleted custom routes", "count", deletedCount)
	}

	return nil
}

// deleteRoute deletes a single route
func (rm *RouteManager) deleteRoute(ctx context.Context, routeName string) error {
	rm.logger.Info("Deleting route", "name", routeName)

	op, err := rm.clients.Compute.Routes.Delete(rm.projectID, routeName).Context(ctx).Do()
	if err != nil {
		return gcputil.HandleResourceDeletionError(err, "route", routeName)
	}

	// Wait for deletion to complete
	err = rm.waitForGlobalOperation(ctx, op.Name, fmt.Sprintf("route %s deletion", routeName))
	if err != nil {
		return fmt.Errorf("failed waiting for route deletion: %w", err)
	}

	rm.logger.Info("Successfully deleted route", "name", routeName)
	return nil
}

// waitForGlobalOperation waits for a global operation to complete
func (rm *RouteManager) waitForGlobalOperation(ctx context.Context, operationName, description string) error {
	rm.logger.Info("Waiting for global operation", "operation", operationName, "description", description)

	return wait.PollUntilContextTimeout(ctx, 5*time.Second, gcpOperationTimeoutMinutes*time.Minute, true, func(ctx context.Context) (bool, error) {
		op, err := rm.clients.Compute.GlobalOperations.Get(rm.projectID, operationName).Context(ctx).Do()
		if err != nil {
			return false, fmt.Errorf("failed to get operation status: %w", err)
		}

		switch op.Status {
		case "DONE":
			if op.Error != nil {
				return false, fmt.Errorf("operation failed: %v", op.Error)
			}
			return true, nil
		case "RUNNING", "PENDING":
			rm.logger.V(1).Info("Operation in progress", "status", op.Status, "progress", op.Progress)
			return false, nil
		default:
			return false, fmt.Errorf("unexpected operation status: %s", op.Status)
		}
	})
}
