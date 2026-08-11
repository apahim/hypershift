# Plan: Add GCP Karpenter Support to Hypershift

## Context Summary

Hypershift already has:
- **Full GCP platform support** (TechPreview) for hosted clusters via CAPG, WIF, PSC, Cloud DNS, etc.
- **Full AWS Karpenter support** (TechPreview) with a mature architecture: `karpenter-operator` (custom) + upstream `karpenter-provider-aws` binary, both deployed in the management cluster's HCP namespace, managing resources in the guest cluster.

The GCP Karpenter provider (`cloudpilot-ai/karpenter-provider-gcp`) is a standalone v0.x/alpha project with all 9 CloudProvider interface methods implemented, using `GCENodeClass` as its CRD.

The goal is to extend Hypershift's Karpenter integration to support GCP, following the same architectural patterns as the AWS implementation.

---

## Architecture Overview

```
Management Cluster (HCP Namespace)            Guest Cluster
┌──────────────────────────────────┐          ┌──────────────────────────────┐
│                                  │          │                              │
│  karpenter-operator              │          │  CRDs:                       │
│  (HyperShift image)              │──guest──>│    OpenshiftGCENodeClass     │
│  - installs CRDs in guest       │  API     │    GCENodeClass (upstream)   │
│  - creates default NodeClass     │          │    NodePool, NodeClaim       │
│  - syncs OGCENC -> GCENC         │          │                              │
│  - manages ignition/user-data    │          │  User creates:               │
│  - approves CSRs                 │          │    OpenshiftGCENodeClass      │
│                                  │          │    NodePool                   │
│  karpenter-provider-gcp          │          │                              │
│  (upstream GCP binary)           │──guest──>│  Karpenter creates:          │
│  - watches NodePool/NodeClaim    │  API     │    NodeClaim -> GCE Instance │
│  - provisions GCE instances      │          │                              │
│  - manages instance lifecycle    │──GCP───> │                              │
│                                  │  API     │                              │
└──────────────────────────────────┘          └──────────────────────────────┘
```

---

## Phase 0: Dependency Alignment

**Goal**: Get all three repos (`sigs.k8s.io/karpenter`, `karpenter-provider-aws`, `karpenter-provider-gcp`) on compatible versions.

### 0.1 Update OpenShift karpenter core fork to v1.14.0

- Rebase `github.com/openshift/kubernetes-sigs-karpenter` from v1.13.0 to v1.14.0
- Update the replace directive in Hypershift's `go.mod`:
  ```
  replace sigs.k8s.io/karpenter => github.com/openshift/kubernetes-sigs-karpenter v0.0.0-<new-date>-<new-commit>
  ```

### 0.2 Update OpenShift karpenter-provider-aws fork

- Rebase `github.com/openshift/aws-karpenter-provider-aws` against the upstream version that uses karpenter core v1.14.0
- Update the replace directive in Hypershift's `go.mod`

### 0.3 Create OpenShift fork of karpenter-provider-gcp

- Fork `github.com/cloudpilot-ai/karpenter-provider-gcp` to `github.com/openshift/karpenter-provider-gcp` (or equivalent)
- Ensure it compiles against the same OpenShift karpenter core fork
- Add a replace directive in Hypershift's `go.mod`:
  ```
  github.com/cloudpilot-ai/karpenter-provider-gcp => github.com/openshift/karpenter-provider-gcp v0.0.0-<date>-<commit>
  ```

### 0.4 Add the GCP provider dependency to Hypershift's go.mod

- Add `github.com/cloudpilot-ai/karpenter-provider-gcp` as a direct dependency
- Run `go mod tidy` and `go mod vendor`
- Verify no compile conflicts between the AWS and GCP provider types coexisting in the same binary

### Current dependency versions for reference

| Dependency | Hypershift (current) | karpenter-provider-gcp (current) |
|---|---|---|
| `sigs.k8s.io/karpenter` | v1.13.0 (OpenShift fork) | v1.14.0 (upstream) |
| `sigs.k8s.io/controller-runtime` | v0.24.1 | v0.24.1 |
| `k8s.io/api` | v0.36.3 | v0.36.0 |
| `k8s.io/apimachinery` | v0.36.3 | v0.36.0 |
| `k8s.io/client-go` | v0.36.3 | v0.36.0 |
| `github.com/awslabs/operatorpkg` | v0.0.0-20260501 | v0.0.0-20260708 |
| Go version | 1.26.3 | 1.26.5 |

