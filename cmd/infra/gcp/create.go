package gcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	gcputil "github.com/openshift/hypershift/cmd/infra/gcp/util"
	"github.com/openshift/hypershift/cmd/log"
	"github.com/openshift/hypershift/cmd/util"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
)

type CreateInfraOptions struct {
	// Basic options (mirror AWS exactly)
	ProjectID        string
	Region           string
	InfraID          string
	Name             string
	BaseDomain       string
	BaseDomainPrefix string
	Zones            []string
	OutputFile       string
	AdditionalLabels []string

	// Network options (mirror AWS)
	VPCCIDR           string
	EnableProxy       bool
	EnableSecureProxy bool
	SingleNATGateway  bool
	PublicOnly        bool

	// GCP-specific credentials
	GCPCredentialsOpts gcputil.GCPCredentialsOptions

	// Internal state
	CredentialsSecretData *util.CredentialsSecretData
	additionalLabels      map[string]string
}

type CreateInfraOutput struct {
	// Mirror AWS output structure exactly
	Region             string                   `json:"region"`
	ProjectID          string                   `json:"projectID"`
	InfraID            string                   `json:"infraID"`
	VPCName            string                   `json:"vpcName"`
	MachineCIDR        string                   `json:"machineCIDR"`
	Zones              []*CreateInfraOutputZone `json:"zones"`
	Name               string                   `json:"name"`
	BaseDomain         string                   `json:"baseDomain"`
	BaseDomainPrefix   string                   `json:"baseDomainPrefix"`
	PublicZoneName     string                   `json:"publicZoneName"`
	PrivateZoneName    string                   `json:"privateZoneName"`
	LocalZoneName      string                   `json:"localZoneName"`
	ProxyAddr          string                   `json:"proxyAddr"`
	SecureProxyAddr    string                   `json:"secureProxyAddr"`
	ProxyPrivateSSHKey string                   `json:"proxyPrivateSSHKey"`
	ProxyCA            string                   `json:"proxyCA"`
	PublicOnly         bool                     `json:"publicOnly"`
}

type CreateInfraOutputZone struct {
	Name       string `json:"name"`
	SubnetName string `json:"subnetName"`
}

const (
	DefaultCIDRBlock      = "10.0.0.0/16"
	basePrivateSubnetCIDR = "10.0.128.0/20"
	basePublicSubnetCIDR  = "10.0.0.0/20"

	clusterLabelValue       = "owned"
	hypershiftLocalZoneName = "hypershift.local"
)

func NewCreateCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "gcp",
		Short:        "Creates GCP infrastructure resources for a cluster",
		SilenceUsage: true,
	}

	opts := CreateInfraOptions{
		Region:  "us-central1",
		Name:    "example",
		VPCCIDR: DefaultCIDRBlock,
	}

	// Mirror AWS flags exactly
	cmd.Flags().StringVar(&opts.InfraID, "infra-id", opts.InfraID, "Cluster ID with which to label GCP resources (required)")
	cmd.Flags().StringVar(&opts.ProjectID, "project-id", opts.ProjectID, "GCP Project ID where resources will be created (required)")
	cmd.Flags().StringVar(&opts.OutputFile, "output-file", opts.OutputFile, "Path to file that will contain output information from infra resources (optional)")
	cmd.Flags().StringVar(&opts.Region, "region", opts.Region, "Region where cluster infra should be created")
	cmd.Flags().StringSliceVar(&opts.AdditionalLabels, "additional-labels", opts.AdditionalLabels, "Additional labels to set on GCP resources")
	cmd.Flags().StringVar(&opts.Name, "name", opts.Name, "A name for the cluster")
	cmd.Flags().StringVar(&opts.BaseDomain, "base-domain", opts.BaseDomain, "The ingress base domain for the cluster")
	cmd.Flags().StringVar(&opts.BaseDomainPrefix, "base-domain-prefix", opts.BaseDomainPrefix, "The ingress base domain prefix for the cluster, defaults to cluster name. Use 'none' for an empty prefix")
	cmd.Flags().StringSliceVar(&opts.Zones, "zones", opts.Zones, "The availability zones in which NodePool can be created")
	cmd.Flags().BoolVar(&opts.EnableProxy, "enable-proxy", opts.EnableProxy, "If true, a proxy should be set up, rather than allowing direct internet access from the nodes")
	cmd.Flags().BoolVar(&opts.EnableSecureProxy, "enable-secure-proxy", opts.EnableSecureProxy, "If true, a secure proxy should be set up, rather than allowing direct internet access from the nodes")
	cmd.Flags().BoolVar(&opts.SingleNATGateway, "single-nat-gateway", opts.SingleNATGateway, "If enabled, only a single NAT gateway is created, even if multiple zones are specified")
	cmd.Flags().StringVar(&opts.VPCCIDR, "vpc-cidr", opts.VPCCIDR, "The CIDR to use for the cluster VPC")
	cmd.Flags().BoolVar(&opts.PublicOnly, "public-only", opts.PublicOnly, "If true, no private subnets or NAT gateway are created")

	_ = cmd.MarkFlagRequired("infra-id")
	_ = cmd.MarkFlagRequired("project-id")
	_ = cmd.MarkFlagRequired("base-domain")

	opts.GCPCredentialsOpts.BindFlags(cmd.Flags())

	logger := log.Log
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		err := opts.GCPCredentialsOpts.Validate()
		if err != nil {
			return err
		}
		if err = opts.Validate(); err != nil {
			return err
		}
		if err := opts.Run(cmd.Context(), logger); err != nil {
			logger.Error(err, "Failed to create infrastructure")
			return err
		}
		logger.Info("Successfully created infrastructure")
		return nil
	}

	return cmd
}

