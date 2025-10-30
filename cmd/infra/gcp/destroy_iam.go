package gcp

import (
	"context"
	"fmt"

	gcputil "github.com/openshift/hypershift/cmd/infra/gcp/util"
	"github.com/openshift/hypershift/cmd/log"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
)

type DestroyIAMOptions struct {
	ProjectID          string
	InfraID            string
	GCPCredentialsOpts gcputil.GCPCredentialsOptions
}

func NewDestroyIAMCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "gcp",
		Short:        "Destroys GCP IAM and WIF resources for a cluster",
		SilenceUsage: true,
	}

	opts := DestroyIAMOptions{}

	cmd.Flags().StringVar(&opts.InfraID, "infra-id", opts.InfraID, "Infrastructure ID of the cluster to destroy (required)")
	cmd.Flags().StringVar(&opts.ProjectID, "project-id", opts.ProjectID, "GCP Project ID where resources exist (required)")

	opts.GCPCredentialsOpts.BindFlags(cmd.Flags())

	_ = cmd.MarkFlagRequired("infra-id")
	_ = cmd.MarkFlagRequired("project-id")

	logger := log.Log
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if err := opts.Run(cmd.Context(), logger); err != nil {
			logger.Error(err, "Failed to destroy IAM resources")
			return err
		}
		logger.Info("Successfully destroyed IAM resources")
		return nil
	}

	return cmd
}

func (o *DestroyIAMOptions) Run(ctx context.Context, logger logr.Logger) error {
	return o.DestroyIAM(ctx, logger)
}

func (o *DestroyIAMOptions) DestroyIAM(ctx context.Context, logger logr.Logger) error {
	logger.Info("Destroying GCP IAM resources", "infraID", o.InfraID, "project", o.ProjectID)

	clients, err := o.GCPCredentialsOpts.GetClients(ctx)
	if err != nil {
		return fmt.Errorf("failed to create GCP clients: %w", err)
	}

	// Create IAM manager
	iamManager := NewIAMManager(clients, o.ProjectID, "", logger)

	// Delete component service accounts
	logger.Info("Deleting component service accounts")
	err = iamManager.DeleteComponentServiceAccounts(ctx, o.InfraID)
	if err != nil {
		logger.Error(err, "Failed to delete some service accounts, continuing")
	}

	// Delete Workload Identity Pool (this also deletes providers)
	logger.Info("Deleting Workload Identity Pool")
	err = iamManager.DeleteWorkloadIdentityPool(ctx, o.InfraID)
	if err != nil {
		logger.Error(err, "Failed to delete Workload Identity Pool")
		return fmt.Errorf("failed to delete Workload Identity Pool: %w", err)
	}

	logger.Info("Successfully destroyed all GCP IAM resources")
	return nil
}
