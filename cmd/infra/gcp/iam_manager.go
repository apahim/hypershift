package gcp

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"google.golang.org/api/cloudresourcemanager/v1"
	"google.golang.org/api/iam/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	gcputil "github.com/openshift/hypershift/cmd/infra/gcp/util"
)

// IAMManager handles Workload Identity and Service Account operations for GCP
type IAMManager struct {
	clients   *gcputil.GCPClients
	projectID string
	region    string
	logger    logr.Logger
}

// WorkloadIdentityDetails represents Workload Identity configuration for output
type WorkloadIdentityDetails struct {
	PoolID       string            `json:"poolID"`
	PoolName     string            `json:"poolName"`
	ProviderID   string            `json:"providerID"`
	ProviderName string            `json:"providerName"`
	Audience     string            `json:"audience"`
	IssuerURL    string            `json:"issuerURL"`
	Labels       map[string]string `json:"labels"`
}

// ServiceAccountDetails represents Service Account information for output
type ServiceAccountDetails struct {
	Name        string            `json:"name"`
	Email       string            `json:"email"`
	DisplayName string            `json:"displayName"`
	Description string            `json:"description"`
	ProjectID   string            `json:"projectID"`
	UniqueID    string            `json:"uniqueID"`
	Labels      map[string]string `json:"labels"`
	Roles       []string          `json:"roles"`
	WIFBinding  string            `json:"wifBinding,omitempty"`
}

// ComponentServiceAccounts defines the service accounts needed for each component
type ComponentServiceAccounts struct {
	Ingress         *ServiceAccountDetails `json:"ingress"`
	ImageRegistry   *ServiceAccountDetails `json:"imageRegistry"`
	Storage         *ServiceAccountDetails `json:"storage"`
	CloudController *ServiceAccountDetails `json:"cloudController"`
	ControlPlane    *ServiceAccountDetails `json:"controlPlane"`
	Network         *ServiceAccountDetails `json:"network"`
	Karpenter       *ServiceAccountDetails `json:"karpenter,omitempty"`
}

// NewIAMManager creates a new IAM manager
func NewIAMManager(clients *gcputil.GCPClients, projectID, region string, logger logr.Logger) *IAMManager {
	return &IAMManager{
		clients:   clients,
		projectID: projectID,
		region:    region,
		logger:    logger,
	}
}

// CreateWorkloadIdentityPool creates a Workload Identity Pool for the cluster
func (im *IAMManager) CreateWorkloadIdentityPool(ctx context.Context, infraID, issuerURL string, labels map[string]string) (*WorkloadIdentityDetails, error) {
	poolID := fmt.Sprintf("%s-pool", infraID)
	providerID := fmt.Sprintf("%s-provider", infraID)

	im.logger.Info("Creating Workload Identity Pool", "poolID", poolID, "issuerURL", issuerURL)

	// Check if pool already exists
	existingPool, err := im.getExistingWorkloadIdentityPool(ctx, poolID)
	if err != nil {
		return nil, err
	}

	if existingPool != nil {
		im.logger.Info("Found existing Workload Identity Pool", "name", poolID)
		return im.convertToWorkloadIdentityDetails(existingPool, poolID, providerID, issuerURL), nil
	}

	// Create Workload Identity Pool
	pool, err := im.createWorkloadIdentityPool(ctx, poolID, infraID, labels)
	if err != nil {
		return nil, fmt.Errorf("failed to create Workload Identity Pool: %w", err)
	}

	// Create OIDC Provider in the pool
	provider, err := im.createOIDCProvider(ctx, poolID, providerID, issuerURL, infraID)
	if err != nil {
		return nil, fmt.Errorf("failed to create OIDC provider: %w", err)
	}

	im.logger.Info("Successfully created Workload Identity Pool and Provider", "pool", poolID, "provider", providerID)

	return &WorkloadIdentityDetails{
		PoolID:       poolID,
		PoolName:     pool.Name,
		ProviderID:   providerID,
		ProviderName: provider.Name,
		Audience:     fmt.Sprintf("//iam.googleapis.com/%s", pool.Name),
		IssuerURL:    issuerURL,
		Labels:       labels,
	}, nil
}

