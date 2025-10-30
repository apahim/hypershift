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

type CreateIAMOptions struct {
	// Basic options (mirror AWS exactly)
	ProjectID        string
	Region           string
	InfraID          string
	IssuerURL        string
	OutputFile       string
	AdditionalLabels []string

	// OIDC storage options (GCP equivalent to AWS S3)
	OIDCStorageProviderGCSBucket string
	OIDCStorageProviderGCSRegion string

	// Credentials
	GCPCredentialsOpts gcputil.GCPCredentialsOptions

	// Internal state
	CredentialsSecretData *util.CredentialsSecretData
	additionalLabels      map[string]string
}

type CreateIAMOutput struct {
	// Mirror AWS output structure
	Region           string                    `json:"region"`
	ProjectID        string                    `json:"projectID"`
	InfraID          string                    `json:"infraID"`
	IssuerURL        string                    `json:"issuerURL"`
	WorkloadIdentity GCPWorkloadIdentityOutput `json:"workloadIdentity"`
	ServiceAccounts  GCPServiceAccountsOutput  `json:"serviceAccounts"`
}

type GCPWorkloadIdentityOutput struct {
	PoolID     string `json:"poolID"`
	ProviderID string `json:"providerID"`
	PoolName   string `json:"poolName"`
	Audience   string `json:"audience"`
}

type GCPServiceAccountsOutput struct {
	Ingress         string `json:"ingress"`
	ImageRegistry   string `json:"imageRegistry"`
	Storage         string `json:"storage"`
	CloudController string `json:"cloudController"`
	ControlPlane    string `json:"controlPlane"`
}

func NewCreateIAMCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "gcp",
		Short:        "Creates GCP WIF and service account resources",
		SilenceUsage: true,
	}

	opts := CreateIAMOptions{
		Region:  "us-central1",
		InfraID: "",
	}

	// Mirror AWS IAM flags exactly
	cmd.Flags().StringVar(&opts.InfraID, "infra-id", opts.InfraID, "Infrastructure ID to use for GCP resources.")
	cmd.Flags().StringVar(&opts.ProjectID, "project-id", opts.ProjectID, "GCP Project ID where WIF resources will be created (required)")
	cmd.Flags().StringVar(&opts.OIDCStorageProviderGCSBucket, "oidc-storage-provider-gcs-bucket", "", "The name of the GCS bucket where the OIDC discovery document is stored")
	cmd.Flags().StringVar(&opts.OIDCStorageProviderGCSRegion, "oidc-storage-provider-gcs-region", "", "The region of the GCS bucket where the OIDC discovery document is stored")
	cmd.Flags().StringVar(&opts.IssuerURL, "oidc-issuer-url", "", "The OIDC provider issuer URL")
	cmd.Flags().StringVar(&opts.Region, "region", opts.Region, "Region where cluster infra should be created")
	cmd.Flags().StringVar(&opts.OutputFile, "output-file", opts.OutputFile, "Path to file that will contain output information from infra resources (optional)")
	cmd.Flags().StringSliceVar(&opts.AdditionalLabels, "additional-labels", opts.AdditionalLabels, "Additional labels to set on GCP resources")

	opts.GCPCredentialsOpts.BindFlags(cmd.Flags())

	_ = cmd.MarkFlagRequired("infra-id")
	_ = cmd.MarkFlagRequired("project-id")

	logger := log.Log
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		err := opts.GCPCredentialsOpts.Validate()
		if err != nil {
			return err
		}
		if err := opts.Run(cmd.Context(), logger); err != nil {
			logger.Error(err, "Failed to create IAM resources")
			return err
		}
		logger.Info("Successfully created IAM resources")
		return nil
	}

	return cmd
}

func (o *CreateIAMOptions) Run(ctx context.Context, logger logr.Logger) error {
	results, err := o.CreateIAM(ctx, logger)
	if err != nil {
		return err
	}
	return o.Output(results)
}

func (o *CreateIAMOptions) Output(results *CreateIAMOutput) error {
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
	outputBytes, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to serialize result: %w", err)
	}
	_, err = out.Write(outputBytes)
	if err != nil {
		return fmt.Errorf("failed to write result: %w", err)
	}
	return nil
}

func (o *CreateIAMOptions) CreateIAM(ctx context.Context, logger logr.Logger) (*CreateIAMOutput, error) {
	logger.Info("Creating GCP IAM resources", "infraID", o.InfraID, "project", o.ProjectID)

	var err error
	if err = o.parseAdditionalLabels(); err != nil {
		return nil, err
	}

	clients, err := o.GCPCredentialsOpts.GetClients(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create GCP clients: %w", err)
	}

	// Create IAM manager
	iamManager := NewIAMManager(clients, o.ProjectID, o.Region, logger)

	// Generate issuer URL
	issuerURL := o.generateIssuerURL()

	// Create Workload Identity Pool and OIDC Provider
	logger.Info("Creating Workload Identity Pool and OIDC Provider")
	wifDetails, err := iamManager.CreateWorkloadIdentityPool(ctx, o.InfraID, issuerURL, o.additionalLabels)
	if err != nil {
		return nil, fmt.Errorf("failed to create Workload Identity Pool: %w", err)
	}

	// Create service accounts for all components
	logger.Info("Creating component service accounts")
	serviceAccounts, err := iamManager.CreateComponentServiceAccounts(ctx, o.InfraID, wifDetails, o.additionalLabels)
	if err != nil {
		return nil, fmt.Errorf("failed to create component service accounts: %w", err)
	}

	// Build results
	results := &CreateIAMOutput{
		InfraID:   o.InfraID,
		ProjectID: o.ProjectID,
		Region:    o.Region,
		IssuerURL: issuerURL,
		WorkloadIdentity: GCPWorkloadIdentityOutput{
			PoolID:     wifDetails.PoolID,
			ProviderID: wifDetails.ProviderID,
			PoolName:   wifDetails.PoolName,
			Audience:   wifDetails.Audience,
		},
		ServiceAccounts: GCPServiceAccountsOutput{
			Ingress:         serviceAccounts.Ingress.Email,
			ImageRegistry:   serviceAccounts.ImageRegistry.Email,
			Storage:         serviceAccounts.Storage.Email,
			CloudController: serviceAccounts.CloudController.Email,
			ControlPlane:    serviceAccounts.ControlPlane.Email,
		},
	}

	logger.Info("Successfully created all GCP IAM resources",
		"poolID", wifDetails.PoolID,
		"serviceAccounts", len([]string{
			serviceAccounts.Ingress.Email,
			serviceAccounts.ImageRegistry.Email,
			serviceAccounts.Storage.Email,
			serviceAccounts.CloudController.Email,
			serviceAccounts.ControlPlane.Email,
		}))
	return results, nil
}

func (o *CreateIAMOptions) parseAdditionalLabels() error {
	var err error
	o.additionalLabels, err = gcputil.ParseAdditionalLabels(o.AdditionalLabels)
	return err
}

func (o *CreateIAMOptions) generateIssuerURL() string {
	if o.IssuerURL != "" {
		return o.IssuerURL
	}
	if o.OIDCStorageProviderGCSBucket != "" {
		return fmt.Sprintf("https://storage.googleapis.com/%s/%s", o.OIDCStorageProviderGCSBucket, o.InfraID)
	}
	// Default issuer URL format
	return fmt.Sprintf("https://hypershift-oidc-%s.%s.example.com", o.InfraID, o.Region)
}
