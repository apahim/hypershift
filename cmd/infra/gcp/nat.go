package gcp

import (
	"context"
	"fmt"
	"time"

	gcputil "github.com/openshift/hypershift/cmd/infra/gcp/util"

	"google.golang.org/api/compute/v1"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/util/wait"
)

// NATManager handles Cloud NAT operations for GCP
type NATManager struct {
	clients   *gcputil.GCPClients
	projectID string
	region    string
	logger    logr.Logger
}

// NATGatewayDetails represents NAT gateway information for output
type NATGatewayDetails struct {
	Name                string            `json:"name"`
	RouterName          string            `json:"routerName"`
	VPCName             string            `json:"vpcName"`
	SourceSubnets       []string          `json:"sourceSubnets"`
	NATIPAllocateOption string            `json:"natIPAllocateOption"`
	Labels              map[string]string `json:"labels"`
	CreationTimestamp   string            `json:"creationTimestamp"`
}

// NewNATManager creates a new NAT manager
func NewNATManager(clients *gcputil.GCPClients, projectID, region string, logger logr.Logger) *NATManager {
	return &NATManager{
		clients:   clients,
		projectID: projectID,
		region:    region,
		logger:    logger,
	}
}

// CreateNATGateway creates a Cloud NAT gateway for private subnet egress
func (nm *NATManager) CreateNATGateway(ctx context.Context, infraID, vpcName string, privateSubnets []string, singleNATGateway bool, labels map[string]string) (*NATGatewayDetails, error) {
	nm.logger.Info("Creating NAT gateway", "infraID", infraID, "vpc", vpcName, "singleNATGateway", singleNATGateway)

	// If no private subnets specified, skip NAT creation
	if len(privateSubnets) == 0 {
		nm.logger.Info("No private subnets specified, skipping NAT gateway creation")
		return nil, nil
	}

	routerName := fmt.Sprintf("%s-router", infraID)
	natName := fmt.Sprintf("%s-nat", infraID)

	// Create Cloud Router first (required for NAT)
	_, err := nm.createCloudRouter(ctx, routerName, vpcName, infraID, labels)
	if err != nil {
		return nil, fmt.Errorf("failed to create Cloud Router: %w", err)
	}

	// Create Cloud NAT
	natGateway, err := nm.createCloudNAT(ctx, natName, routerName, privateSubnets, singleNATGateway, labels)
	if err != nil {
		return nil, fmt.Errorf("failed to create Cloud NAT: %w", err)
	}

	nm.logger.Info("Successfully created NAT gateway", "name", natName, "router", routerName)

	return &NATGatewayDetails{
		Name:                natName,
		RouterName:          routerName,
		VPCName:             vpcName,
		SourceSubnets:       privateSubnets,
		NATIPAllocateOption: natGateway.NatIpAllocateOption,
		Labels:              labels,
		CreationTimestamp:   "", // Will be populated when we fetch the created NAT
	}, nil
}

// createCloudRouter creates a Cloud Router for the NAT gateway
func (nm *NATManager) createCloudRouter(ctx context.Context, routerName, vpcName, infraID string, labels map[string]string) (*compute.Router, error) {
	// Check if router already exists
	existingRouter, err := nm.getExistingRouter(ctx, routerName)
	if err != nil {
		return nil, err
	}

	if existingRouter != nil {
		nm.logger.Info("Found existing Cloud Router", "name", routerName)
		return existingRouter, nil
	}

	// Build VPC network reference
	vpcSelfLink := fmt.Sprintf("projects/%s/global/networks/%s", nm.projectID, vpcName)

	router := &compute.Router{
		Name:        routerName,
		Network:     vpcSelfLink,
		Description: fmt.Sprintf("HyperShift Cloud Router for cluster %s", infraID),
		Region:      fmt.Sprintf("projects/%s/regions/%s", nm.projectID, nm.region),
	}

	nm.logger.Info("Creating Cloud Router", "name", routerName, "vpc", vpcName)

	op, err := nm.clients.Compute.Routers.Insert(nm.projectID, nm.region, router).Context(ctx).Do()
	if err != nil {
		return nil, gcputil.HandleResourceCreationError(err, "Cloud Router", routerName)
	}

	// Wait for operation to complete
	err = nm.waitForRegionalOperation(ctx, op.Name, fmt.Sprintf("Cloud Router %s creation", routerName))
	if err != nil {
		return nil, fmt.Errorf("failed waiting for Cloud Router creation: %w", err)
	}

	nm.logger.Info("Successfully created Cloud Router", "name", routerName)

	// Get the created router
	createdRouter, err := nm.clients.Compute.Routers.Get(nm.projectID, nm.region, routerName).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("failed to get created Cloud Router: %w", err)
	}

	return createdRouter, nil
}