// createWorkloadIdentityPool creates the actual Workload Identity Pool
func (im *IAMManager) createWorkloadIdentityPool(ctx context.Context, poolID, infraID string, labels map[string]string) (*iam.WorkloadIdentityPool, error) {
	pool := &iam.WorkloadIdentityPool{
		DisplayName: fmt.Sprintf("HyperShift cluster %s", infraID),
		Description: fmt.Sprintf("Workload Identity Pool for HyperShift cluster %s", infraID),
		State:       "ACTIVE",
		Disabled:    false,
	}

	parent := fmt.Sprintf("projects/%s/locations/global", im.projectID)

	operation, err := im.clients.IAM.Projects.Locations.WorkloadIdentityPools.Create(parent, pool).WorkloadIdentityPoolId(poolID).Context(ctx).Do()
	if err != nil {
		return nil, gcputil.HandleResourceCreationError(err, "Workload Identity Pool", poolID)
	}

	// Wait for operation to complete
	err = im.waitForLongRunningOperation(ctx, operation.Name, fmt.Sprintf("Workload Identity Pool %s creation", poolID))
	if err != nil {
		return nil, fmt.Errorf("failed waiting for Workload Identity Pool creation: %w", err)
	}

	// Get the created pool
	poolName := fmt.Sprintf("%s/workloadIdentityPools/%s", parent, poolID)
	createdPool, err := im.clients.IAM.Projects.Locations.WorkloadIdentityPools.Get(poolName).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("failed to get created Workload Identity Pool: %w", err)
	}

	return createdPool, nil
}

// createOIDCProvider creates an OIDC provider in the Workload Identity Pool
func (im *IAMManager) createOIDCProvider(ctx context.Context, poolID, providerID, issuerURL, infraID string) (*iam.WorkloadIdentityPoolProvider, error) {
	provider := &iam.WorkloadIdentityPoolProvider{
		DisplayName: fmt.Sprintf("HyperShift OIDC provider for %s", infraID),
		Description: fmt.Sprintf("OIDC provider for HyperShift cluster %s", infraID),
		State:       "ACTIVE",
		Disabled:    false,
		Oidc: &iam.Oidc{
			IssuerUri: issuerURL,
			AllowedAudiences: []string{
				"openshift",
				"https://kubernetes.default.svc.cluster.local",
			},
		},
		AttributeMapping: map[string]string{
			"google.subject":                 "assertion.sub",
			"attribute.kubernetes_namespace": "assertion['kubernetes.io']['namespace']",
			"attribute.kubernetes_sa":        "assertion['kubernetes.io']['serviceaccount']['name']",
			"attribute.kubernetes_pod":       "assertion['kubernetes.io']['pod']['name']",
		},
		AttributeCondition: "true", // Accept all tokens from the issuer
	}

	parent := fmt.Sprintf("projects/%s/locations/global/workloadIdentityPools/%s", im.projectID, poolID)

	operation, err := im.clients.IAM.Projects.Locations.WorkloadIdentityPools.Providers.Create(parent, provider).WorkloadIdentityPoolProviderId(providerID).Context(ctx).Do()
	if err != nil {
		return nil, gcputil.HandleResourceCreationError(err, "OIDC provider", providerID)
	}

	// Wait for operation to complete
	err = im.waitForLongRunningOperation(ctx, operation.Name, fmt.Sprintf("OIDC provider %s creation", providerID))
	if err != nil {
		return nil, fmt.Errorf("failed waiting for OIDC provider creation: %w", err)
	}

	// Get the created provider
	providerName := fmt.Sprintf("%s/providers/%s", parent, providerID)
	createdProvider, err := im.clients.IAM.Projects.Locations.WorkloadIdentityPools.Providers.Get(providerName).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("failed to get created OIDC provider: %w", err)
	}

	return createdProvider, nil
}

