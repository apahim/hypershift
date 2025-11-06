// Package gcp provides Google Cloud Platform support for HyperShift.
//
// This package implements the Platform interface to enable HyperShift
// to create and manage OpenShift control planes on Google Cloud Platform.
//
// The package currently provides enough functionality to allow HostedCluster
// resources with platform.type: GCP to be accepted and reconciled by the
// HyperShift operator without errors. All methods return nil or no-op values
// to indicate that no platform-specific operations should be performed at this time.
//
// For more information about HyperShift platform implementations, see:
// https://github.com/openshift/hypershift/blob/main/docs/content/how-to/platforms.md
package gcp

import (
	"context"
	"fmt"
	"os"
	"strings"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/support/images"
	"github.com/openshift/hypershift/support/upsert"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	capigcp "sigs.k8s.io/cluster-api-provider-gcp/api/v1beta1"
	capiv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/blang/semver"
)

// GCP implements the Platform interface for Google Cloud Platform.
//
// This implementation enables HostedCluster reconciliation for GCP platform
// and provides the necessary CAPI provider deployment configuration for
// CAPG (Cluster API Provider GCP) integration with HyperShift.
type GCP struct {
	utilitiesImage    string
	capiProviderImage string
	payloadVersion    *semver.Version
}

// New creates a new GCP platform instance.
//
// This function returns a new instance of the GCP platform implementation
// that can be used with the HyperShift platform factory. The returned
// platform provides CAPI provider deployment configuration and integration
// with CAPG (Cluster API Provider GCP).
//
// Parameters:
//   - utilitiesImage: The HyperShift utilities image for token minter sidecar
//   - capiProviderImage: The CAPG provider image from release payload
//   - payloadVersion: The OpenShift payload version for version-aware behavior
//
// Returns:
//   - *GCP: A new GCP platform instance ready for use with HyperShift controllers
func New(utilitiesImage string, capiProviderImage string, payloadVersion *semver.Version) *GCP {
	return &GCP{
		utilitiesImage:    utilitiesImage,
		capiProviderImage: capiProviderImage,
		payloadVersion:    payloadVersion,
	}
}

// ReconcileCAPIInfraCR creates and reconciles the GCPCluster infrastructure resource.
//
// This method creates a GCPCluster resource in the management cluster namespace that
// represents the GCP infrastructure configuration for CAPG (Cluster API Provider GCP).
// The GCPCluster serves as the infrastructure template that CAPG uses to provision
// worker nodes via NodePool resources.
func (p GCP) ReconcileCAPIInfraCR(ctx context.Context, c client.Client, createOrUpdate upsert.CreateOrUpdateFN,
	hcluster *hyperv1.HostedCluster,
	controlPlaneNamespace string, apiEndpoint hyperv1.APIEndpoint) (client.Object, error) {

	gcpCluster := &capigcp.GCPCluster{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: controlPlaneNamespace,
			Name:      hcluster.Name,
		},
	}

	_, err := createOrUpdate(ctx, c, gcpCluster, func() error {
		return reconcileGCPCluster(gcpCluster, hcluster, apiEndpoint)
	})
	if err != nil {
		return nil, err
	}
	return gcpCluster, nil
}

// reconcileGCPCluster configures the GCPCluster resource for CAPG integration.
//
// This function sets up the GCP infrastructure configuration including:
// - Project and region from HostedCluster spec
// - Network configuration for VPC and Private Service Connect
// - Control plane endpoint for API server access
// - CAPI annotations for external management
// - Status fields required for CAPI integration
func reconcileGCPCluster(gcpCluster *capigcp.GCPCluster, hcluster *hyperv1.HostedCluster, apiEndpoint hyperv1.APIEndpoint) error {
	// Set CAPI annotation to mark this resource as externally managed by HyperShift
	// This prevents CAPG from trying to manage the lifecycle of this infrastructure
	if gcpCluster.Annotations == nil {
		gcpCluster.Annotations = map[string]string{}
	}
	gcpCluster.Annotations[capiv1.ManagedByAnnotation] = "external"

	// Configure required GCP project and region from HostedCluster spec
	if hcluster.Spec.Platform.GCP == nil {
		return fmt.Errorf("GCP platform configuration is required")
	}

	gcpCluster.Spec.Project = hcluster.Spec.Platform.GCP.Project
	gcpCluster.Spec.Region = hcluster.Spec.Platform.GCP.Region

	// Configure VPC network from GCP network configuration
	// This sets up the VPC that CAPG will use for creating worker nodes
	if hcluster.Spec.Platform.GCP.NetworkConfig.Network.Name != "" {
		gcpCluster.Spec.Network.Name = ptr.To(hcluster.Spec.Platform.GCP.NetworkConfig.Network.Name)

		// Configure Auto Create Subnetworks mode
		// Set to false for custom VPC networks (Private Service Connect requirement)
		gcpCluster.Spec.Network.AutoCreateSubnetworks = ptr.To(false)
	}

	// Set control plane endpoint for API server access
	// This allows CAPG to configure worker nodes to communicate with the hosted control plane
	gcpCluster.Spec.ControlPlaneEndpoint = capiv1.APIEndpoint{
		Host: apiEndpoint.Host,
		Port: apiEndpoint.Port,
	}

	// Configure additional labels if present in HostedCluster
	// This allows users to tag GCP resources created by CAPG
	gcpCluster.Spec.AdditionalLabels = map[string]string{
		"hypershift.openshift.io/cluster":   hcluster.Name,
		"hypershift.openshift.io/namespace": hcluster.Namespace,
	}

	// Set status to ready for CAPI integration
	// This indicates to CAPG that the infrastructure is ready for use
	gcpCluster.Status.Ready = true

	return nil
}

