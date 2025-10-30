package gcp

import (
	"context"
	"fmt"
	"net"
	"time"

	gcputil "github.com/openshift/hypershift/cmd/infra/gcp/util"

	"google.golang.org/api/compute/v1"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/util/wait"
)

// SubnetManager handles subnet operations for GCP
type SubnetManager struct {
	clients   *gcputil.GCPClients
	projectID string
	region    string
	logger    logr.Logger
}

// SubnetDetails represents subnet information for output
type SubnetDetails struct {
	Name              string            `json:"name"`
	SelfLink          string            `json:"selfLink"`
	CIDR              string            `json:"cidr"`
	Type              string            `json:"type"` // "public" or "private"
	Zone              string            `json:"zone"`
	VPCName           string            `json:"vpcName"`
	Labels            map[string]string `json:"labels"`
	CreationTimestamp string            `json:"creationTimestamp"`
}

// NewSubnetManager creates a new subnet manager
func NewSubnetManager(clients *gcputil.GCPClients, projectID, region string, logger logr.Logger) *SubnetManager {
	return &SubnetManager{
		clients:   clients,
		projectID: projectID,
		region:    region,
		logger:    logger,
	}
}

// CreateSubnets creates public and private subnets across availability zones
func (sm *SubnetManager) CreateSubnets(ctx context.Context, infraID, vpcName, vpcCIDR string, zones []string, publicOnly bool, labels map[string]string) ([]*SubnetDetails, error) {
	sm.logger.Info("Creating subnets", "infraID", infraID, "vpc", vpcName, "zones", zones, "publicOnly", publicOnly)

	if len(zones) == 0 {
		// Default to region's zones if none specified
		defaultZones, err := sm.getDefaultZones(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to get default zones: %w", err)
		}
		zones = defaultZones
	}

	var subnets []*SubnetDetails

	// Parse VPC CIDR to calculate subnet CIDRs
	_, vpcNet, err := net.ParseCIDR(vpcCIDR)
	if err != nil {
		return nil, fmt.Errorf("invalid VPC CIDR %s: %w", vpcCIDR, err)
	}

	// Create public subnets (one per zone)
	for i, zone := range zones {
		publicSubnetCIDR, err := sm.calculateSubnetCIDR(vpcNet, i*2, 20) // Public subnets use even indices
		if err != nil {
			return nil, fmt.Errorf("failed to calculate public subnet CIDR for zone %s: %w", zone, err)
		}

		subnet, err := sm.createSubnet(ctx, infraID, vpcName, zone, "public", publicSubnetCIDR, labels)
		if err != nil {
			return nil, fmt.Errorf("failed to create public subnet in zone %s: %w", zone, err)
		}
		subnets = append(subnets, subnet)
	}

	// Create private subnets (one per zone) if not public-only
	if !publicOnly {
		for i, zone := range zones {
			privateSubnetCIDR, err := sm.calculateSubnetCIDR(vpcNet, i*2+1, 20) // Private subnets use odd indices
			if err != nil {
				return nil, fmt.Errorf("failed to calculate private subnet CIDR for zone %s: %w", zone, err)
			}

			subnet, err := sm.createSubnet(ctx, infraID, vpcName, zone, "private", privateSubnetCIDR, labels)
			if err != nil {
				return nil, fmt.Errorf("failed to create private subnet in zone %s: %w", zone, err)
			}
			subnets = append(subnets, subnet)
		}
	}

	sm.logger.Info("Successfully created all subnets", "count", len(subnets))
	return subnets, nil
}

