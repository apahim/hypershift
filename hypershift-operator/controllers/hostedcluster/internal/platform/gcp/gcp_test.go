package gcp

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/support/upsert"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"

	capigcp "sigs.k8s.io/cluster-api-provider-gcp/api/v1beta1"
	capiv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// buildTestScheme creates a runtime scheme with both hypershift and CAPG types
func buildTestScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	// Add all the basic Kubernetes types
	_ = metav1.AddMetaToScheme(scheme)
	// Add CAPG types
	_ = capigcp.AddToScheme(scheme)
	_ = capiv1.AddToScheme(scheme)
	// Add HyperShift types
	_ = hyperv1.AddToScheme(scheme)
	return scheme
}

func TestGCPPlatformInterface(t *testing.T) {
	g := NewWithT(t)

	// Test that GCP implements the Platform interface
	platform := New("test-utilities:latest", "test-capg:latest", nil)
	g.Expect(platform).ToNot(BeNil())
}

func TestReconcileCAPIInfraCR(t *testing.T) {
	g := NewWithT(t)

	platform := New("test-utilities:latest", "test-capg:latest", nil)
	fakeClient := fake.NewClientBuilder().WithScheme(buildTestScheme()).Build()

	// Create HostedCluster with full GCP configuration
	hcluster := &hyperv1.HostedCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster",
			Namespace: "test-namespace",
		},
		Spec: hyperv1.HostedClusterSpec{
			Platform: hyperv1.PlatformSpec{
				Type: hyperv1.GCPPlatform,
				GCP: &hyperv1.GCPPlatformSpec{
					Project: "test-project",
					Region:  "us-central1",
					NetworkConfig: hyperv1.GCPNetworkConfig{
						Network: hyperv1.GCPResourceReference{
							Name: "test-vpc",
						},
						PrivateServiceConnectSubnet: hyperv1.GCPResourceReference{
							Name: "test-psc-subnet",
						},
					},
				},
			},
		},
	}

	// Test GCPCluster creation and configuration
	obj, err := platform.ReconcileCAPIInfraCR(
		context.Background(),
		fakeClient,
		upsert.New(false).CreateOrUpdate, // createOrUpdate function
		hcluster,
		"test-control-plane-namespace",
		hyperv1.APIEndpoint{Host: "example.com", Port: 443},
	)

	g.Expect(err).To(BeNil())
	g.Expect(obj).ToNot(BeNil())

	// Verify the returned object is a GCPCluster
	gcpCluster, ok := obj.(*capigcp.GCPCluster)
	g.Expect(ok).To(BeTrue())
	g.Expect(gcpCluster.Name).To(Equal("test-cluster"))
	g.Expect(gcpCluster.Namespace).To(Equal("test-control-plane-namespace"))

	// Verify CAPI annotations
	g.Expect(gcpCluster.Annotations).ToNot(BeNil())
	g.Expect(gcpCluster.Annotations[capiv1.ManagedByAnnotation]).To(Equal("external"))

	// Verify GCP configuration
	g.Expect(gcpCluster.Spec.Project).To(Equal("test-project"))
	g.Expect(gcpCluster.Spec.Region).To(Equal("us-central1"))
	g.Expect(gcpCluster.Spec.Network.Name).ToNot(BeNil())
	g.Expect(*gcpCluster.Spec.Network.Name).To(Equal("test-vpc"))
	g.Expect(gcpCluster.Spec.Network.AutoCreateSubnetworks).To(Equal(ptr.To(false)))

	// Verify control plane endpoint
	g.Expect(gcpCluster.Spec.ControlPlaneEndpoint.Host).To(Equal("example.com"))
	g.Expect(gcpCluster.Spec.ControlPlaneEndpoint.Port).To(Equal(int32(443)))

	// Verify additional labels
	g.Expect(gcpCluster.Spec.AdditionalLabels).ToNot(BeNil())
	g.Expect(gcpCluster.Spec.AdditionalLabels["hypershift.openshift.io/cluster"]).To(Equal("test-cluster"))
	g.Expect(gcpCluster.Spec.AdditionalLabels["hypershift.openshift.io/namespace"]).To(Equal("test-namespace"))

	// Verify status
	g.Expect(gcpCluster.Status.Ready).To(BeTrue())
}