// CAPIProviderDeploymentSpec returns the deployment specification for CAPG.
//
// This method creates a deployment specification for the Cluster API Provider GCP
// (CAPG) following the HyperShift pattern with token minter sidecar for guest
// cluster access and proper GCP credential management.
func (p GCP) CAPIProviderDeploymentSpec(hcluster *hyperv1.HostedCluster, hcp *hyperv1.HostedControlPlane) (*appsv1.DeploymentSpec, error) {
	// Image resolution (following AWS pattern)
	providerImage := p.capiProviderImage
	if envImage := os.Getenv(images.GCPCAPIProviderEnvVar); len(envImage) > 0 {
		providerImage = envImage
	}
	// Allow annotation override for custom CAPG provider images
	if override, ok := hcluster.Annotations[hyperv1.ClusterAPIProviderGCPImage]; ok {
		providerImage = override
	}

	// Feature gates - GKE support disabled due to missing GKE managed CRDs in CAPG v1.10.0
	// When GKE managed clusters are needed, ensure proper CRDs are available first
	featureGates := []string{}
	defaultMode := int32(0640)
	deploymentSpec := &appsv1.DeploymentSpec{
		Replicas: ptr.To[int32](1),
		Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				TerminationGracePeriodSeconds: ptr.To[int64](10),
				Tolerations: []corev1.Toleration{
					{
						Key:    "node-role.kubernetes.io/master",
						Effect: corev1.TaintEffectNoSchedule,
					},
				},
				Volumes: []corev1.Volume{
					{
						Name: "capi-webhooks-tls",
						VolumeSource: corev1.VolumeSource{
							Secret: &corev1.SecretVolumeSource{
								DefaultMode: &defaultMode,
								SecretName:  "capi-webhooks-tls",
							},
						},
					},
					{
						Name: "svc-kubeconfig",
						VolumeSource: corev1.VolumeSource{
							Secret: &corev1.SecretVolumeSource{
								DefaultMode: &defaultMode,
								SecretName:  "service-network-admin-kubeconfig",
							},
						},
					},
					{
						Name: "token",
						VolumeSource: corev1.VolumeSource{
							EmptyDir: &corev1.EmptyDirVolumeSource{
								Medium: corev1.StorageMediumMemory,
							},
						},
					},
				},
				Containers: []corev1.Container{
					{
						Name:            "manager",
						Image:           providerImage,
						ImagePullPolicy: corev1.PullIfNotPresent,
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceMemory: resource.MustParse("100Mi"),
								corev1.ResourceCPU:    resource.MustParse("10m"),
							},
						},
						VolumeMounts: []corev1.VolumeMount{
							{
								Name:      "capi-webhooks-tls",
								ReadOnly:  true,
								MountPath: "/tmp/k8s-webhook-server/serving-certs",
							},
							{
								Name:      "token",
								MountPath: "/var/run/secrets/openshift/serviceaccount",
							},
						},
						Env: []corev1.EnvVar{
							{
								Name: "MY_NAMESPACE",
								ValueFrom: &corev1.EnvVarSource{
									FieldRef: &corev1.ObjectFieldSelector{
										FieldPath: "metadata.namespace",
									},
								},
							},
							{
								Name:  "GOOGLE_APPLICATION_CREDENTIALS",
								Value: "/var/run/secrets/openshift/serviceaccount/token",
							},
							{
								Name:  "USE_WORKLOAD_IDENTITY",
								Value: "true",
							},
							{
								Name:  "GOOGLE_CLOUD_PROJECT",
								Value: hcluster.Spec.Platform.GCP.Project,
							},
						},
						Args: []string{
							"--namespace", "$(MY_NAMESPACE)",
							"--v=4",
							"--leader-elect=true",
							fmt.Sprintf("--feature-gates=%s", strings.Join(featureGates, ",")),
						},
						Ports: []corev1.ContainerPort{
							{
								Name:          "healthz",
								ContainerPort: 9440,
								Protocol:      corev1.ProtocolTCP,
							},
						},
						LivenessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path: "/healthz",
									Port: intstr.FromString("healthz"),
								},
							},
							// More conservative timeouts for GKE environments with many APIs
							InitialDelaySeconds: 60,
							TimeoutSeconds:      10,
							PeriodSeconds:       30,
							FailureThreshold:    3,
						},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path: "/readyz",
									Port: intstr.FromString("healthz"),
								},
							},
							// Conservative settings for API discovery in GKE clusters
							InitialDelaySeconds: 30,
							TimeoutSeconds:      5,
							PeriodSeconds:       10,
							FailureThreshold:    5,
						},
					},
					{
						Name:            "token-minter",
						Image:           p.utilitiesImage,
						ImagePullPolicy: corev1.PullIfNotPresent,
						VolumeMounts: []corev1.VolumeMount{
							{
								Name:      "token",
								MountPath: "/var/run/secrets/openshift/serviceaccount",
							},
							{
								Name:      "svc-kubeconfig",
								MountPath: "/etc/kubernetes",
							},
						},
						Command: []string{"/usr/bin/control-plane-operator", "token-minter"},
						Args: []string{
							"--service-account-namespace=kube-system",
							"--service-account-name=capi-provider",
							"--token-audience=openshift",
							"--token-file=/var/run/secrets/openshift/serviceaccount/token",
							"--kubeconfig=/etc/kubernetes/kubeconfig",
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("10m"),
								corev1.ResourceMemory: resource.MustParse("30Mi"),
							},
						},
					},
				},
			},
		},
	}
	return deploymentSpec, nil
}

