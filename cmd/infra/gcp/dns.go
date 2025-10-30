package gcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	"google.golang.org/api/dns/v1"

	gcputil "github.com/openshift/hypershift/cmd/infra/gcp/util"
)

// DNSManager handles DNS zone operations for GCP
type DNSManager struct {
	clients   *gcputil.GCPClients
	projectID string
	region    string
	logger    logr.Logger
}

// DNSZoneDetails represents DNS zone information for output
type DNSZoneDetails struct {
	Name              string            `json:"name"`
	DNSName           string            `json:"dnsName"`
	Type              string            `json:"type"` // "public", "private", or "local"
	ID                string            `json:"id"`
	NameServers       []string          `json:"nameServers,omitempty"`
	Labels            map[string]string `json:"labels"`
	CreationTimestamp string            `json:"creationTimestamp"`
}

// NewDNSManager creates a new DNS manager
func NewDNSManager(clients *gcputil.GCPClients, projectID, region string, logger logr.Logger) *DNSManager {
	return &DNSManager{
		clients:   clients,
		projectID: projectID,
		region:    region,
		logger:    logger,
	}
}

// CreateDNSZones creates DNS zones for the cluster following AWS Route53 patterns
func (dm *DNSManager) CreateDNSZones(ctx context.Context, infraID, baseDomain, baseDomainPrefix, vpcName string, labels map[string]string) ([]*DNSZoneDetails, error) {
	dm.logger.Info("Creating DNS zones", "infraID", infraID, "baseDomain", baseDomain, "baseDomainPrefix", baseDomainPrefix)

	var zones []*DNSZoneDetails

	// Determine the cluster domain
	clusterDomain := dm.buildClusterDomain(baseDomain, baseDomainPrefix)

	// Create public DNS zone for external resolution
	publicZone, err := dm.createPublicDNSZone(ctx, infraID, clusterDomain, labels)
	if err != nil {
		return nil, fmt.Errorf("failed to create public DNS zone: %w", err)
	}
	zones = append(zones, publicZone)

	// Create private DNS zone for internal cluster resolution
	privateZone, err := dm.createPrivateDNSZone(ctx, infraID, clusterDomain, vpcName, labels)
	if err != nil {
		return nil, fmt.Errorf("failed to create private DNS zone: %w", err)
	}
	zones = append(zones, privateZone)

	// Create local DNS zone (equivalent to hypershift.local in AWS)
	localZone, err := dm.createLocalDNSZone(ctx, infraID, vpcName, labels)
	if err != nil {
		return nil, fmt.Errorf("failed to create local DNS zone: %w", err)
	}
	zones = append(zones, localZone)

	dm.logger.Info("Successfully created all DNS zones", "count", len(zones))
	return zones, nil
}

// createPublicDNSZone creates a public DNS zone for external resolution
func (dm *DNSManager) createPublicDNSZone(ctx context.Context, infraID, clusterDomain string, labels map[string]string) (*DNSZoneDetails, error) {
	zoneName := fmt.Sprintf("%s-public", infraID)
	dnsName := fmt.Sprintf("%s.", clusterDomain) // Cloud DNS requires trailing dot

	// Check if zone already exists
	existingZone, err := dm.getExistingDNSZone(ctx, zoneName)
	if err != nil {
		return nil, err
	}

	if existingZone != nil {
		dm.logger.Info("Found existing public DNS zone", "name", zoneName, "dnsName", existingZone.DnsName)
		return &DNSZoneDetails{
			Name:              existingZone.Name,
			DNSName:           strings.TrimSuffix(existingZone.DnsName, "."),
			Type:              "public",
			ID:                fmt.Sprintf("%d", existingZone.Id),
			NameServers:       existingZone.NameServers,
			Labels:            existingZone.Labels,
			CreationTimestamp: existingZone.CreationTime,
		}, nil
	}

	zone := &dns.ManagedZone{
		Name:        zoneName,
		DnsName:     dnsName,
		Description: fmt.Sprintf("HyperShift public DNS zone for cluster %s", infraID),
		Visibility:  "public",
		Labels:      labels,
	}

	dm.logger.Info("Creating public DNS zone", "name", zoneName, "dnsName", dnsName)

	createdZone, err := dm.clients.DNS.ManagedZones.Create(dm.projectID, zone).Context(ctx).Do()
	if err != nil {
		return nil, gcputil.HandleResourceCreationError(err, "public DNS zone", zoneName)
	}

	dm.logger.Info("Successfully created public DNS zone", "name", zoneName)

	return &DNSZoneDetails{
		Name:              createdZone.Name,
		DNSName:           strings.TrimSuffix(createdZone.DnsName, "."),
		Type:              "public",
		ID:                fmt.Sprintf("%d", createdZone.Id),
		NameServers:       createdZone.NameServers,
		Labels:            createdZone.Labels,
		CreationTimestamp: createdZone.CreationTime,
	}, nil
}