// createSubnet creates a single subnet
func (sm *SubnetManager) createSubnet(ctx context.Context, infraID, vpcName, zone, subnetType, cidr string, labels map[string]string) (*SubnetDetails, error) {
	subnetName := fmt.Sprintf("%s-%s-%s", infraID, subnetType, zone)

	// Check if subnet already exists (idempotent operation)
	existingSubnet, err := sm.getExistingSubnet(ctx, subnetName)
	if err != nil {
		return nil, err
	}

	if existingSubnet != nil {
		sm.logger.Info("Found existing subnet", "name", subnetName, "selfLink", existingSubnet.SelfLink)
		return &SubnetDetails{
			Name:              existingSubnet.Name,
			SelfLink:          existingSubnet.SelfLink,
			CIDR:              existingSubnet.IpCidrRange,
			Type:              subnetType,
			Zone:              zone,
			VPCName:           vpcName,
			Labels:            nil, // GCP doesn't have labels on subnets directly
			CreationTimestamp: existingSubnet.CreationTimestamp,
		}, nil
	}

	// Create new subnet
	vpcSelfLink := fmt.Sprintf("projects/%s/global/networks/%s", sm.projectID, vpcName)

	subnet := &compute.Subnetwork{
		Name:        subnetName,
		IpCidrRange: cidr,
		Network:     vpcSelfLink,
		Region:      fmt.Sprintf("projects/%s/regions/%s", sm.projectID, sm.region),
		Description: fmt.Sprintf("HyperShift %s subnet for cluster %s in zone %s", subnetType, infraID, zone),
	}

	// Set subnet-specific configuration
	if subnetType == "private" {
		subnet.PrivateIpGoogleAccess = true // Enable private Google access for private subnets
		subnet.Purpose = "PRIVATE"
	} else {
		subnet.Purpose = "PRIVATE" // GCP subnets are always private by default, public access is via external IPs
	}

	sm.logger.Info("Creating subnet", "name", subnetName, "cidr", cidr, "type", subnetType, "zone", zone)

	op, err := sm.clients.Compute.Subnetworks.Insert(sm.projectID, sm.region, subnet).Context(ctx).Do()
	if err != nil {
		return nil, gcputil.HandleResourceCreationError(err, "subnet", subnetName)
	}

	// Wait for operation to complete
	err = sm.waitForRegionalOperation(ctx, op.Name, fmt.Sprintf("subnet %s creation", subnetName))
	if err != nil {
		return nil, fmt.Errorf("failed waiting for subnet creation: %w", err)
	}

	sm.logger.Info("Successfully created subnet", "name", subnetName, "cidr", cidr, "type", subnetType)

	return &SubnetDetails{
		Name:              subnetName,
		SelfLink:          fmt.Sprintf("projects/%s/regions/%s/subnetworks/%s", sm.projectID, sm.region, subnetName),
		CIDR:              cidr,
		Type:              subnetType,
		Zone:              zone,
		VPCName:           vpcName,
		Labels:            labels,
		CreationTimestamp: "", // Will be populated when we fetch the created subnet
	}, nil
}

// DeleteSubnets deletes all subnets for a cluster
func (sm *SubnetManager) DeleteSubnets(ctx context.Context, infraID string, zones []string, publicOnly bool) error {
	sm.logger.Info("Deleting subnets", "infraID", infraID, "zones", zones, "publicOnly", publicOnly)

	if len(zones) == 0 {
		// Default to region's zones if none specified
		defaultZones, err := sm.getDefaultZones(ctx)
		if err != nil {
			return fmt.Errorf("failed to get default zones: %w", err)
		}
		zones = defaultZones
	}

	// Delete public subnets
	for _, zone := range zones {
		publicSubnetName := fmt.Sprintf("%s-public-%s", infraID, zone)
		if err := sm.deleteSubnet(ctx, publicSubnetName); err != nil {
			return fmt.Errorf("failed to delete public subnet %s: %w", publicSubnetName, err)
		}
	}

	// Delete private subnets if not public-only
	if !publicOnly {
		for _, zone := range zones {
			privateSubnetName := fmt.Sprintf("%s-private-%s", infraID, zone)
			if err := sm.deleteSubnet(ctx, privateSubnetName); err != nil {
				return fmt.Errorf("failed to delete private subnet %s: %w", privateSubnetName, err)
			}
		}
	}

	sm.logger.Info("Successfully deleted all subnets")
	return nil
}

// deleteSubnet deletes a single subnet
func (sm *SubnetManager) deleteSubnet(ctx context.Context, subnetName string) error {
	sm.logger.Info("Deleting subnet", "name", subnetName)

	op, err := sm.clients.Compute.Subnetworks.Delete(sm.projectID, sm.region, subnetName).Context(ctx).Do()
	if err != nil {
		return gcputil.HandleResourceDeletionError(err, "subnet", subnetName)
	}

	// Wait for deletion to complete
	err = sm.waitForRegionalOperation(ctx, op.Name, fmt.Sprintf("subnet %s deletion", subnetName))
	if err != nil {
		return fmt.Errorf("failed waiting for subnet deletion: %w", err)
	}

	sm.logger.Info("Successfully deleted subnet", "name", subnetName)
	return nil
}