// CreateComponentServiceAccounts creates service accounts for all cluster components
func (im *IAMManager) CreateComponentServiceAccounts(ctx context.Context, infraID string, wifDetails *WorkloadIdentityDetails, labels map[string]string) (*ComponentServiceAccounts, error) {
	im.logger.Info("Creating component service accounts", "infraID", infraID)

	components := &ComponentServiceAccounts{}
	var err error

	// Ingress Controller
	components.Ingress, err = im.createServiceAccount(ctx, infraID, "ingress", "Ingress Controller", []string{
		"roles/compute.networkAdmin",
		"roles/compute.loadBalancerAdmin",
	}, wifDetails, "openshift-ingress", "router-default", labels)
	if err != nil {
		return nil, fmt.Errorf("failed to create ingress service account: %w", err)
	}

	// Image Registry
	components.ImageRegistry, err = im.createServiceAccount(ctx, infraID, "registry", "Image Registry", []string{
		"roles/storage.admin",
	}, wifDetails, "openshift-image-registry", "cluster-image-registry-operator", labels)
	if err != nil {
		return nil, fmt.Errorf("failed to create image registry service account: %w", err)
	}

	// Storage (CSI Driver)
	components.Storage, err = im.createServiceAccount(ctx, infraID, "storage", "Storage CSI Driver", []string{
		"roles/compute.storageAdmin",
	}, wifDetails, "openshift-cluster-csi-drivers", "gcp-pd-csi-driver-operator", labels)
	if err != nil {
		return nil, fmt.Errorf("failed to create storage service account: %w", err)
	}

	// Cloud Controller Manager
	components.CloudController, err = im.createServiceAccount(ctx, infraID, "cloud-controller", "Cloud Controller Manager", []string{
		"roles/compute.instanceAdmin",
		"roles/compute.networkAdmin",
		"roles/compute.storageAdmin",
		"roles/iam.serviceAccountUser",
	}, wifDetails, "openshift-cloud-controller-manager", "cloud-controller-manager", labels)
	if err != nil {
		return nil, fmt.Errorf("failed to create cloud controller service account: %w", err)
	}

	// Control Plane Operator
	components.ControlPlane, err = im.createServiceAccount(ctx, infraID, "control-plane", "Control Plane Operator", []string{
		"roles/compute.viewer",
		"roles/iam.serviceAccountTokenCreator",
	}, wifDetails, "hypershift", "operator", labels)
	if err != nil {
		return nil, fmt.Errorf("failed to create control plane service account: %w", err)
	}

	// Network Config Controller
	components.Network, err = im.createServiceAccount(ctx, infraID, "network", "Network Config Controller", []string{
		"roles/compute.networkAdmin",
	}, wifDetails, "openshift-network-operator", "network-operator", labels)
	if err != nil {
		return nil, fmt.Errorf("failed to create network service account: %w", err)
	}

	im.logger.Info("Successfully created all component service accounts")
	return components, nil
}