---

## Phase 1: API Types

**Goal**: Extend the Hypershift API to support GCP as a Karpenter platform.

### 1.1 Extend `KarpenterConfig` union type

**File**: `api/hypershift/v1beta1/hostedcluster_types.go`

- Add `GCP` to the `KarpenterConfig.Platform` enum: `+kubebuilder:validation:Enum=AWS;GCP`
- Add `KarpenterGCPConfig` struct as a new union member:
  ```go
  // KarpenterGCPConfig specifies GCP-specific configuration for the Karpenter provisioner.
  type KarpenterGCPConfig struct {
      // serviceAccountEmail is the GCP service account email that Karpenter
      // uses via Workload Identity Federation to manage GCE instances
      // in the hosted cluster's GCP project.
      //
      // +required
      // +kubebuilder:validation:XValidation:rule="self.matches('^[^@]+@[^@]+\\.iam\\.gserviceaccount\\.com$')",message="serviceAccountEmail must be a valid GCP IAM service account email"
      ServiceAccountEmail string `json:"serviceAccountEmail"`
  }
  ```
- Add CEL validation rule to `KarpenterConfig`:
  ```
  +kubebuilder:validation:XValidation:rule="self.platform == 'GCP' ? has(self.gcp) : !has(self.gcp)",message="gcp is required when platform is GCP, and forbidden otherwise"
  ```
- Add the union member field:
  ```go
  // +optional
  // +unionMember
  // +openshift:enable:FeatureGate=GCPPlatform
  GCP KarpenterGCPConfig `json:"gcp,omitzero"`
  ```

### 1.2 Create `OpenshiftGCENodeClass` CRD types

**New file**: `api/karpenter/v1/gcp_types.go`

This is the GCP equivalent of `OpenshiftEC2NodeClass` -- the user-facing abstraction over the upstream `GCENodeClass`.

```go
// +genclient
// +genclient:nonNamespaced
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=ogcenc;ogcencs,categories=karpenter
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type==\"Ready\")].status"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
type OpenshiftGCENodeClass struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    Spec   OpenshiftGCENodeClassSpec   `json:"spec,omitzero"`
    Status OpenshiftGCENodeClassStatus `json:"status,omitempty,omitzero"`
}
```

**`OpenshiftGCENodeClassSpec` fields:**

| Field | Type | Description |
|---|---|---|
| `disks` | `[]GCPDisk` | Boot/data disk config (type, size, KMS encryption, IOPS) |
| `imageSelectorTerms` | `[]GCPImageSelectorTerm` | Image family, channel, version (COS/Ubuntu) |
| `networkTags` | `[]string` | GCE network tags for firewall rules |
| `shieldedInstanceConfig` | `*ShieldedInstanceConfig` | Secure Boot, vTPM, Integrity Monitoring |
| `confidentialInstanceType` | `*string` | SEV, SEV_SNP, TDX |
| `labels` | `map[string]string` | GCE VM instance labels |
| `metadata` | `map[string]string` | Custom instance metadata (reserved GKE keys blocked) |
| `version` | `string` | OpenShift version (resolved to release image via Cincinnati) |
| `kubelet` | `*KubeletConfiguration` | Kubelet settings (reuse from `kubelet_config.go`) |
| `autoGPUTaint` | `*bool` | Auto-apply nvidia.com/gpu taint |
| `gpuDriverVersion` | `*string` | default / latest / disabled |

**`OpenshiftGCENodeClassStatus` fields:**

| Field | Type | Description |
|---|---|---|
| `conditions` | `[]metav1.Condition` | Ready, VersionResolved, SupportedVersionSkew |
| `images` | `[]GCPImage` | Resolved GCE images (sourceImage + requirements) |
| `releaseImage` | `string` | Fully qualified release image |
| `version` | `string` | Resolved OpenShift version |

**Constants:**
```go
KarpenterProviderGCPImage = "hypershift.openshift.io/karpenter-provider-gcp-image"
```

### 1.3 Register new types in API scheme

**File**: `api/karpenter/v1/register.go`

Register `OpenshiftGCENodeClass` and `OpenshiftGCENodeClassList` in the `SchemeBuilder`.

### 1.4 Generate deepcopy, CRD manifests, and clients