// getExistingSubnet checks if a subnet already exists
func (sm *SubnetManager) getExistingSubnet(ctx context.Context, subnetName string) (*compute.Subnetwork, error) {
	subnet, err := sm.clients.Compute.Subnetworks.Get(sm.projectID, sm.region, subnetName).Context(ctx).Do()
	if err != nil {
		if gcputil.IsGCPNotFoundError(err) {
			return nil, nil // Subnet doesn't exist
		}
		return nil, fmt.Errorf("failed to check existing subnet %s: %w", subnetName, err)
	}
	return subnet, nil
}

// getDefaultZones gets the default zones for the region
func (sm *SubnetManager) getDefaultZones(ctx context.Context) ([]string, error) {
	// For simplicity, use the first 3 zones in the region
	// In production, this could be configurable or auto-discovered
	regionZones := []string{
		sm.region + "-a",
		sm.region + "-b",
		sm.region + "-c",
	}

	// Validate that zones exist by trying to get them
	var validZones []string
	for _, zone := range regionZones {
		_, err := sm.clients.Compute.Zones.Get(sm.projectID, zone).Context(ctx).Do()
		if err == nil {
			validZones = append(validZones, zone)
		} else if !gcputil.IsGCPNotFoundError(err) {
			return nil, fmt.Errorf("failed to check zone %s: %w", zone, err)
		}
		// If zone doesn't exist, skip it
	}

	if len(validZones) == 0 {
		return nil, fmt.Errorf("no valid zones found for region %s", sm.region)
	}

	return validZones, nil
}

// calculateSubnetCIDR calculates subnet CIDR from VPC CIDR
func (sm *SubnetManager) calculateSubnetCIDR(vpcNet *net.IPNet, index int, prefixLen int) (string, error) {
	// Convert VPC network to /20 subnets (AWS pattern)
	vpcIP := vpcNet.IP
	vpcMask, _ := vpcNet.Mask.Size()

	if prefixLen <= vpcMask {
		return "", fmt.Errorf("subnet prefix length %d must be greater than VPC prefix length %d", prefixLen, vpcMask)
	}

	// Calculate the subnet increment
	subnetSize := 1 << (32 - prefixLen)
	subnetIncrement := uint32(subnetSize)

	// Calculate the subnet IP
	vpcIPInt := ip4ToInt(vpcIP)
	subnetIPInt := vpcIPInt + uint32(index)*subnetIncrement

	// Check if we're within the VPC range
	maxSubnetIPInt := vpcIPInt + (1 << (32 - vpcMask))
	if subnetIPInt+subnetIncrement > maxSubnetIPInt {
		return "", fmt.Errorf("subnet %d would exceed VPC CIDR range", index)
	}

	subnetIP := intToIP4(subnetIPInt)
	return fmt.Sprintf("%s/%d", subnetIP.String(), prefixLen), nil
}

// waitForRegionalOperation waits for a regional operation to complete
func (sm *SubnetManager) waitForRegionalOperation(ctx context.Context, operationName, description string) error {
	sm.logger.Info("Waiting for regional operation", "operation", operationName, "description", description)

	return wait.PollUntilContextTimeout(ctx, 5*time.Second, gcpOperationTimeoutMinutes*time.Minute, true, func(ctx context.Context) (bool, error) {
		op, err := sm.clients.Compute.RegionOperations.Get(sm.projectID, sm.region, operationName).Context(ctx).Do()
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
			sm.logger.V(1).Info("Operation in progress", "status", op.Status, "progress", op.Progress)
			return false, nil
		default:
			return false, fmt.Errorf("unexpected operation status: %s", op.Status)
		}
	})
}

// Helper functions for IP calculations
func ip4ToInt(ip net.IP) uint32 {
	ip = ip.To4()
	return uint32(ip[0])<<24 + uint32(ip[1])<<16 + uint32(ip[2])<<8 + uint32(ip[3])
}

func intToIP4(i uint32) net.IP {
	return net.IPv4(byte(i>>24), byte(i>>16), byte(i>>8), byte(i))
}