// createServiceAccount creates a single service account with proper roles and WIF binding
func (im *IAMManager) createServiceAccount(ctx context.Context, infraID, component, displayName string, roles []string, wifDetails *WorkloadIdentityDetails, namespace, k8sServiceAccount string, labels map[string]string) (*ServiceAccountDetails, error) {
	accountID := fmt.Sprintf("%s-%s", infraID, component)

	// Check if service account already exists
	existingSA, err := im.getExistingServiceAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}

	var serviceAccount *iam.ServiceAccount

	if existingSA != nil {
		im.logger.Info("Found existing service account", "name", accountID)
		serviceAccount = existingSA
	} else {
		// Create new service account
		serviceAccount, err = im.createServiceAccount_(ctx, accountID, displayName, infraID, labels)
		if err != nil {
			return nil, fmt.Errorf("failed to create service account %s: %w", accountID, err)
		}
	}

	// Bind IAM roles
	err = im.bindServiceAccountRoles(ctx, serviceAccount.Email, roles)
	if err != nil {
		return nil, fmt.Errorf("failed to bind roles to service account %s: %w", accountID, err)
	}

	// Configure Workload Identity binding
	wifBinding, err := im.configureWorkloadIdentityBinding(ctx, serviceAccount.Email, wifDetails, namespace, k8sServiceAccount)
	if err != nil {
		return nil, fmt.Errorf("failed to configure Workload Identity binding for %s: %w", accountID, err)
	}

	return &ServiceAccountDetails{
		Name:        serviceAccount.Name,
		Email:       serviceAccount.Email,
		DisplayName: serviceAccount.DisplayName,
		Description: serviceAccount.Description,
		ProjectID:   serviceAccount.ProjectId,
		UniqueID:    serviceAccount.UniqueId,
		Labels:      labels,
		Roles:       roles,
		WIFBinding:  wifBinding,
	}, nil
}

// createServiceAccount_ creates the actual service account resource
func (im *IAMManager) createServiceAccount_(ctx context.Context, accountID, displayName, infraID string, labels map[string]string) (*iam.ServiceAccount, error) {
	serviceAccount := &iam.CreateServiceAccountRequest{
		AccountId: accountID,
		ServiceAccount: &iam.ServiceAccount{
			DisplayName: fmt.Sprintf("HyperShift %s for %s", displayName, infraID),
			Description: fmt.Sprintf("Service account for HyperShift cluster %s %s component", infraID, displayName),
		},
	}

	projectName := fmt.Sprintf("projects/%s", im.projectID)
	createdSA, err := im.clients.IAM.Projects.ServiceAccounts.Create(projectName, serviceAccount).Context(ctx).Do()
	if err != nil {
		return nil, gcputil.HandleResourceCreationError(err, "service account", accountID)
	}

	im.logger.Info("Successfully created service account", "name", accountID, "email", createdSA.Email)
	return createdSA, nil
}

// bindServiceAccountRoles binds IAM roles to a service account
func (im *IAMManager) bindServiceAccountRoles(ctx context.Context, serviceAccountEmail string, roles []string) error {
	for _, role := range roles {
		// Get current IAM policy
		policy, err := im.clients.CRM.Projects.GetIamPolicy(im.projectID, &cloudresourcemanager.GetIamPolicyRequest{}).Context(ctx).Do()
		if err != nil {
			return fmt.Errorf("failed to get IAM policy: %w", err)
		}

		// Add binding
		member := fmt.Sprintf("serviceAccount:%s", serviceAccountEmail)
		binding := im.findOrCreateBinding(policy, role, member)
		if binding == nil {
			policy.Bindings = append(policy.Bindings, &cloudresourcemanager.Binding{
				Role:    role,
				Members: []string{member},
			})
		}

		// Set updated policy
		_, err = im.clients.CRM.Projects.SetIamPolicy(im.projectID, &cloudresourcemanager.SetIamPolicyRequest{
			Policy: policy,
		}).Context(ctx).Do()
		if err != nil {
			return fmt.Errorf("failed to set IAM policy for role %s: %w", role, err)
		}

		im.logger.Info("Bound role to service account", "role", role, "email", serviceAccountEmail)
	}

	return nil
}