func TestReconcileCAPIInfraCRMissingGCPConfig(t *testing.T) {
	g := NewWithT(t)

	platform := New("test-utilities:latest", "test-capg:latest", nil)
	fakeClient := fake.NewClientBuilder().WithScheme(buildTestScheme()).Build()

	// Create HostedCluster without GCP configuration
	hcluster := &hyperv1.HostedCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster",
			Namespace: "test-namespace",
		},
		Spec: hyperv1.HostedClusterSpec{
			Platform: hyperv1.PlatformSpec{
				Type: hyperv1.GCPPlatform,
				GCP:  nil, // Missing GCP configuration
			},
		},
	}

	// Test error handling for missing GCP configuration
	obj, err := platform.ReconcileCAPIInfraCR(
		context.Background(),
		fakeClient,
		upsert.New(false).CreateOrUpdate,
		hcluster,
		"test-control-plane-namespace",
		hyperv1.APIEndpoint{Host: "example.com", Port: 443},
	)

	g.Expect(err).ToNot(BeNil())
	g.Expect(err.Error()).To(ContainSubstring("GCP platform configuration is required"))
	g.Expect(obj).To(BeNil())
}

func TestReconcileGCPCluster(t *testing.T) {
	g := NewWithT(t)

	// Test direct reconcileGCPCluster function
	gcpCluster := &capigcp.GCPCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster",
			Namespace: "test-control-plane-namespace",
		},
	}

	hcluster := &hyperv1.HostedCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster",
			Namespace: "test-namespace",
		},
		Spec: hyperv1.HostedClusterSpec{
			Platform: hyperv1.PlatformSpec{
				Type: hyperv1.GCPPlatform,
				GCP: &hyperv1.GCPPlatformSpec{
					Project: "my-gcp-project",
					Region:  "europe-west2",
					NetworkConfig: hyperv1.GCPNetworkConfig{
						Network: hyperv1.GCPResourceReference{
							Name: "my-vpc-network",
						},
						PrivateServiceConnectSubnet: hyperv1.GCPResourceReference{
							Name: "my-psc-subnet",
						},
					},
				},
			},
		},
	}

	apiEndpoint := hyperv1.APIEndpoint{
		Host: "api.test-cluster.example.com",
		Port: 443,
	}

	// Call reconcileGCPCluster
	err := reconcileGCPCluster(gcpCluster, hcluster, apiEndpoint)

	g.Expect(err).To(BeNil())

	// Verify all configurations are set correctly
	g.Expect(gcpCluster.Annotations[capiv1.ManagedByAnnotation]).To(Equal("external"))
	g.Expect(gcpCluster.Spec.Project).To(Equal("my-gcp-project"))
	g.Expect(gcpCluster.Spec.Region).To(Equal("europe-west2"))
	g.Expect(gcpCluster.Spec.Network.Name).ToNot(BeNil())
	g.Expect(*gcpCluster.Spec.Network.Name).To(Equal("my-vpc-network"))
	g.Expect(gcpCluster.Spec.Network.AutoCreateSubnetworks).To(Equal(ptr.To(false)))
	g.Expect(gcpCluster.Spec.ControlPlaneEndpoint.Host).To(Equal("api.test-cluster.example.com"))
	g.Expect(gcpCluster.Spec.ControlPlaneEndpoint.Port).To(Equal(int32(443)))
	g.Expect(gcpCluster.Spec.AdditionalLabels["hypershift.openshift.io/cluster"]).To(Equal("test-cluster"))
	g.Expect(gcpCluster.Spec.AdditionalLabels["hypershift.openshift.io/namespace"]).To(Equal("test-namespace"))
	g.Expect(gcpCluster.Status.Ready).To(BeTrue())
}

func TestReconcileGCPClusterMissingGCPConfig(t *testing.T) {
	g := NewWithT(t)

	gcpCluster := &capigcp.GCPCluster{}
	hcluster := &hyperv1.HostedCluster{
		Spec: hyperv1.HostedClusterSpec{
			Platform: hyperv1.PlatformSpec{
				Type: hyperv1.GCPPlatform,
				GCP:  nil, // Missing GCP configuration
			},
		},
	}

	apiEndpoint := hyperv1.APIEndpoint{Host: "example.com", Port: 443}

	// Test error handling for missing GCP configuration
	err := reconcileGCPCluster(gcpCluster, hcluster, apiEndpoint)

	g.Expect(err).ToNot(BeNil())
	g.Expect(err.Error()).To(ContainSubstring("GCP platform configuration is required"))
}