- Run `make generate` for deepcopy (`zz_generated.deepcopy.go`)
- Run CRD generation to produce `karpenter.hypershift.openshift.io_openshiftgcenodeclasses.yaml`
- Run client-gen for typed clients, listers, informers, apply configurations:
  - `client/clientset/clientset/typed/karpenter/v1/openshiftgcenodeclass.go`
  - `client/listers/karpenter/v1/openshiftgcenodeclass.go`
  - `client/informers/externalversions/karpenter/v1/openshiftgcenodeclass.go`
  - `client/applyconfiguration/karpenter/v1/openshiftgcenodeclassspec.go`
  - `client/applyconfiguration/hypershift/v1beta1/karpentergcpconfig.go`

---

## Phase 2: Support Utilities

**Goal**: Extend shared Karpenter utilities for GCP.

### 2.1 Extend `IsKarpenterEnabled()`

**File**: `support/karpenter/karpenter.go` (line 100-103)

```go
func IsKarpenterEnabled(autoNode hyperv1.AutoNode) bool {
    return autoNode.Provisioner.Name == hyperv1.ProvisionerKarpenter &&
        (autoNode.Provisioner.Karpenter.Platform == hyperv1.AWSPlatform ||
         autoNode.Provisioner.Karpenter.Platform == hyperv1.GCPPlatform)
}
```

### 2.2 Extend `SupportedArchitectures()`

**File**: `support/karpenter/karpenter.go` (line 89-96)

Add GCP case:
```go
case hyperv1.GCPPlatform:
    return []string{hyperv1.ArchitectureAMD64, hyperv1.ArchitectureARM64}, nil
```

### 2.3 Add GCP NodeClass helper functions

**File**: `support/karpenter/karpenter.go`

```go
func KarpenterGCPNodePoolName(ogcenc *hyperkarpenterv1.OpenshiftGCENodeClass) string {
    return ogcenc.Name + "-karpenter"
}

func ArchToImageLabelKey(arch string) string {
    return "hypershift.openshift.io/image-" + arch
}
```

### 2.4 Register GCP Karpenter types in scheme

**File**: `support/api/scheme.go`

Add alongside existing EC2NodeClass registrations (around line 176):
```go
gcpKarpenterGroupVersion := schema.GroupVersion{Group: gcpkarpenterapis.Group, Version: "v1alpha1"}
scheme.AddKnownTypes(gcpKarpenterGroupVersion,
    &gcpkarpenterv1alpha1.GCENodeClass{},
    &gcpkarpenterv1alpha1.GCENodeClassList{},
)
```

---

## Phase 3: Karpenter Operator Controllers

**Goal**: Create GCP-specific controllers mirroring the AWS ones.

### 3.1 GCENodeClass Controller

**New file**: `karpenter-operator/controllers/nodeclass/gce_nodeclass_controller.go`

This is the most substantial new code -- maps `OpenshiftGCENodeClass` to upstream `GCENodeClass`. Responsibilities:

1. **Watch** `OpenshiftGCENodeClass` in guest cluster
2. **Create/sync** a corresponding upstream `GCENodeClass` with:
   - **Service-managed fields**: `imageSelectorTerms` (from resolved release image), `serviceAccount` (from HCP config)
   - **User-configured fields**: `disks`, `networkTags`, `shieldedInstanceConfig`, `confidentialInstanceType`, `labels`, `metadata`, `kubeletConfiguration`, `autoGPUTaint`, `gpuDriverVersion`
3. **Install VAP** in guest cluster preventing direct `GCENodeClass` modification
4. **Sync status** back (images, conditions)
5. **Reconcile CRDs** (install `GCENodeClass` CRD + `OpenshiftGCENodeClass` CRD)

Key mapping logic (OpenshiftGCENodeClass -> upstream GCENodeClass):

| OpenshiftGCENodeClass field | GCENodeClass field | Notes |
|---|---|---|
| `spec.disks` | `spec.disks` | Direct mapping with type conversion |
| `spec.imageSelectorTerms` | `spec.imageSelectorTerms` | Merged with service-managed image terms |
| `spec.networkTags` | `spec.networkTags` | Direct mapping |
| `spec.shieldedInstanceConfig` | `spec.shieldedInstanceConfig` | Direct mapping |
| `spec.confidentialInstanceType` | `spec.confidentialInstanceType` | Direct mapping |
| `spec.labels` | `spec.labels` | Merged with platform tags |
| `spec.metadata` | `spec.metadata` | Merged with bootstrap metadata |
| `spec.kubelet` | `spec.kubeletConfiguration` | Type conversion |
| (service-managed) | `spec.serviceAccount` | From HCP GCP platform config |

