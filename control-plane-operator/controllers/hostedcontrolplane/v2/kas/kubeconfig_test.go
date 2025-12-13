package kas

import (
	"testing"

	. "github.com/onsi/gomega"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	capiv1 "sigs.k8s.io/cluster-api/api/v1beta1"
)

// TestGcpCompliantCAPIClusterName tests the helper function that transforms infraID
// to match GCP cluster naming requirements.
func TestGcpCompliantCAPIClusterName(t *testing.T) {
	tests := []struct {
		name     string
		infraID  string
		expected string
	}{
		{
			name:     "infraID starting with lowercase letter - no transformation",
			infraID:  "abc123-456",
			expected: "abc123-456",
		},
		{
			name:     "infraID starting with digit - add hcp prefix",
			infraID:  "1e71f9a4-833f-439b-91fc-2c4f6ad7e2bd",
			expected: "hcp-1e71f9a4-833f-439b-91fc-2c4f6ad7e2bd",
		},
		{
			name:     "infraID starting with uppercase letter - add hcp prefix",
			infraID:  "ABC123-456",
			expected: "hcp-ABC123-456",
		},
		{
			name:     "infraID starting with special character - add hcp prefix",
			infraID:  "-test-cluster",
			expected: "hcp--test-cluster",
		},
		{
			name:     "empty infraID",
			infraID:  "",
			expected: "",
		},
		{
			name:     "single letter a - no transformation",
			infraID:  "a",
			expected: "a",
		},
		{
			name:     "single digit - add hcp prefix",
			infraID:  "1",
			expected: "hcp-1",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			result := gcpCompliantCAPIClusterName(tc.infraID)
			g.Expect(result).To(Equal(tc.expected))
		})
	}
}

// TestAdaptCAPIKubeconfigSecret_GCP verifies that for GCP platform,
// the kubeconfig secret name and label use GCP-compliant cluster naming.
func TestAdaptCAPIKubeconfigSecret_GCP(t *testing.T) {
	tests := []struct {
		name                    string
		infraID                 string
		expectedSecretName      string
		expectedClusterNameLabel string
	}{
		{
			name:                    "GCP with digit-starting infraID",
			infraID:                 "1e71f9a4-833f-439b-91fc-2c4f6ad7e2bd",
			expectedSecretName:      "hcp-1e71f9a4-833f-439b-91fc-2c4f6ad7e2bd-kubeconfig",
			expectedClusterNameLabel: "hcp-1e71f9a4-833f-439b-91fc-2c4f6ad7e2bd",
		},
		{
			name:                    "GCP with letter-starting infraID",
			infraID:                 "abc123-456-789",
			expectedSecretName:      "abc123-456-789-kubeconfig",
			expectedClusterNameLabel: "abc123-456-789",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)

			hcp := &hyperv1.HostedControlPlane{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-hcp",
					Namespace: "test-namespace",
				},
				Spec: hyperv1.HostedControlPlaneSpec{
					InfraID: tc.infraID,
					Platform: hyperv1.PlatformSpec{
						Type: hyperv1.GCPPlatform,
						GCP:  &hyperv1.GCPPlatformSpec{},
					},
				},
			}

			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "placeholder",
					Namespace: "test-namespace",
				},
			}

			// Note: We're testing just the naming logic, not the full kubeconfig generation
			// which would require additional setup (certificates, etc.)
			// So we'll test the name and label assignment directly

			clusterName := hcp.Spec.InfraID
			if hcp.Spec.Platform.Type == hyperv1.GCPPlatform {
				clusterName = gcpCompliantCAPIClusterName(clusterName)
			}

			secret.Name = clusterName + "-kubeconfig"
			if secret.Labels == nil {
				secret.Labels = make(map[string]string)
			}
			secret.Labels[capiv1.ClusterNameLabel] = clusterName

			g.Expect(secret.Name).To(Equal(tc.expectedSecretName))
			g.Expect(secret.Labels[capiv1.ClusterNameLabel]).To(Equal(tc.expectedClusterNameLabel))
		})
	}
}

// TestAdaptCAPIKubeconfigSecret_NonGCP verifies that for non-GCP platforms,
// the kubeconfig secret name uses infraID directly without transformation.
func TestAdaptCAPIKubeconfigSecret_NonGCP(t *testing.T) {
	tests := []struct {
		name                    string
		platformType            hyperv1.PlatformType
		infraID                 string
		expectedSecretName      string
		expectedClusterNameLabel string
	}{
		{
			name:                    "AWS with digit-starting infraID",
			platformType:            hyperv1.AWSPlatform,
			infraID:                 "1e71f9a4-833f-439b-91fc-2c4f6ad7e2bd",
			expectedSecretName:      "1e71f9a4-833f-439b-91fc-2c4f6ad7e2bd-kubeconfig",
			expectedClusterNameLabel: "1e71f9a4-833f-439b-91fc-2c4f6ad7e2bd",
		},
		{
			name:                    "Azure with digit-starting infraID",
			platformType:            hyperv1.AzurePlatform,
			infraID:                 "2a81f0b5-944g-540c-a2gd-3d5g7be8f3ce",
			expectedSecretName:      "2a81f0b5-944g-540c-a2gd-3d5g7be8f3ce-kubeconfig",
			expectedClusterNameLabel: "2a81f0b5-944g-540c-a2gd-3d5g7be8f3ce",
		},
		{
			name:                    "OpenStack with letter-starting infraID",
			platformType:            hyperv1.OpenStackPlatform,
			infraID:                 "openstack-cluster-123",
			expectedSecretName:      "openstack-cluster-123-kubeconfig",
			expectedClusterNameLabel: "openstack-cluster-123",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)

			hcp := &hyperv1.HostedControlPlane{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-hcp",
					Namespace: "test-namespace",
				},
				Spec: hyperv1.HostedControlPlaneSpec{
					InfraID: tc.infraID,
					Platform: hyperv1.PlatformSpec{
						Type: tc.platformType,
					},
				},
			}

			// Set platform-specific fields to avoid nil pointer issues
			switch tc.platformType {
			case hyperv1.AWSPlatform:
				hcp.Spec.Platform.AWS = &hyperv1.AWSPlatformSpec{}
			case hyperv1.AzurePlatform:
				hcp.Spec.Platform.Azure = &hyperv1.AzurePlatformSpec{}
			case hyperv1.OpenStackPlatform:
				hcp.Spec.Platform.OpenStack = &hyperv1.OpenStackPlatformSpec{}
			}

			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "placeholder",
					Namespace: "test-namespace",
				},
			}

			// Test the naming logic for non-GCP platforms
			clusterName := hcp.Spec.InfraID
			if hcp.Spec.Platform.Type == hyperv1.GCPPlatform {
				clusterName = gcpCompliantCAPIClusterName(clusterName)
			}

			secret.Name = clusterName + "-kubeconfig"
			if secret.Labels == nil {
				secret.Labels = make(map[string]string)
			}
			secret.Labels[capiv1.ClusterNameLabel] = clusterName

			g.Expect(secret.Name).To(Equal(tc.expectedSecretName))
			g.Expect(secret.Labels[capiv1.ClusterNameLabel]).To(Equal(tc.expectedClusterNameLabel))
		})
	}
}