// createPrivateDNSZone creates a private DNS zone for internal cluster resolution
func (dm *DNSManager) createPrivateDNSZone(ctx context.Context, infraID, clusterDomain, vpcName string, labels map[string]string) (*DNSZoneDetails, error) {
	zoneName := fmt.Sprintf("%s-private", infraID)
	dnsName := fmt.Sprintf("%s.", clusterDomain) // Cloud DNS requires trailing dot

	// Check if zone already exists
	existingZone, err := dm.getExistingDNSZone(ctx, zoneName)
	if err != nil {
		return nil, err
	}

	if existingZone != nil {
		dm.logger.Info("Found existing private DNS zone", "name", zoneName, "dnsName", existingZone.DnsName)
		return &DNSZoneDetails{
			Name:              existingZone.Name,
			DNSName:           strings.TrimSuffix(existingZone.DnsName, "."),
			Type:              "private",
			ID:                fmt.Sprintf("%d", existingZone.Id),
			Labels:            existingZone.Labels,
			CreationTimestamp: existingZone.CreationTime,
		}, nil
	}

	// Build VPC network reference
	vpcSelfLink := fmt.Sprintf("https://www.googleapis.com/compute/v1/projects/%s/global/networks/%s", dm.projectID, vpcName)

	zone := &dns.ManagedZone{
		Name:        zoneName,
		DnsName:     dnsName,
		Description: fmt.Sprintf("HyperShift private DNS zone for cluster %s", infraID),
		Visibility:  "private",
		Labels:      labels,
		PrivateVisibilityConfig: &dns.ManagedZonePrivateVisibilityConfig{
			Networks: []*dns.ManagedZonePrivateVisibilityConfigNetwork{
				{
					NetworkUrl: vpcSelfLink,
				},
			},
		},
	}

	dm.logger.Info("Creating private DNS zone", "name", zoneName, "dnsName", dnsName, "vpc", vpcName)

	createdZone, err := dm.clients.DNS.ManagedZones.Create(dm.projectID, zone).Context(ctx).Do()
	if err != nil {
		return nil, gcputil.HandleResourceCreationError(err, "private DNS zone", zoneName)
	}

	dm.logger.Info("Successfully created private DNS zone", "name", zoneName)

	return &DNSZoneDetails{
		Name:              createdZone.Name,
		DNSName:           strings.TrimSuffix(createdZone.DnsName, "."),
		Type:              "private",
		ID:                fmt.Sprintf("%d", createdZone.Id),
		Labels:            createdZone.Labels,
		CreationTimestamp: createdZone.CreationTime,
	}, nil
}

