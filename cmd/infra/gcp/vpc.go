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

const (
	// GCP-specific constants
	gcpOperationTimeoutMinutes = 10
)

// VPCManager handles VPC network operations for GCP
type VPCManager struct {
	clients   *gcputil.GCPClients
	projectID string
	region    string
	logger    logr.Logger
}

// NewVPCManager creates a new VPC manager
func NewVPCManager(clients *gcputil.GCPClients, projectID, region string, logger logr.Logger) *VPCManager {
	return &VPCManager{
		clients:   clients,
		projectID: projectID,
		region:    region,
		logger:    logger,
	}
}

// CreateVPC creates a VPC network following AWS patterns
func (vm *VPCManager) CreateVPC(ctx context.Context, infraID, vpcCIDR string, labels map[string]string) (string, error) {
	vpcName := fmt.Sprintf("%s-vpc", infraID)

	// Check if VPC already exists (idempotent operation)
	existingVPC, err := vm.getExistingVPC(ctx, vpcName)
	if err != nil {
		return "", err
	}

	if existingVPC != nil {
		vm.logger.Info("Found existing VPC", "name", vpcName, "selfLink", existingVPC.SelfLink)
		return existingVPC.Name, nil
	}

	// Create new VPC network
	network := &compute.Network{
		Name:                  vpcName,
		AutoCreateSubnetworks: false, // Manual subnet creation like AWS
		Description:           fmt.Sprintf("HyperShift VPC for cluster %s", infraID),
		RoutingConfig: &compute.NetworkRoutingConfig{
			RoutingMode: "REGIONAL", // Regional routing for better performance
		},
	}

	// Note: GCP VPC networks don't support labels like other resources
	// Labels will be applied to other resources like subnets, instances, etc.

	vm.logger.Info("Creating VPC", "name", vpcName, "cidr", vpcCIDR)

	op, err := vm.clients.Compute.Networks.Insert(vm.projectID, network).Context(ctx).Do()
	if err != nil {
		return "", gcputil.HandleResourceCreationError(err, "VPC", vpcName)
	}

	// Wait for operation to complete
	err = vm.waitForGlobalOperation(ctx, op.Name, "VPC creation")
	if err != nil {
		return "", fmt.Errorf("failed waiting for VPC creation: %w", err)
	}

	vm.logger.Info("Successfully created VPC", "name", vpcName)
	return vpcName, nil
}

// DeleteVPC deletes a VPC network
func (vm *VPCManager) DeleteVPC(ctx context.Context, vpcName string) error {
	vm.logger.Info("Deleting VPC", "name", vpcName)

	op, err := vm.clients.Compute.Networks.Delete(vm.projectID, vpcName).Context(ctx).Do()
	if err != nil {
		return gcputil.HandleResourceDeletionError(err, "VPC", vpcName)
	}

	// Wait for deletion to complete
	err = vm.waitForGlobalOperation(ctx, op.Name, "VPC deletion")
	if err != nil {
		return fmt.Errorf("failed waiting for VPC deletion: %w", err)
	}

	vm.logger.Info("Successfully deleted VPC", "name", vpcName)
	return nil
}

// getExistingVPC checks if a VPC already exists
func (vm *VPCManager) getExistingVPC(ctx context.Context, vpcName string) (*compute.Network, error) {
	network, err := vm.clients.Compute.Networks.Get(vm.projectID, vpcName).Context(ctx).Do()
	if err != nil {
		if gcputil.IsGCPNotFoundError(err) {
			return nil, nil // VPC doesn't exist
		}
		return nil, fmt.Errorf("failed to check existing VPC %s: %w", vpcName, err)
	}
	return network, nil
}

// waitForGlobalOperation waits for a global operation to complete
func (vm *VPCManager) waitForGlobalOperation(ctx context.Context, operationName, description string) error {
	vm.logger.Info("Waiting for operation", "operation", operationName, "description", description)

	return wait.PollUntilContextTimeout(ctx, 5*time.Second, gcpOperationTimeoutMinutes*time.Minute, true, func(ctx context.Context) (bool, error) {
		op, err := vm.clients.Compute.GlobalOperations.Get(vm.projectID, operationName).Context(ctx).Do()
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
			vm.logger.V(1).Info("Operation in progress", "status", op.Status, "progress", op.Progress)
			return false, nil
		default:
			return false, fmt.Errorf("unexpected operation status: %s", op.Status)
		}
	})
}

// GetVPCDetails retrieves VPC information for output
func (vm *VPCManager) GetVPCDetails(ctx context.Context, vpcName string) (*VPCDetails, error) {
	network, err := vm.clients.Compute.Networks.Get(vm.projectID, vpcName).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("failed to get VPC details: %w", err)
	}

	return &VPCDetails{
		Name:              network.Name,
		SelfLink:          network.SelfLink,
		Description:       network.Description,
		Labels:            nil, // GCP networks don't support labels
		CreationTimestamp: network.CreationTimestamp,
	}, nil
}

// VPCDetails represents VPC information for output
type VPCDetails struct {
	Name              string            `json:"name"`
	SelfLink          string            `json:"selfLink"`
	Description       string            `json:"description"`
	Labels            map[string]string `json:"labels"`
	CreationTimestamp string            `json:"creationTimestamp"`
}