**Also create**: `gce_karpenter_util.go` for type conversion helpers.

### 3.2 Extend the main Karpenter Controller for GCP

**File**: `karpenter-operator/controllers/karpenter/karpenter_controller.go`

Changes needed:

- **Line 59**: Add `crdGCENodeClass` CRD asset loading
- **Line 97**: Add `gcenodeclasses.karpenter.k8s.gcp` to watched CRDs
- **Line 109**: Add watch for `&gcpkarpenterv1alpha1.GCENodeClass{}`
- **Line 289**: Conditionalize default NodeClass creation:
  ```go
  switch hcp.Spec.Platform.Type {
  case hyperv1.AWSPlatform:
      r.reconcileOpenshiftEC2NodeClassDefault(ctx, hcp)
  case hyperv1.GCPPlatform:
      r.reconcileOpenshiftGCENodeClassDefault(ctx, hcp)
  }
  ```
- **Line 402**: Add `crdGCENodeClass` to CRD reconciliation loop (conditionally)
- **New method** `reconcileOpenshiftGCENodeClassDefault()`: creates a default `OpenshiftGCENodeClass` with GCP-appropriate defaults (pd-balanced boot disk, COS image family, cluster channel)

### 3.3 GCP Machine Approver

**New file**: `karpenter-operator/controllers/karpenter/gcp_machine_approver.go`

Replace the EC2 `DescribeInstances` call with a GCE Compute API equivalent:

```go
func (r *GCPMachineApproverController) getGCEInstancesDNSNames(ctx context.Context) ([]string, error) {
    // Use compute.Instances.AggregatedList() to find instances
    // with the cluster's karpenter labels
    // Return internal DNS names for CSR validation
}

func getComputeClient(ctx context.Context) (*compute.InstancesClient, error) {
    // Create GCE compute client from GOOGLE_APPLICATION_CREDENTIALS
}
```

Authenticate using the WIF credentials mounted in the karpenter-operator pod.

### 3.4 Extend Ignition Controller for GCP

**File**: `karpenter-operator/controllers/karpenterignition/karpenterignition_controller.go`

This controller is ~95% provider-neutral. Changes needed:

- **Line 77**: Add a watch for `OpenshiftGCENodeClass` (in addition to `OpenshiftEC2NodeClass`)
- **Line 110**: Make the reconcile loop handle both NodeClass types

Option A (recommended): Introduce a `NodeClassAccessor` interface:
```go
type NodeClassAccessor interface {
    client.Object
    GetVersion() string
    GetKubelet() *KubeletConfiguration
}
```

Both `OpenshiftEC2NodeClass` and `OpenshiftGCENodeClass` would implement this, and the controller would use the interface instead of concrete types.

Option B: Duplicate the controller with GCP-specific types (simpler but more code).

### 3.5 Add GCE CRD assets

**New files in** `karpenter-operator/controllers/karpenter/assets/`:

| File | Source |
|---|---|
| `karpenter.k8s.gcp_gcenodeclasses.yaml` | From upstream karpenter-provider-gcp `charts/karpenter/crds/` |
| `karpenter.hypershift.openshift.io_openshiftgcenodeclasses.yaml` | Generated from Phase 1.4 |

**Update**: `assets.go` to embed these files:
```go
//go:embed karpenter.k8s.gcp_gcenodeclasses.yaml
//go:embed karpenter.hypershift.openshift.io_openshiftgcenodeclasses.yaml
```

Add CRD validation test suites under `assets/tests/openshiftgcenodeclasses.karpenter.hypershift.openshift.io/`.

### 3.6 Update karpenter-operator `main.go`

**File**: `karpenter-operator/main.go`

- **Line 89-92**: Register GCP Karpenter types in the scheme:
  ```go
  gcpKarpenterGroupVersion := schema.GroupVersion{Group: gcpkarpenterapis.Group, Version: "v1alpha1"}
  scheme.AddKnownTypes(gcpKarpenterGroupVersion,
      &gcpkarpenterv1alpha1.GCENodeClass{},
      &gcpkarpenterv1alpha1.GCENodeClassList{},
  )
  ```