// createLocalDNSZone creates a local DNS zone (equivalent to hypershift.local in AWS)
func (dm *DNSManager) createLocalDNSZone(ctx context.Context, infraID, vpcName string, labels map[string]string) (*DNSZoneDetails, error) {
	zoneName := fmt.Sprintf("%s-local", infraID)
	dnsName := "hypershift.local." // Match AWS pattern

	// Check if zone already exists
	existingZone, err := dm.getExistingDNSZone(ctx, zoneName)
	if err != nil {
		return nil, err
	}

	if existingZone != nil {
		dm.logger.Info("Found existing local DNS zone", "name", zoneName, "dnsName", existingZone.DnsName)
		return &DNSZoneDetails{
			Name:              existingZone.Name,
			DNSName:           strings.TrimSuffix(existingZone.DnsName, "."),
			Type:              "local",
			ID:                fmt.Sprintf("%d", existingZone.Id),
			Labels:            existingZone.Labels,
			CreationTimestamp: existingZone.CreationTime,
		}, nil
	}

	// Build VPC network reference
	vpcSelfLink := fmt.Sprintf("https://www.googleapis.com/compute/v1/projects/%s/global/networks/%s", dm.projectID, vpcName)

	zone := &dns.ManagedZone{
		Name:        zoneName,
		DnsName:     dnsName,
		Description: fmt.Sprintf("HyperShift local DNS zone for cluster %s", infraID),
		Visibility:  "private",
		Labels:      labels,
		PrivateVisibilityConfig: &dns.ManagedZonePrivateVisibilityConfig{
			Networks: []*dns.ManagedZonePrivateVisibilityConfigNetwork{
				{
					NetworkUrl: vpcSelfLink,
				},
			},
		},
	}

	dm.logger.Info("Creating local DNS zone", "name", zoneName, "dnsName", dnsName, "vpc", vpcName)

	createdZone, err := dm.clients.DNS.ManagedZones.Create(dm.projectID, zone).Context(ctx).Do()
	if err != nil {
		return nil, gcputil.HandleResourceCreationError(err, "local DNS zone", zoneName)
	}

	dm.logger.Info("Successfully created local DNS zone", "name", zoneName)

	return &DNSZoneDetails{
		Name:              createdZone.Name,
		DNSName:           strings.TrimSuffix(createdZone.DnsName, "."),
		Type:              "local",
		ID:                fmt.Sprintf("%d", createdZone.Id),
		Labels:            createdZone.Labels,
		CreationTimestamp: createdZone.CreationTime,
	}, nil
}

// DeleteDNSZones deletes all DNS zones for a cluster
func (dm *DNSManager) DeleteDNSZones(ctx context.Context, infraID string) error {
	dm.logger.Info("Deleting DNS zones", "infraID", infraID)

	zoneNames := []string{
		fmt.Sprintf("%s-local", infraID),
		fmt.Sprintf("%s-private", infraID),
		fmt.Sprintf("%s-public", infraID),
	}

	for _, zoneName := range zoneNames {
		if err := dm.deleteDNSZone(ctx, zoneName); err != nil {
			// Don't fail the entire operation if zone deletion fails - it might not exist
			dm.logger.Error(err, "Failed to delete DNS zone, continuing", "zone", zoneName)
		} else {
			dm.logger.Info("Successfully deleted DNS zone", "name", zoneName)
		}
	}

	dm.logger.Info("Successfully deleted all DNS zones")
	return nil
}

// deleteDNSZone deletes a single DNS zone
func (dm *DNSManager) deleteDNSZone(ctx context.Context, zoneName string) error {
	dm.logger.Info("Deleting DNS zone", "name", zoneName)

	err := dm.clients.DNS.ManagedZones.Delete(dm.projectID, zoneName).Context(ctx).Do()
	if err != nil {
		return gcputil.HandleResourceDeletionError(err, "DNS zone", zoneName)
	}

	dm.logger.Info("Successfully deleted DNS zone", "name", zoneName)
	return nil
}

// getExistingDNSZone checks if a DNS zone already exists
func (dm *DNSManager) getExistingDNSZone(ctx context.Context, zoneName string) (*dns.ManagedZone, error) {
	zone, err := dm.clients.DNS.ManagedZones.Get(dm.projectID, zoneName).Context(ctx).Do()
	if err != nil {
		if gcputil.IsGCPNotFoundError(err) {
			return nil, nil // Zone doesn't exist
		}
		return nil, fmt.Errorf("failed to check existing DNS zone %s: %w", zoneName, err)
	}
	return zone, nil
}

// buildClusterDomain builds the cluster domain from base domain and prefix
func (dm *DNSManager) buildClusterDomain(baseDomain, baseDomainPrefix string) string {
	if baseDomainPrefix == "" || baseDomainPrefix == "none" {
		return baseDomain
	}
	return fmt.Sprintf("%s.%s", baseDomainPrefix, baseDomain)
}