// ReconcileCredentials is a no-op for GCP platform.
//
// GCP uses Workload Identity authentication via the token minter pattern:
// 1. Token minter creates guest cluster ServiceAccount tokens
// 2. CAPG uses these tokens with Workload Identity to access GCP APIs
// 3. No static credential secrets needed in management cluster
func (p GCP) ReconcileCredentials(ctx context.Context, c client.Client, createOrUpdate upsert.CreateOrUpdateFN,
	hcluster *hyperv1.HostedCluster,
	controlPlaneNamespace string) error {

	// No credential secrets needed - using Workload Identity via token minter
	return nil
}

// ReconcileSecretEncryption is a no-op for GCP platform.
// TODO: Implement GCP Cloud KMS integration for etcd secret encryption.
// This should configure Cloud KMS keys for etcd encryption at rest, following
// the AWS KMS pattern but using GCP-specific KMS APIs and service accounts.
func (p GCP) ReconcileSecretEncryption(ctx context.Context, c client.Client, createOrUpdate upsert.CreateOrUpdateFN,
	hcluster *hyperv1.HostedCluster,
	controlPlaneNamespace string) error {

	// GCP Cloud KMS secret encryption not yet implemented
	return nil
}

// CAPIProviderPolicyRules returns nil for GCP platform.
//
// GCP follows the cloud provider pattern (like AWS/Azure) where cloud resources
// are managed via GCP APIs using GCP credentials, not through platform-specific
// Kubernetes CRDs. CAPG only needs standard Cluster API resources (Clusters,
// Machines, etc.) in the guest cluster, which are covered by standard CAPI
// controller RBAC configured elsewhere in HyperShift.
//
// Returns nil to indicate no additional RBAC policy rules are required beyond
// the standard Cluster API permissions.
func (p GCP) CAPIProviderPolicyRules() []rbacv1.PolicyRule {
	return nil
}

// DeleteCredentials is a no-op for GCP platform.
// TODO: Implement cleanup of GCP Workload Identity bindings and service accounts.
// This should clean up any IAM bindings between guest cluster ServiceAccounts and
// GCP service accounts that were created for Workload Identity authentication.
func (p GCP) DeleteCredentials(ctx context.Context, c client.Client, hcluster *hyperv1.HostedCluster, controlPlaneNamespace string) error {
	// GCP Workload Identity credential cleanup not yet implemented
	return nil
}
