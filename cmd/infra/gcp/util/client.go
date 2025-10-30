package util

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/pflag"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/cloudresourcemanager/v1"
	"google.golang.org/api/compute/v1"
	"google.golang.org/api/dns/v1"
	"google.golang.org/api/iam/v1"
	"google.golang.org/api/option"
)

type GCPCredentialsOptions struct {
	GCPCredentialsFile string
	GCPProject         string
	GCPRegion          string
}

func (opts *GCPCredentialsOptions) BindFlags(flags *pflag.FlagSet) {
	flags.StringVar(&opts.GCPCredentialsFile, "gcp-creds", opts.GCPCredentialsFile, "Path to GCP service account key file")
	flags.StringVar(&opts.GCPProject, "gcp-project", opts.GCPProject, "GCP Project ID")
	flags.StringVar(&opts.GCPRegion, "gcp-region", opts.GCPRegion, "GCP Region")
}

func (opts *GCPCredentialsOptions) Validate() error {
	if opts.GCPCredentialsFile != "" {
		if _, err := os.Stat(opts.GCPCredentialsFile); os.IsNotExist(err) {
			return fmt.Errorf("GCP credentials file does not exist: %s", opts.GCPCredentialsFile)
		}
	}
	return nil
}

func (opts *GCPCredentialsOptions) GetClients(ctx context.Context) (*GCPClients, error) {
	var clientOpts []option.ClientOption

	// Handle credentials
	if opts.GCPCredentialsFile != "" {
		clientOpts = append(clientOpts, option.WithCredentialsFile(opts.GCPCredentialsFile))
	} else {
		// Use Application Default Credentials
		creds, err := google.FindDefaultCredentials(ctx,
			compute.ComputeScope,
			iam.CloudPlatformScope,
			dns.CloudPlatformScope,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to find default credentials: %w", err)
		}
		clientOpts = append(clientOpts, option.WithCredentials(creds))
	}

	// Create services
	computeService, err := compute.NewService(ctx, clientOpts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create compute service: %w", err)
	}

	iamService, err := iam.NewService(ctx, clientOpts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create IAM service: %w", err)
	}

	dnsService, err := dns.NewService(ctx, clientOpts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create DNS service: %w", err)
	}

	crmService, err := cloudresourcemanager.NewService(ctx, clientOpts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create Cloud Resource Manager service: %w", err)
	}

	clients := &GCPClients{
		Compute: computeService,
		IAM:     iamService,
		DNS:     dnsService,
		CRM:     crmService,
	}

	// Validate access
	err = clients.ValidateAccess(ctx, opts.GCPProject)
	if err != nil {
		return nil, fmt.Errorf("failed to validate GCP access: %w", err)
	}

	return clients, nil
}

type GCPClients struct {
	Compute *compute.Service
	IAM     *iam.Service
	DNS     *dns.Service
	CRM     *cloudresourcemanager.Service
}

func (c *GCPClients) ValidateAccess(ctx context.Context, projectID string) error {
	if projectID == "" {
		return fmt.Errorf("GCP project ID is required")
	}

	// Validate project access
	project, err := c.CRM.Projects.Get(projectID).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("failed to access project %s: %w", projectID, err)
	}

	if project.LifecycleState != "ACTIVE" {
		return fmt.Errorf("project %s is not active (state: %s)", projectID, project.LifecycleState)
	}

	// Validate compute access
	_, err = c.Compute.Zones.List(projectID).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("failed to access Compute Engine API: %w", err)
	}

	// Validate IAM access
	_, err = c.IAM.Projects.ServiceAccounts.List(fmt.Sprintf("projects/%s", projectID)).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("failed to access IAM API: %w", err)
	}

	// Validate DNS access
	_, err = c.DNS.ManagedZones.List(projectID).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("failed to access Cloud DNS API: %w", err)
	}

	return nil
}

func (c *GCPClients) GetProjectNumber(ctx context.Context, projectID string) (int64, error) {
	project, err := c.CRM.Projects.Get(projectID).Context(ctx).Do()
	if err != nil {
		return 0, fmt.Errorf("failed to get project details: %w", err)
	}
	return project.ProjectNumber, nil
}