- **Line 142-143**: Register the GCENodeClass controller (conditionally based on platform):
  ```go
  // Determine platform from HCP
  switch platform {
  case hyperv1.AWSPlatform:
      encr := nodeclass.EC2NodeClassReconciler{...}
  case hyperv1.GCPPlatform:
      encr := nodeclass.GCENodeClassReconciler{...}
  }
  ```
- Register the GCP machine approver (conditionally)

---

## Phase 4: Control Plane Operator Components

**Goal**: Deploy the GCP Karpenter provider binary alongside the karpenter-operator.

### 4.1 Extend Karpenter deployment for GCP

**File**: `control-plane-operator/controllers/hostedcontrolplane/v2/karpenter/deployment.go`

Add platform switch in `adaptDeployment`:

```go
switch hcp.Spec.Platform.Type {
case hyperv1.AWSPlatform:
    // existing AWS env vars (AWS_REGION, etc.)
case hyperv1.GCPPlatform:
    podspec.SetEnvVar(container, &corev1.EnvVar{
        Name:  "GOOGLE_APPLICATION_CREDENTIALS",
        Value: "/etc/provider/credentials.json",
    })
    podspec.SetEnvVar(container, &corev1.EnvVar{
        Name:  "PROJECT_ID",
        Value: hcp.Spec.Platform.GCP.ProjectID,
    })
    podspec.SetEnvVar(container, &corev1.EnvVar{
        Name:  "CLUSTER_NAME",
        Value: hcp.Spec.InfraID,
    })
    podspec.SetEnvVar(container, &corev1.EnvVar{
        Name:  "CLUSTER_LOCATION",
        Value: hcp.Spec.Platform.GCP.Region,
    })
    // Set GCP provider image
    if image, ok := hcp.Annotations[hyperkarpenterv1.KarpenterProviderGCPImage]; ok {
        container.Image = image
    }
}
```

### 4.2 Extend karpenter-operator credentials (`secret.go`)

**File**: `control-plane-operator/controllers/hostedcontrolplane/v2/karpenteroperator/secret.go`

Add GCP WIF credential generation. Reuse the existing `support/gcputil/gcputil.go` `BuildWorkloadIdentityCredentials()` function:

```go
func adaptCredentialsSecret(cpContext ControlPlaneContext, secret *corev1.Secret) error {
    hcp := cpContext.HCP
    switch hcp.Spec.Platform.Type {
    case hyperv1.AWSPlatform:
        // existing AWS credentials template
    case hyperv1.GCPPlatform:
        gcpWIF := hcp.Spec.Platform.GCP.WorkloadIdentity
        karpenterSA := hcp.Spec.AutoNode.Provisioner.Karpenter.GCP.ServiceAccountEmail
        credJSON := gcputil.BuildWorkloadIdentityCredentials(
            gcpWIF.ProjectNumber,
            gcpWIF.PoolID,
            gcpWIF.ProviderID,
            karpenterSA,
            "/var/run/secrets/openshift/serviceaccount/token",
        )
        secret.Data = map[string][]byte{
            "credentials.json": credJSON,
        }
    }
    return nil
}
```

### 4.3 Extend karpenter-operator deployment

**File**: `control-plane-operator/controllers/hostedcontrolplane/v2/karpenteroperator/deployment.go`

Add `case hyperv1.GCPPlatform:` block (around line 28-73):

```go
case hyperv1.GCPPlatform:
    // Mount GCP credentials volume
    dep.Spec.Template.Spec.Volumes = append(dep.Spec.Template.Spec.Volumes,
        corev1.Volume{
            Name: "provider-creds",
            VolumeSource: corev1.VolumeSource{
                Secret: &corev1.SecretVolumeSource{
                    SecretName: "karpenter-credentials",
                },
            },
        },
    )
    container.VolumeMounts = append(container.VolumeMounts,
        corev1.VolumeMount{Name: "provider-creds", MountPath: "/etc/provider"},
    )
    // Set GCP env vars
    podspec.SetEnvVar(container, &corev1.EnvVar{
        Name:  "GOOGLE_APPLICATION_CREDENTIALS",
        Value: "/etc/provider/credentials.json",
    })
    podspec.SetEnvVar(container, &corev1.EnvVar{
        Name:  "GCP_REGION",
        Value: hcp.Spec.Platform.GCP.Region,
    })
    // Extract shared args from the AWS-only block
    container.Args = append(container.Args,
        "--control-plane-operator-image", karp.ControlPlaneOperatorImage,
    )
```