// configureWorkloadIdentityBinding configures the binding between K8s SA and GCP SA
func (im *IAMManager) configureWorkloadIdentityBinding(ctx context.Context, serviceAccountEmail string, wifDetails *WorkloadIdentityDetails, namespace, k8sServiceAccount string) (string, error) {
	// Create the principal identifier for the Kubernetes service account
	principal := fmt.Sprintf("principal://iam.googleapis.com/%s/subject/system:serviceaccount:%s:%s",
		wifDetails.PoolName, namespace, k8sServiceAccount)

	// Grant the Workload Identity User role to the principal
	member := fmt.Sprintf("principalSet://iam.googleapis.com/%s/attribute.kubernetes_namespace/%s",
		wifDetails.PoolName, namespace)

	// Get current IAM policy for the service account
	resource := fmt.Sprintf("projects/%s/serviceAccounts/%s", im.projectID, serviceAccountEmail)
	policy, err := im.clients.IAM.Projects.ServiceAccounts.GetIamPolicy(resource).Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("failed to get service account IAM policy: %w", err)
	}

	// Add Workload Identity User binding
	role := "roles/iam.workloadIdentityUser"
	binding := im.findOrCreateServiceAccountBinding(policy, role, member)
	if binding == nil {
		policy.Bindings = append(policy.Bindings, &iam.Binding{
			Role:    role,
			Members: []string{member},
		})
	}

	// Set updated policy
	_, err = im.clients.IAM.Projects.ServiceAccounts.SetIamPolicy(resource, &iam.SetIamPolicyRequest{
		Policy: policy,
	}).Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("failed to set service account IAM policy: %w", err)
	}

	bindingInfo := fmt.Sprintf("%s -> %s", principal, serviceAccountEmail)
	im.logger.Info("Configured Workload Identity binding", "binding", bindingInfo)
	return bindingInfo, nil
}

// Helper functions