func TestCAPIProviderDeploymentSpec(t *testing.T) {
	g := NewWithT(t)

	platform := New("test-utilities:latest", "test-capg:latest", nil)

	// Test minimal implementation returns nil (no CAPI provider)
	spec, err := platform.CAPIProviderDeploymentSpec(
		&hyperv1.HostedCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-cluster",
				Namespace: "test-namespace",
			},
			Spec: hyperv1.HostedClusterSpec{
				Platform: hyperv1.PlatformSpec{
					Type: hyperv1.GCPPlatform,
					GCP: &hyperv1.GCPPlatformSpec{
						Project: "test-project",
						Region:  "us-central1",
					},
				},
			},
		},
		nil, // HostedControlPlane
	)

	g.Expect(err).To(BeNil())
	g.Expect(spec).ToNot(BeNil()) // Phase 3 returns valid deployment spec
	g.Expect(spec.Replicas).To(Equal(ptr.To[int32](1)))
	g.Expect(spec.Template.Spec.TerminationGracePeriodSeconds).To(Equal(ptr.To[int64](10)))
	g.Expect(len(spec.Template.Spec.Tolerations)).To(Equal(1))
	g.Expect(spec.Template.Spec.Tolerations[0].Key).To(Equal("node-role.kubernetes.io/master"))

	// Test Phase 4: Required volumes are configured (no static credentials)
	g.Expect(len(spec.Template.Spec.Volumes)).To(Equal(3))
	volumeNames := make([]string, len(spec.Template.Spec.Volumes))
	for i, vol := range spec.Template.Spec.Volumes {
		volumeNames[i] = vol.Name
	}
	g.Expect(volumeNames).To(ContainElements("capi-webhooks-tls", "svc-kubeconfig", "token"))

	// Test Phase 5: CAPG manager container is configured
	g.Expect(len(spec.Template.Spec.Containers)).To(Equal(2)) // Manager + token minter containers
	managerContainer := spec.Template.Spec.Containers[0]
	g.Expect(managerContainer.Name).To(Equal("manager"))
	g.Expect(managerContainer.Image).To(Equal("test-capg:latest"))
	g.Expect(len(managerContainer.VolumeMounts)).To(Equal(2)) // No static credentials mount
	g.Expect(len(managerContainer.Env)).To(Equal(4))          // MY_NAMESPACE, GOOGLE_APPLICATION_CREDENTIALS, USE_WORKLOAD_IDENTITY, GOOGLE_CLOUD_PROJECT
	g.Expect(managerContainer.Env[1].Name).To(Equal("GOOGLE_APPLICATION_CREDENTIALS"))
	g.Expect(managerContainer.Env[1].Value).To(Equal("/var/run/secrets/openshift/serviceaccount/token")) // Workload Identity token
	g.Expect(managerContainer.Env[2].Name).To(Equal("USE_WORKLOAD_IDENTITY"))
	g.Expect(managerContainer.Env[3].Name).To(Equal("GOOGLE_CLOUD_PROJECT"))
	g.Expect(managerContainer.LivenessProbe).ToNot(BeNil())
	g.Expect(managerContainer.ReadinessProbe).ToNot(BeNil())

	// Test Phase 6: Token minter sidecar is configured
	tokenMinterContainer := spec.Template.Spec.Containers[1]
	g.Expect(tokenMinterContainer.Name).To(Equal("token-minter"))
	g.Expect(tokenMinterContainer.Image).To(Equal("test-utilities:latest"))
	g.Expect(len(tokenMinterContainer.VolumeMounts)).To(Equal(2))
	g.Expect(tokenMinterContainer.Command[0]).To(Equal("/usr/bin/control-plane-operator"))
	g.Expect(tokenMinterContainer.Command[1]).To(Equal("token-minter"))
	g.Expect(len(tokenMinterContainer.Args)).To(Equal(5))
	g.Expect(tokenMinterContainer.Args[1]).To(Equal("--service-account-name=capi-provider"))
}