Also extract `--control-plane-operator-image` and `RHOBS_MONITORING` from the AWS-only block to be shared across platforms.

### 4.4 Add GCP provider image references

**File**: `support/images/envvars.go`
```go
GCPKarpenterProviderEnvVar = "IMAGE_GCP_KARPENTER_PROVIDER"
```

**File**: `karpenter-operator/controllers/karpenter/assets/assets.go`
```go
DefaultKarpenterProviderGCPImage = "public.ecr.aws/cloudpilotai/gcp/karpenter:v0.5.0"
```

**File**: `support/controlplane-component/defaults.go`
Add fallback for GCP Karpenter provider image (alongside the existing AWS fallback around line 648).

**File**: `api/hypershift/v1beta1/hostedcluster_types.go`
```go
ClusterAPIGCPKarpenterProviderImage = "hypershift.openshift.io/capi-provider-gcp-karpenter-image"
```

### 4.5 Deployment YAML assets

The existing `assets/karpenter/deployment.yaml` is AWS-specific (hardcoded `AWS_SHARED_CREDENTIALS_FILE`, `AWS_SDK_LOAD_CONFIG` env vars). Two options:

**Option A (recommended)**: Make the YAML more generic by removing cloud-specific env vars and volumes, moving them entirely to Go adaptation code. This avoids maintaining separate YAML per platform.

**Option B**: Create a separate `gcp-deployment.yaml` for the GCP case.

---

## Phase 5: HostedCluster Controller Integration

**Goal**: Wire GCP Karpenter into the HostedCluster reconciliation loop.

### 5.1 HostedCluster Karpenter reconciler

**File**: `hypershift-operator/controllers/hostedcluster/karpenter.go`

**No changes needed.** The existing code delegates through `IsKarpenterEnabled()` (extended in Phase 2.1) and ControlPlaneComponent abstractions. The control flow (enable/disable, finalizer management, condition computation) is fully platform-agnostic.

### 5.2 Cluster dump support

**File**: `cmd/cluster/core/dump.go` (line 110)

Add GCENodeClass to the dump resource list:
```go
&gcpkarpenterv1alpha1.GCENodeClass{},
```

---

## Phase 6: CLI Support

**Goal**: Allow users to create GCP hosted clusters with Karpenter enabled.

### 6.1 Extend `hypershift create cluster gcp` command

**File**: `cmd/cluster/gcp/create.go`

Add flags:
```go
cmd.Flags().BoolVar(&opts.AutoNode, "auto-node", false, "Enable Karpenter for automatic node provisioning")
cmd.Flags().StringVar(&opts.KarpenterServiceAccount, "karpenter-service-account", "", "GCP service account email for Karpenter")
```

Populate `HostedCluster.Spec.AutoNode` with the GCP Karpenter config:
```go
if opts.AutoNode {
    hc.Spec.AutoNode = hyperv1.AutoNode{
        Provisioner: hyperv1.ProvisionerConfig{
            Name: hyperv1.ProvisionerKarpenter,
            Karpenter: hyperv1.KarpenterConfig{
                Platform: hyperv1.GCPPlatform,
                GCP: hyperv1.KarpenterGCPConfig{
                    ServiceAccountEmail: opts.KarpenterServiceAccount,
                },
            },
        },
    }
}
```

### 6.2 Extend GCP IAM creation

**File**: `cmd/infra/gcp/create_iam.go` and `cmd/infra/gcp/iam-bindings.json`

Add a Karpenter-specific service account with the required IAM roles:

```json
{
  "name": "karpenter",
  "roles": [
    "roles/compute.instanceAdmin.v1",
    "roles/compute.networkUser",
    "roles/iam.serviceAccountUser",
    "roles/container.clusterViewer"
  ],
  "k8sServiceAccounts": [
    {
      "namespace": "kube-system",
      "name": "karpenter"
    }
  ]
}
```

These roles are required by the GCP Karpenter provider (per `karpenter-provider-gcp/deploy/iam/karpenter-controller-role.yaml`).

---

## Phase 7: Testing

### 7.1 Unit tests