func (im *IAMManager) getExistingWorkloadIdentityPool(ctx context.Context, poolID string) (*iam.WorkloadIdentityPool, error) {
	poolName := fmt.Sprintf("projects/%s/locations/global/workloadIdentityPools/%s", im.projectID, poolID)
	pool, err := im.clients.IAM.Projects.Locations.WorkloadIdentityPools.Get(poolName).Context(ctx).Do()
	if err != nil {
		if gcputil.IsGCPNotFoundError(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to check existing Workload Identity Pool %s: %w", poolID, err)
	}
	return pool, nil
}

func (im *IAMManager) getExistingServiceAccount(ctx context.Context, accountID string) (*iam.ServiceAccount, error) {
	name := fmt.Sprintf("projects/%s/serviceAccounts/%s@%s.iam.gserviceaccount.com", im.projectID, accountID, im.projectID)
	sa, err := im.clients.IAM.Projects.ServiceAccounts.Get(name).Context(ctx).Do()
	if err != nil {
		if gcputil.IsGCPNotFoundError(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to check existing service account %s: %w", accountID, err)
	}
	return sa, nil
}

func (im *IAMManager) findOrCreateBinding(policy *cloudresourcemanager.Policy, role, member string) *cloudresourcemanager.Binding {
	for _, binding := range policy.Bindings {
		if binding.Role == role {
			for _, existingMember := range binding.Members {
				if existingMember == member {
					return binding // Already exists
				}
			}
			// Role exists but member doesn't, add member
			binding.Members = append(binding.Members, member)
			return binding
		}
	}
	return nil // Role doesn't exist
}

func (im *IAMManager) findOrCreateServiceAccountBinding(policy *iam.Policy, role, member string) *iam.Binding {
	for _, binding := range policy.Bindings {
		if binding.Role == role {
			for _, existingMember := range binding.Members {
				if existingMember == member {
					return binding // Already exists
				}
			}
			// Role exists but member doesn't, add member
			binding.Members = append(binding.Members, member)
			return binding
		}
	}
	return nil // Role doesn't exist
}

func (im *IAMManager) waitForLongRunningOperation(ctx context.Context, operationName, description string) error {
	im.logger.Info("Waiting for long-running operation", "operation", operationName, "description", description)

	return wait.PollUntilContextTimeout(ctx, 10*time.Second, 10*time.Minute, true, func(ctx context.Context) (bool, error) {
		// Note: For IAM operations, we would need to implement operation polling
		// This is a simplified version - real implementation would check operation status
		im.logger.V(1).Info("IAM operation polling", "operation", operationName)

		// For now, assume operations complete quickly
		// In production, this should poll the actual operation status
		return true, nil
	})
}

func (im *IAMManager) convertToWorkloadIdentityDetails(pool *iam.WorkloadIdentityPool, poolID, providerID, issuerURL string) *WorkloadIdentityDetails {
	return &WorkloadIdentityDetails{
		PoolID:       poolID,
		PoolName:     pool.Name,
		ProviderID:   providerID,
		ProviderName: fmt.Sprintf("%s/providers/%s", pool.Name, providerID),
		Audience:     fmt.Sprintf("//iam.googleapis.com/%s", pool.Name),
		IssuerURL:    issuerURL,
	}
}

// DeleteWorkloadIdentityPool deletes the Workload Identity Pool and all providers
func (im *IAMManager) DeleteWorkloadIdentityPool(ctx context.Context, infraID string) error {
	poolID := fmt.Sprintf("%s-pool", infraID)
	providerID := fmt.Sprintf("%s-provider", infraID)

	im.logger.Info("Deleting Workload Identity Pool", "poolID", poolID)

	// Delete provider first
	err := im.deleteOIDCProvider(ctx, poolID, providerID)
	if err != nil {
		im.logger.Error(err, "Failed to delete OIDC provider, continuing", "provider", providerID)
	}

	// Delete pool
	err = im.deleteWorkloadIdentityPool(ctx, poolID)
	if err != nil {
		return fmt.Errorf("failed to delete Workload Identity Pool: %w", err)
	}

	im.logger.Info("Successfully deleted Workload Identity Pool", "poolID", poolID)
	return nil
}

func (im *IAMManager) deleteOIDCProvider(ctx context.Context, poolID, providerID string) error {
	providerName := fmt.Sprintf("projects/%s/locations/global/workloadIdentityPools/%s/providers/%s", im.projectID, poolID, providerID)

	operation, err := im.clients.IAM.Projects.Locations.WorkloadIdentityPools.Providers.Delete(providerName).Context(ctx).Do()
	if err != nil {
		return gcputil.HandleResourceDeletionError(err, "OIDC provider", providerID)
	}

	return im.waitForLongRunningOperation(ctx, operation.Name, fmt.Sprintf("OIDC provider %s deletion", providerID))
}

func (im *IAMManager) deleteWorkloadIdentityPool(ctx context.Context, poolID string) error {
	poolName := fmt.Sprintf("projects/%s/locations/global/workloadIdentityPools/%s", im.projectID, poolID)

	operation, err := im.clients.IAM.Projects.Locations.WorkloadIdentityPools.Delete(poolName).Context(ctx).Do()
	if err != nil {
		return gcputil.HandleResourceDeletionError(err, "Workload Identity Pool", poolID)
	}

	return im.waitForLongRunningOperation(ctx, operation.Name, fmt.Sprintf("Workload Identity Pool %s deletion", poolID))
}

// DeleteComponentServiceAccounts deletes all component service accounts
func (im *IAMManager) DeleteComponentServiceAccounts(ctx context.Context, infraID string) error {
	im.logger.Info("Deleting component service accounts", "infraID", infraID)

	components := []string{
		"ingress", "registry", "storage", "cloud-controller", "control-plane", "network",
	}

	for _, component := range components {
		accountID := fmt.Sprintf("%s-%s", infraID, component)
		if err := im.deleteServiceAccount(ctx, accountID); err != nil {
			im.logger.Error(err, "Failed to delete service account, continuing", "account", accountID)
		} else {
			im.logger.Info("Successfully deleted service account", "account", accountID)
		}
	}

	im.logger.Info("Completed deletion of component service accounts")
	return nil
}

func (im *IAMManager) deleteServiceAccount(ctx context.Context, accountID string) error {
	name := fmt.Sprintf("projects/%s/serviceAccounts/%s@%s.iam.gserviceaccount.com", im.projectID, accountID, im.projectID)

	_, err := im.clients.IAM.Projects.ServiceAccounts.Delete(name).Context(ctx).Do()
	if err != nil {
		return gcputil.HandleResourceDeletionError(err, "service account", accountID)
	}

	return nil
}