// TestGCPPlatformComprehensive validates the complete implementation against plan requirements
func TestGCPPlatformComprehensive(t *testing.T) {
	g := NewWithT(t)

	// Test platform creation with all parameters (Phase 1)
	platform := New("utilities:latest", "capg:v1.0.0", nil)
	g.Expect(platform).ToNot(BeNil())

	// Test CAPIProviderPolicyRules follows cloud provider pattern (Phase 2)
	rules := platform.CAPIProviderPolicyRules()
	g.Expect(rules).To(BeNil()) // Cloud provider pattern like AWS/Azure

	// Test complete deployment spec generation (Phases 3-6)
	spec, err := platform.CAPIProviderDeploymentSpec(
		&hyperv1.HostedCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-cluster",
				Namespace: "test-ns",
			},
			Spec: hyperv1.HostedClusterSpec{
				Platform: hyperv1.PlatformSpec{
					Type: hyperv1.GCPPlatform,
					GCP: &hyperv1.GCPPlatformSpec{
						Project: "test-project",
						Region:  "us-central1",
					},
				},
			},
		},
		nil,
	)

	g.Expect(err).To(BeNil())
	g.Expect(spec).ToNot(BeNil())

	// Validate complete deployment specification
	g.Expect(spec.Replicas).To(Equal(ptr.To[int32](1)))
	g.Expect(len(spec.Template.Spec.Volumes)).To(Equal(3))     // All required volumes (no static credentials)
	g.Expect(len(spec.Template.Spec.Containers)).To(Equal(2))  // Manager + token minter
	g.Expect(len(spec.Template.Spec.Tolerations)).To(Equal(1)) // Control plane toleration

	// Validate CAPG manager container
	manager := spec.Template.Spec.Containers[0]
	g.Expect(manager.Name).To(Equal("manager"))
	g.Expect(manager.Image).To(Equal("capg:v1.0.0"))
	g.Expect(len(manager.VolumeMounts)).To(Equal(2))                                            // No static credentials mount
	g.Expect(manager.Env[1].Value).To(Equal("/var/run/secrets/openshift/serviceaccount/token")) // Workload Identity token

	// Validate token minter sidecar
	tokenMinter := spec.Template.Spec.Containers[1]
	g.Expect(tokenMinter.Name).To(Equal("token-minter"))
	g.Expect(tokenMinter.Image).To(Equal("utilities:latest"))
	g.Expect(tokenMinter.Args[1]).To(Equal("--service-account-name=capi-provider"))
}

func TestReconcileCredentials(t *testing.T) {
	g := NewWithT(t)

	platform := New("test-utilities:latest", "test-capg:latest", nil)
	fakeClient := fake.NewClientBuilder().Build()

	// Test minimal implementation returns no error
	err := platform.ReconcileCredentials(
		context.Background(),
		fakeClient,
		nil, // createOrUpdate function
		&hyperv1.HostedCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-cluster",
				Namespace: "test-namespace",
			},
			Spec: hyperv1.HostedClusterSpec{
				Platform: hyperv1.PlatformSpec{
					Type: hyperv1.GCPPlatform,
					GCP: &hyperv1.GCPPlatformSpec{
						Project: "test-project",
						Region:  "us-central1",
					},
				},
			},
		},
		"test-control-plane-namespace",
	)

	g.Expect(err).To(BeNil()) // Minimal implementation returns nil
}

func TestReconcileSecretEncryption(t *testing.T) {
	g := NewWithT(t)

	platform := New("test-utilities:latest", "test-capg:latest", nil)
	fakeClient := fake.NewClientBuilder().Build()

	// Test minimal implementation returns no error
	err := platform.ReconcileSecretEncryption(
		context.Background(),
		fakeClient,
		nil, // createOrUpdate function
		&hyperv1.HostedCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-cluster",
				Namespace: "test-namespace",
			},
			Spec: hyperv1.HostedClusterSpec{
				Platform: hyperv1.PlatformSpec{
					Type: hyperv1.GCPPlatform,
					GCP: &hyperv1.GCPPlatformSpec{
						Project: "test-project",
						Region:  "us-central1",
					},
				},
			},
		},
		"test-control-plane-namespace",
	)

	g.Expect(err).To(BeNil()) // Minimal implementation returns nil
}

func TestCAPIProviderPolicyRules(t *testing.T) {
	g := NewWithT(t)

	platform := New("test-utilities:latest", "test-capg:latest", nil)

	// Test cloud provider pattern returns nil (like AWS/Azure)
	rules := platform.CAPIProviderPolicyRules()
	g.Expect(rules).To(BeNil()) // Cloud provider pattern returns nil
}

func TestDeleteCredentials(t *testing.T) {
	g := NewWithT(t)

	platform := New("test-utilities:latest", "test-capg:latest", nil)
	fakeClient := fake.NewClientBuilder().Build()

	// Test minimal implementation returns no error
	err := platform.DeleteCredentials(
		context.Background(),
		fakeClient,
		&hyperv1.HostedCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-cluster",
				Namespace: "test-namespace",
			},
			Spec: hyperv1.HostedClusterSpec{
				Platform: hyperv1.PlatformSpec{
					Type: hyperv1.GCPPlatform,
					GCP: &hyperv1.GCPPlatformSpec{
						Project: "test-project",
						Region:  "us-central1",
					},
				},
			},
		},
		"test-control-plane-namespace",
	)

	g.Expect(err).To(BeNil()) // Minimal implementation returns nil
}