| New test file | Tests |
|---|---|
| `karpenter-operator/controllers/nodeclass/gce_nodeclass_controller_test.go` | GCENodeClass reconciliation, status sync, VAP creation, default disk config |
| `karpenter-operator/controllers/nodeclass/gce_karpenter_util_test.go` | Type conversion helpers |
| `karpenter-operator/controllers/karpenter/gcp_machine_approver_test.go` | GCE instance DNS lookup, CSR authorization |
| `api/karpenter/v1/gcp_types_test.go` | CRD validation, defaulting |
| `control-plane-operator/controllers/hostedcontrolplane/v2/karpenteroperator/secret_test.go` | Extend with GCP WIF credential generation tests |
| `control-plane-operator/controllers/hostedcontrolplane/v2/karpenteroperator/deployment_test.go` | Extend with GCP env var injection tests |

**Extend existing tests:**

| Existing test file | Changes |
|---|---|
| `support/karpenter/karpenter_test.go` | Add GCP cases for `IsKarpenterEnabled`, `SupportedArchitectures` |
| `karpenter-operator/controllers/karpenter/karpenter_controller_test.go` | Add GCP default NodeClass creation tests |

### 7.2 E2E tests

**New file**: `test/e2e/karpenter_gcp_test.go`

Test scenarios:
- Default `OpenshiftGCENodeClass` creation on cluster bootstrap
- Node provisioning (on-demand, spot)
- NodeClass drift detection (change disk config, verify node replacement)
- GPU node provisioning (if applicable)
- Version resolution and skew detection
- Kubelet configuration propagation
- Cluster deletion with graceful NodeClaim cleanup

**New test fixtures**:
- `test/e2e/v2/tests/assets/karpenter-gcp-nodepool.yaml`
- `test/e2e/v2/tests/assets/karpenter-gcp-workloads.yaml`

---

## Implementation Order

| Phase | Effort | Dependencies | Parallelizable |
|---|---|---|---|
| **Phase 0** (Dependencies) | Medium | External (fork management) | No -- must be first |
| **Phase 1** (API Types) | Medium | Phase 0 | No -- types needed by all other phases |
| **Phase 2** (Support Utils) | Small | Phase 1 | Yes -- with Phase 3/4 |
| **Phase 3** (Operator Controllers) | **Large** -- bulk of the work | Phase 1, 2 | Yes -- with Phase 4 |
| **Phase 4** (CPO Components) | Medium | Phase 1, 2 | Yes -- with Phase 3 |
| **Phase 5** (HC Controller) | Small | Phase 2 | Yes -- with Phase 3/4 |
| **Phase 6** (CLI) | Small | Phase 1 | Yes -- with Phase 3/4 |
| **Phase 7** (Testing) | Medium | All phases | Partially -- unit tests with each phase |

---

## Key Files Modified (Summary)

### Existing files to modify

| File | Phase | Change description |
|---|---|---|
| `api/hypershift/v1beta1/hostedcluster_types.go` | 1.1 | Add `KarpenterGCPConfig`, extend `KarpenterConfig` union |
| `api/karpenter/v1/register.go` | 1.3 | Register `OpenshiftGCENodeClass` types |
| `support/karpenter/karpenter.go` | 2.1-2.3 | Extend `IsKarpenterEnabled`, `SupportedArchitectures`, add GCP helpers |
| `support/api/scheme.go` | 2.4 | Register `GCENodeClass` in scheme |
| `karpenter-operator/main.go` | 3.6 | Register GCP types and controllers |
| `karpenter-operator/controllers/karpenter/karpenter_controller.go` | 3.2 | Add GCP CRD, watch, default NodeClass |
| `karpenter-operator/controllers/karpenter/assets/assets.go` | 3.5 | Add GCP CRD assets and image constant |
| `karpenter-operator/controllers/karpenterignition/karpenterignition_controller.go` | 3.4 | Add GCP NodeClass watch, introduce `NodeClassAccessor` |
| `control-plane-operator/controllers/hostedcontrolplane/v2/karpenter/deployment.go` | 4.1 | Add GCP platform case |
| `control-plane-operator/controllers/hostedcontrolplane/v2/karpenteroperator/deployment.go` | 4.3 | Add GCP platform case |
| `control-plane-operator/controllers/hostedcontrolplane/v2/karpenteroperator/secret.go` | 4.2 | Add GCP WIF credential generation |
| `control-plane-operator/controllers/hostedcontrolplane/v2/assets/karpenter/deployment.yaml` | 4.4 | Make cloud-neutral (move env vars to Go code) |
| `support/images/envvars.go` | 4.4 | Add GCP Karpenter provider env var |
| `support/controlplane-component/defaults.go` | 4.4 | Add GCP provider image fallback |
| `cmd/cluster/gcp/create.go` | 6.1 | Add `--auto-node` and `--karpenter-service-account` flags |
| `cmd/infra/gcp/create_iam.go` | 6.2 | Add Karpenter service account |
| `cmd/infra/gcp/iam-bindings.json` | 6.2 | Add Karpenter IAM role bindings |
| `cmd/cluster/core/dump.go` | 5.2 | Add `GCENodeClass` to dump list |
| `go.mod` | 0.4 | Add GCP provider dependency, update replace directives |