// createCloudNAT creates a Cloud NAT configuration on the router
func (nm *NATManager) createCloudNAT(ctx context.Context, natName, routerName string, privateSubnets []string, singleNATGateway bool, labels map[string]string) (*compute.RouterNat, error) {
	// Check if NAT already exists
	existingNAT, err := nm.getExistingNAT(ctx, routerName, natName)
	if err != nil {
		return nil, err
	}

	if existingNAT != nil {
		nm.logger.Info("Found existing Cloud NAT", "name", natName, "router", routerName)
		return existingNAT, nil
	}

	// Build subnet references for source subnet ranges
	var sourceSubnetIpRangesToNat []*compute.RouterNatSubnetworkToNat
	for _, subnetName := range privateSubnets {
		subnetSelfLink := fmt.Sprintf("projects/%s/regions/%s/subnetworks/%s", nm.projectID, nm.region, subnetName)
		sourceSubnetIpRangesToNat = append(sourceSubnetIpRangesToNat, &compute.RouterNatSubnetworkToNat{
			Name:                subnetSelfLink,
			SourceIpRangesToNat: []string{"ALL_IP_RANGES"}, // NAT all traffic from the subnet
		})
	}

	// Configure NAT IP allocation
	natIPAllocateOption := "AUTO_ONLY" // Let GCP auto-allocate NAT IPs
	if singleNATGateway {
		// For single NAT gateway, we might want to use manual allocation for better control
		natIPAllocateOption = "AUTO_ONLY" // Keep auto for simplicity
	}

	nat := &compute.RouterNat{
		Name:                          natName,
		NatIpAllocateOption:           natIPAllocateOption,
		SourceSubnetworkIpRangesToNat: "LIST_OF_SUBNETWORKS",
		Subnetworks:                   sourceSubnetIpRangesToNat,
		MinPortsPerVm:                 64,   // Minimum ports per VM (default: 64)
		UdpIdleTimeoutSec:             30,   // UDP idle timeout (default: 30s)
		IcmpIdleTimeoutSec:            30,   // ICMP idle timeout (default: 30s)
		TcpEstablishedIdleTimeoutSec:  1200, // TCP established connection timeout (default: 1200s)
		TcpTransitoryIdleTimeoutSec:   30,   // TCP transitory connection timeout (default: 30s)
		LogConfig: &compute.RouterNatLogConfig{
			Enable: false, // Disable logging to reduce costs
			Filter: "ERRORS_ONLY",
		},
	}

	nm.logger.Info("Creating Cloud NAT", "name", natName, "router", routerName, "subnets", len(privateSubnets))

	// Get the router to add NAT to it
	router, err := nm.clients.Compute.Routers.Get(nm.projectID, nm.region, routerName).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("failed to get router for NAT creation: %w", err)
	}

	// Add NAT to router's NAT list
	if router.Nats == nil {
		router.Nats = []*compute.RouterNat{}
	}
	router.Nats = append(router.Nats, nat)

	// Update the router with the new NAT
	op, err := nm.clients.Compute.Routers.Update(nm.projectID, nm.region, routerName, router).Context(ctx).Do()
	if err != nil {
		return nil, gcputil.HandleResourceCreationError(err, "Cloud NAT", natName)
	}

	// Wait for operation to complete
	err = nm.waitForRegionalOperation(ctx, op.Name, fmt.Sprintf("Cloud NAT %s creation", natName))
	if err != nil {
		return nil, fmt.Errorf("failed waiting for Cloud NAT creation: %w", err)
	}

	nm.logger.Info("Successfully created Cloud NAT", "name", natName)

	return nat, nil
}

// DeleteNATGateway deletes the Cloud NAT gateway and router
func (nm *NATManager) DeleteNATGateway(ctx context.Context, infraID string) error {
	nm.logger.Info("Deleting NAT gateway", "infraID", infraID)

	routerName := fmt.Sprintf("%s-router", infraID)
	natName := fmt.Sprintf("%s-nat", infraID)

	// Delete Cloud NAT first
	err := nm.deleteCloudNAT(ctx, routerName, natName)
	if err != nil {
		// Don't fail the entire operation if NAT deletion fails - it might not exist
		nm.logger.Error(err, "Failed to delete Cloud NAT, continuing with router deletion", "nat", natName)
	} else {
		nm.logger.Info("Successfully deleted Cloud NAT", "name", natName)
	}

	// Delete Cloud Router
	err = nm.deleteCloudRouter(ctx, routerName)
	if err != nil {
		// Don't fail the entire operation if router deletion fails - it might not exist
		nm.logger.Error(err, "Failed to delete Cloud Router", "router", routerName)
		return err
	}

	nm.logger.Info("Successfully deleted NAT gateway", "router", routerName)
	return nil
}

