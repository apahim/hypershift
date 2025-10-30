package gcp

import (
	"context"
	"fmt"

	gcputil "github.com/openshift/hypershift/cmd/infra/gcp/util"
	"github.com/openshift/hypershift/cmd/log"
	"github.com/openshift/hypershift/cmd/util"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
)

type DestroyInfraOptions struct {
	ProjectID             string
	Region                string
	InfraID               string
	GCPCredentialsOpts    gcputil.GCPCredentialsOptions
	CredentialsSecretData *util.CredentialsSecretData
}

func NewDestroyCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "gcp",
		Short:        "Destroys GCP infrastructure resources for a cluster",
		SilenceUsage: true,
	}

	opts := DestroyInfraOptions{
		Region: "us-central1",
	}

	cmd.Flags().StringVar(&opts.InfraID, "infra-id", opts.InfraID, "Infrastructure ID of the cluster to destroy (required)")
	cmd.Flags().StringVar(&opts.ProjectID, "project-id", opts.ProjectID, "GCP Project ID where resources exist (required)")
	cmd.Flags().StringVar(&opts.Region, "region", opts.Region, "Region where cluster infra exists")

	opts.GCPCredentialsOpts.BindFlags(cmd.Flags())

	_ = cmd.MarkFlagRequired("infra-id")
	_ = cmd.MarkFlagRequired("project-id")

	logger := log.Log
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if err := opts.Run(cmd.Context(), logger); err != nil {
			logger.Error(err, "Failed to destroy infrastructure")
			return err
		}
		logger.Info("Successfully destroyed infrastructure")
		return nil
	}

	return cmd
}

func (o *DestroyInfraOptions) Run(ctx context.Context, logger logr.Logger) error {
	return o.DestroyInfra(ctx, logger)
}

func (o *DestroyInfraOptions) DestroyInfra(ctx context.Context, logger logr.Logger) error {
	logger.Info("Destroying GCP infrastructure", "infraID", o.InfraID, "project", o.ProjectID)

	clients, err := o.GCPCredentialsOpts.GetClients(ctx)
	if err != nil {
		return fmt.Errorf("failed to create GCP clients: %w", err)
	}

	// Destroy resources in reverse order: custom routes first, then NAT gateways, then firewall rules, then DNS zones, then subnets, then VPC

	// Delete custom routes
	routeManager := NewRouteManager(clients, o.ProjectID, o.Region, logger)
	err = routeManager.DeleteRoutes(ctx, o.InfraID)
	if err != nil {
		// Don't fail the entire operation if route deletion fails - they might not exist
		logger.Error(err, "Failed to delete custom routes, continuing with proxy VM deletion")
	} else {
		logger.Info("Successfully deleted custom routes")
	}

	// Delete proxy VM (if it exists)
	computeManager := NewComputeManager(clients, o.ProjectID, o.Region, logger)
	err = computeManager.DeleteProxyVM(ctx, o.InfraID)
	if err != nil {
		// Don't fail the entire operation if proxy VM deletion fails - it might not exist
		logger.Error(err, "Failed to delete proxy VM, continuing with proxy firewall rule deletion")
	} else {
		logger.Info("Successfully deleted proxy VM")
	}

	// Delete proxy firewall rule (if it exists)
	firewallManager := NewFirewallManager(clients, o.ProjectID, o.Region, logger)
	err = firewallManager.DeleteProxyFirewallRule(ctx, o.InfraID)
	if err != nil {
		// Don't fail the entire operation if proxy firewall rule deletion fails - it might not exist
		logger.Error(err, "Failed to delete proxy firewall rule, continuing with NAT gateway deletion")
	} else {
		logger.Info("Successfully deleted proxy firewall rule")
	}

	// Delete NAT gateway
	natManager := NewNATManager(clients, o.ProjectID, o.Region, logger)
	err = natManager.DeleteNATGateway(ctx, o.InfraID)
	if err != nil {
		// Don't fail the entire operation if NAT gateway deletion fails - it might not exist
		logger.Error(err, "Failed to delete NAT gateway, continuing with firewall rule deletion")
	} else {
		logger.Info("Successfully deleted NAT gateway")
	}

	// Delete firewall rules
	firewallManager = NewFirewallManager(clients, o.ProjectID, o.Region, logger)
	err = firewallManager.DeleteFirewallRules(ctx, o.InfraID)
	if err != nil {
		// Don't fail the entire operation if firewall rule deletion fails - they might not exist
		logger.Error(err, "Failed to delete firewall rules, continuing with DNS zone deletion")
	} else {
		logger.Info("Successfully deleted firewall rules")
	}

	// Delete DNS zones
	dnsManager := NewDNSManager(clients, o.ProjectID, o.Region, logger)
	err = dnsManager.DeleteDNSZones(ctx, o.InfraID)
	if err != nil {
		// Don't fail the entire operation if DNS zone deletion fails - they might not exist
		logger.Error(err, "Failed to delete DNS zones, continuing with subnet deletion")
	} else {
		logger.Info("Successfully deleted DNS zones")
	}

	// Delete subnets
	subnetManager := NewSubnetManager(clients, o.ProjectID, o.Region, logger)
	err = subnetManager.DeleteSubnets(ctx, o.InfraID, nil, false) // Use default zones and include private subnets
	if err != nil {
		// Don't fail the entire operation if subnet deletion fails - they might not exist
		logger.Error(err, "Failed to delete subnets, continuing with VPC deletion")
	} else {
		logger.Info("Successfully deleted subnets")
	}

	// Delete VPC
	vpcManager := NewVPCManager(clients, o.ProjectID, o.Region, logger)
	vpcName := fmt.Sprintf("%s-vpc", o.InfraID)
	err = vpcManager.DeleteVPC(ctx, vpcName)
	if err != nil {
		return fmt.Errorf("failed to delete VPC: %w", err)
	}

	logger.Info("Successfully destroyed all infrastructure resources")
	return nil
}