### New files to create

| File | Phase | Description |
|---|---|---|
| `api/karpenter/v1/gcp_types.go` | 1.2 | `OpenshiftGCENodeClass` CRD types |
| `api/karpenter/v1/zz_generated.deepcopy.go` | 1.4 | Generated deepcopy (updated) |
| `karpenter-operator/controllers/nodeclass/gce_nodeclass_controller.go` | 3.1 | GCENodeClass reconciler |
| `karpenter-operator/controllers/nodeclass/gce_karpenter_util.go` | 3.1 | Type conversion helpers |
| `karpenter-operator/controllers/karpenter/gcp_machine_approver.go` | 3.3 | GCP CSR approval |
| `karpenter-operator/controllers/karpenter/assets/karpenter.k8s.gcp_gcenodeclasses.yaml` | 3.5 | Upstream GCENodeClass CRD |
| `karpenter-operator/controllers/karpenter/assets/karpenter.hypershift.openshift.io_openshiftgcenodeclasses.yaml` | 3.5 | OpenShift GCE NodeClass CRD |
| Generated client files under `client/` | 1.4 | Clientset, listers, informers, apply configs |
| Test files (see Phase 7) | 7 | Unit and E2E tests |

---

## Key Risk: OpenShift Node Bootstrap on GCP

The GCP Karpenter provider's bootstrap mechanism (`karpenter-provider-gcp/pkg/providers/instance/metadata_bootstrap.go`) is designed for standard GKE nodes using `kube-env` metadata for kubelet configuration. OpenShift nodes use **ignition-based provisioning** with MachineConfig.

The karpenter-operator's ignition controller (Phase 3.4) will need to generate OpenShift-compatible ignition configs and inject them as the `GCENodeClass.spec.metadata` user data. This is the **most technically challenging** piece of the integration.

The AWS integration handles this by:
1. The ignition controller creates a userData Secret containing an ignition config
2. The EC2NodeClass controller sets `spec.userData` pointing to this Secret
3. The upstream karpenter-provider-aws injects the userData into the EC2 instance's launch template

For GCP, the equivalent flow would be:
1. The ignition controller creates a userData Secret (same as AWS -- this is provider-neutral)
2. The GCENodeClass controller injects the ignition/bootstrap content into `spec.metadata` (specifically the `user-data` or `startup-script` metadata key)
3. The upstream karpenter-provider-gcp needs to pass this metadata through to the GCE instance

**Risk mitigation**: The upstream karpenter-provider-gcp already supports custom `spec.metadata` on `GCENodeClass`. The key question is whether the OpenShift ignition payload can be delivered via GCE instance metadata and whether the RHCOS/FCOS boot process on GCP can consume it. This needs to be validated early (e.g., in Phase 0 as a proof-of-concept).

---

## Open Questions

1. **Ignition delivery on GCP**: Can OpenShift ignition configs be delivered via GCE instance metadata? RHCOS on GCP typically reads ignition from the `user-data` metadata key -- confirm this works with the karpenter-provider-gcp's metadata injection.

2. **Image selection**: OpenShift uses RHCOS images, not GKE node images (COS/Ubuntu). The `OpenshiftGCENodeClass` image selector needs to resolve to RHCOS images in the OpenShift release payload, not GKE images. The ignition controller already handles this for AWS (AMI resolution from release image) -- the same pattern applies to GCP (GCE image resolution from release image).

3. **Feature gate**: Should GCP Karpenter be behind its own feature gate (e.g., `KarpenterGCP`) or share the existing `KarpenterOperator` + `GCPPlatform` gates?

4. **Network config**: The default `OpenshiftGCENodeClass` needs sensible defaults for the GCP subnetwork. Should it inherit from the HostedCluster's GCP platform config (PSC subnet, primary subnet)?

5. **Service account**: Should the Karpenter node service account be the same as the one used by the HostedCluster's CAPG node pool, or a separate dedicated one?