func (o *CreateInfraOptions) Run(ctx context.Context, l logr.Logger) error {
	result, err := o.CreateInfra(ctx, l)
	if err != nil {
		return err
	}
	return o.Output(result)
}

func (o *CreateInfraOptions) Validate() error {
	if o.EnableProxy && o.EnableSecureProxy {
		return fmt.Errorf("specify either --enable-proxy or --enable-secure-proxy, but not both")
	}
	return nil
}

func (o *CreateInfraOptions) Output(result *CreateInfraOutput) error {
	// Mirror AWS output logic exactly
	out := os.Stdout
	if len(o.OutputFile) > 0 {
		var err error
		out, err = os.Create(o.OutputFile)
		if err != nil {
			return fmt.Errorf("cannot create output file: %w", err)
		}
		defer func(out *os.File) {
			_ = out.Close()
		}(out)
	}
	outputBytes, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to serialize result: %w", err)
	}
	_, err = out.Write(outputBytes)
	if err != nil {
		return fmt.Errorf("failed to write result: %w", err)
	}
	return nil
}

func (o *CreateInfraOptions) CreateInfra(ctx context.Context, l logr.Logger) (*CreateInfraOutput, error) {
	l.Info("Creating GCP infrastructure", "infraID", o.InfraID, "project", o.ProjectID)

	if o.VPCCIDR == "" {
		o.VPCCIDR = DefaultCIDRBlock
	}

	if err := gcputil.ValidateVPCCIDR(o.VPCCIDR); err != nil {
		return nil, err
	}

	clients, err := o.GCPCredentialsOpts.GetClients(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create GCP clients: %w", err)
	}

	if err := o.parseAdditionalLabels(); err != nil {
		return nil, err
	}

	result := &CreateInfraOutput{
		InfraID:          o.InfraID,
		ProjectID:        o.ProjectID,
		MachineCIDR:      o.VPCCIDR,
		Region:           o.Region,
		Name:             o.Name,
		BaseDomain:       o.BaseDomain,
		BaseDomainPrefix: o.BaseDomainPrefix,
		PublicOnly:       o.PublicOnly,
	}

	// Create VPC network
	vpcManager := NewVPCManager(clients, o.ProjectID, o.Region, l)
	vpcName, err := vpcManager.CreateVPC(ctx, o.InfraID, o.VPCCIDR, o.additionalLabels)
	if err != nil {
		return nil, fmt.Errorf("failed to create VPC: %w", err)
	}
	result.VPCName = vpcName

	// Create subnets
	subnetManager := NewSubnetManager(clients, o.ProjectID, o.Region, l)
	subnets, err := subnetManager.CreateSubnets(ctx, o.InfraID, vpcName, o.VPCCIDR, o.Zones, o.PublicOnly, o.additionalLabels)
	if err != nil {
		return nil, fmt.Errorf("failed to create subnets: %w", err)
	}

	// Create DNS zones
	dnsManager := NewDNSManager(clients, o.ProjectID, o.Region, l)
	dnsZones, err := dnsManager.CreateDNSZones(ctx, o.InfraID, o.BaseDomain, o.BaseDomainPrefix, vpcName, o.additionalLabels)
	if err != nil {
		return nil, fmt.Errorf("failed to create DNS zones: %w", err)
	}

	// Create firewall rules
	firewallManager := NewFirewallManager(clients, o.ProjectID, o.Region, l)
	firewallRules, err := firewallManager.CreateFirewallRules(ctx, o.InfraID, vpcName, o.VPCCIDR, o.additionalLabels)
	if err != nil {
		return nil, fmt.Errorf("failed to create firewall rules: %w", err)
	}

	// Create NAT gateway for private subnets (if not public-only)
	var natGateway *NATGatewayDetails
	if !o.PublicOnly {
		// Collect private subnet names
		var privateSubnets []string
		for _, subnet := range subnets {
			if subnet.Type == "private" {
				privateSubnets = append(privateSubnets, subnet.Name)
			}
		}

		if len(privateSubnets) > 0 {
			natManager := NewNATManager(clients, o.ProjectID, o.Region, l)
			natGateway, err = natManager.CreateNATGateway(ctx, o.InfraID, vpcName, privateSubnets, o.SingleNATGateway, o.additionalLabels)
			if err != nil {
				return nil, fmt.Errorf("failed to create NAT gateway: %w", err)
			}
		}
	}

	// Populate DNS zone names in output (following AWS pattern)
	for _, zone := range dnsZones {
		switch zone.Type {
		case "public":
			result.PublicZoneName = zone.Name
		case "private":
			result.PrivateZoneName = zone.Name
		case "local":
			result.LocalZoneName = zone.Name
		}
	}

	// Populate zones in output
	result.Zones = make([]*CreateInfraOutputZone, 0, len(subnets))
	for _, subnet := range subnets {
		if subnet.Type == "public" { // Only add zones for public subnets to match AWS pattern
			result.Zones = append(result.Zones, &CreateInfraOutputZone{
				Name:       subnet.Zone,
				SubnetName: subnet.Name,
			})
		}
	}

	// Create custom routes (if any are needed)
	routeManager := NewRouteManager(clients, o.ProjectID, o.Region, l)
	routes, err := routeManager.CreateRoutes(ctx, o.InfraID, vpcName, o.additionalLabels)
	if err != nil {
		return nil, fmt.Errorf("failed to create routes: %w", err)
	}

	// Create proxy VM if enabled
	var proxyDetails *ProxyVMDetails
	if o.EnableProxy || o.EnableSecureProxy {
		// Find the first public subnet for proxy placement
		var publicSubnetName string
		for _, subnet := range subnets {
			if subnet.Type == "public" {
				publicSubnetName = subnet.Name
				break
			}
		}

		if publicSubnetName == "" {
			return nil, fmt.Errorf("no public subnet found for proxy VM placement")
		}

		// Create proxy firewall rule first
		firewallManager := NewFirewallManager(clients, o.ProjectID, o.Region, l)
		_, err = firewallManager.CreateProxyFirewallRule(ctx, o.InfraID, vpcName, o.VPCCIDR, o.additionalLabels)
		if err != nil {
			return nil, fmt.Errorf("failed to create proxy firewall rule: %w", err)
		}

		// Create proxy VM
		computeManager := NewComputeManager(clients, o.ProjectID, o.Region, l)
		proxyDetails, err = computeManager.CreateProxyVM(ctx, o.InfraID, publicSubnetName, fmt.Sprintf("%s-proxy", o.InfraID), o.EnableSecureProxy, o.additionalLabels)
		if err != nil {
			return nil, fmt.Errorf("failed to create proxy VM: %w", err)
		}

		// Update result with proxy information
		result.ProxyAddr = proxyDetails.HTTPProxyURL
		result.ProxyPrivateSSHKey = proxyDetails.PrivateSSHKey
		if o.EnableSecureProxy {
			// For secure proxy, populate the HTTPS URL and CA certificate
			result.SecureProxyAddr = proxyDetails.HTTPSProxyURL
			if proxyDetails.CA != "" {
				result.ProxyCA = proxyDetails.CA
				l.Info("Secure proxy created with CA certificate", "httpsProxyURL", proxyDetails.HTTPSProxyURL, "caLength", len(proxyDetails.CA))
			} else {
				l.Info("Secure proxy created", "httpsProxyURL", proxyDetails.HTTPSProxyURL, "note", "CA certificate retrieval failed")
			}
		}
	}

	natCount := 0
	if natGateway != nil {
		natCount = 1
	}

	proxyCount := 0
	if proxyDetails != nil {
		proxyCount = 1
	}

	l.Info("Successfully created infrastructure", "vpc", vpcName, "subnets", len(subnets), "dnsZones", len(dnsZones), "firewallRules", len(firewallRules), "natGateways", natCount, "customRoutes", len(routes), "proxyVMs", proxyCount)
	return result, nil
}

func (o *CreateInfraOptions) parseAdditionalLabels() error {
	var err error
	o.additionalLabels, err = gcputil.ParseAdditionalLabels(o.AdditionalLabels)
	if err != nil {
		return err
	}

	// Add cluster label
	if o.additionalLabels == nil {
		o.additionalLabels = make(map[string]string)
	}
	o.additionalLabels[fmt.Sprintf("kubernetes.io/cluster/%s", o.InfraID)] = clusterLabelValue

	return nil
}