// deleteCloudNAT deletes a Cloud NAT from a router
func (nm *NATManager) deleteCloudNAT(ctx context.Context, routerName, natName string) error {
	nm.logger.Info("Deleting Cloud NAT", "name", natName, "router", routerName)

	// Get the router
	router, err := nm.clients.Compute.Routers.Get(nm.projectID, nm.region, routerName).Context(ctx).Do()
	if err != nil {
		if gcputil.IsGCPNotFoundError(err) {
			return nil // Router doesn't exist, NAT is already gone
		}
		return fmt.Errorf("failed to get router for NAT deletion: %w", err)
	}

	// Remove NAT from router's NAT list
	var updatedNATs []*compute.RouterNat
	for _, nat := range router.Nats {
		if nat.Name != natName {
			updatedNATs = append(updatedNATs, nat)
		}
	}
	router.Nats = updatedNATs

	// Update the router without the NAT
	op, err := nm.clients.Compute.Routers.Update(nm.projectID, nm.region, routerName, router).Context(ctx).Do()
	if err != nil {
		return gcputil.HandleResourceDeletionError(err, "Cloud NAT", natName)
	}

	// Wait for operation to complete
	err = nm.waitForRegionalOperation(ctx, op.Name, fmt.Sprintf("Cloud NAT %s deletion", natName))
	if err != nil {
		return fmt.Errorf("failed waiting for Cloud NAT deletion: %w", err)
	}

	nm.logger.Info("Successfully deleted Cloud NAT", "name", natName)
	return nil
}

// deleteCloudRouter deletes a Cloud Router
func (nm *NATManager) deleteCloudRouter(ctx context.Context, routerName string) error {
	nm.logger.Info("Deleting Cloud Router", "name", routerName)

	op, err := nm.clients.Compute.Routers.Delete(nm.projectID, nm.region, routerName).Context(ctx).Do()
	if err != nil {
		return gcputil.HandleResourceDeletionError(err, "Cloud Router", routerName)
	}

	// Wait for deletion to complete
	err = nm.waitForRegionalOperation(ctx, op.Name, fmt.Sprintf("Cloud Router %s deletion", routerName))
	if err != nil {
		return fmt.Errorf("failed waiting for Cloud Router deletion: %w", err)
	}

	nm.logger.Info("Successfully deleted Cloud Router", "name", routerName)
	return nil
}

// getExistingRouter checks if a Cloud Router already exists
func (nm *NATManager) getExistingRouter(ctx context.Context, routerName string) (*compute.Router, error) {
	router, err := nm.clients.Compute.Routers.Get(nm.projectID, nm.region, routerName).Context(ctx).Do()
	if err != nil {
		if gcputil.IsGCPNotFoundError(err) {
			return nil, nil // Router doesn't exist
		}
		return nil, fmt.Errorf("failed to check existing Cloud Router %s: %w", routerName, err)
	}
	return router, nil
}

// getExistingNAT checks if a Cloud NAT already exists on a router
func (nm *NATManager) getExistingNAT(ctx context.Context, routerName, natName string) (*compute.RouterNat, error) {
	router, err := nm.getExistingRouter(ctx, routerName)
	if err != nil {
		return nil, err
	}

	if router == nil {
		return nil, nil // Router doesn't exist, so NAT doesn't exist
	}

	// Look for NAT in router's NAT list
	for _, nat := range router.Nats {
		if nat.Name == natName {
			return nat, nil
		}
	}

	return nil, nil // NAT doesn't exist
}

// waitForRegionalOperation waits for a regional operation to complete
func (nm *NATManager) waitForRegionalOperation(ctx context.Context, operationName, description string) error {
	nm.logger.Info("Waiting for regional operation", "operation", operationName, "description", description)

	return wait.PollUntilContextTimeout(ctx, 5*time.Second, gcpOperationTimeoutMinutes*time.Minute, true, func(ctx context.Context) (bool, error) {
		op, err := nm.clients.Compute.RegionOperations.Get(nm.projectID, nm.region, operationName).Context(ctx).Do()
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
			nm.logger.V(1).Info("Operation in progress", "status", op.Status, "progress", op.Progress)
			return false, nil
		default:
			return false, fmt.Errorf("unexpected operation status: %s", op.Status)
		}
	})
}
