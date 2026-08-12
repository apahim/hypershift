# Plan: Add GCP Karpenter Support to Hypershift

## Context Summary

Hypershift already has:
- **Full GCP platform support** (TechPreview) for hosted clusters via CAPG, WIF, PSC, Cloud DNS, etc.
- **Full AWS Karpenter support** (TechPreview) with a mature architecture: `karpenter-operator` (custom) + upstream `karpenter-provider-aws` binary, both deployed in the management cluster's HCP namespace, managing resources in the guest cluster.

The GCP Karpenter provider (`cloudpilot-ai/karpenter-provider-gcp`) is a standalone v0.x/alpha project with all 9 CloudProvider interface methods implemented, using `GCENodeClass` as its CRD.

The goal is to extend Hypershift's Karpenter integration to support GCP, following the same architectural patterns as the AWS implementation.

### Karpenter vs CAPI/CAPG -- No Overlap

Karpenter is a **complete replacement** for CAPI/CAPG as the node lifecycle manager. When Karpenter is enabled via `spec.autoNode`, the Hypershift `NodePool` controller (which creates CAPI `MachineDeployments` / `GCPMachineTemplates`) is **not used**. Instead, users create Karpenter `NodePool` resources (a different CRD: `karpenter.sh/v1`) directly in the guest cluster, and the karpenter-provider-gcp provisions GCE instances directly via the Compute API. There are no Machine, MachineDeployment, or GCPMachineTemplate resources involved.

### What the Upstream Provider Does vs What We Must Build

The upstream `karpenter-provider-gcp` binary only handles the **cloud-side work**: it watches Karpenter `NodePool`/`NodeClaim` CRs, discovers GCE machine types and pricing, provisions and deletes GCE instances, and handles spot interruption and drift detection. It knows nothing about Hypershift, OpenShift, ignition, split control planes, or version management.

Everything else is the **karpenter-operator's responsibility** (the Hypershift-specific controller we must build):

| Responsibility | Component |
|---|---|
| Install CRDs in guest cluster | karpenter-operator (we build) |
| Create default `OpenshiftGCENodeClass` | karpenter-operator (we build) |
| Map `OpenshiftGCENodeClass` -> upstream `GCENodeClass` | karpenter-operator (we build) |
| Generate OpenShift ignition/user-data/token secrets | karpenter-operator (we build) |
| Resolve OCP version -> release image via Cincinnati | karpenter-operator (we build) |
| Resolve release image -> RHCOS GCE image | karpenter-operator (we build) |
| Inject ignition payload into `GCENodeClass.spec.metadata` | karpenter-operator (we build) |
| Manage kubelet config per NodeClass | karpenter-operator (we build) |
| Protect upstream `GCENodeClass` with VAP | karpenter-operator (we build) |
| Approve CSRs for new nodes (GCE API lookup) | karpenter-operator (we build) |
| Manage HCP finalizer + graceful cleanup on deletion | karpenter-operator (we build) |
| Report AutoNode status (node count, vCPUs) | karpenter-operator (we build) |
| Watch NodePool/NodeClaim, provision GCE instances | karpenter-provider-gcp (upstream, deploy as-is) |
| Discover GCE machine types, pricing, zones | karpenter-provider-gcp (upstream, deploy as-is) |
| Handle spot interruption, drift, consolidation | karpenter-provider-gcp (upstream, deploy as-is) |

### User-Facing Workflow

Users interact with the guest cluster API only. They create exactly **two** types of CRs:

1. **`OpenshiftGCENodeClass`** (optional -- a `"default"` is auto-created by the karpenter-operator):
   Configures GCP-specific node properties (disks, network tags, shielded VM, version, kubelet).
   The karpenter-operator mirrors this to an upstream `GCENodeClass` with service-managed fields
   injected (RHCOS image, ignition user-data, service account). A VAP prevents users from
   touching the upstream `GCENodeClass` directly.

2. **`NodePool`** (`karpenter.sh/v1`, required -- triggers actual provisioning):
   References `GCENodeClass` (the upstream type, not the OpenShift wrapper) via `spec.template.spec.nodeClassRef`.
   Defines scheduling constraints (instance families, capacity type, arch) and disruption policy.
   The upstream karpenter-provider-gcp watches this and creates `NodeClaim` resources when pods
   are unschedulable.

Users **never** directly create or modify `GCENodeClass`, `NodeClaim`, or any management-cluster resource.

### CAPI and Karpenter Are Mutually Exclusive

A GCP HostedCluster uses **either** CAPI/CAPG (via Hypershift `NodePool` resources) **or** Karpenter
(via `spec.autoNode`), never both simultaneously. When `spec.autoNode` is configured with Karpenter,
the Hypershift `NodePool` controller does not create CAPI `MachineDeployments` or
`GCPMachineTemplates`. Enabling Karpenter on an existing cluster with CAPI-managed nodes is a
migration: the user must drain and delete the Hypershift `NodePool` resources before (or after)
enabling Karpenter, which will provision replacement nodes via Karpenter `NodePool` resources in the
guest cluster. The plan does not cover an automated migration path -- this is a manual user action,
consistent with how the AWS integration works today.

### Real-World CR Examples

The following examples show how the three main CRs relate to each other in a production scenario.
The first two (`OpenshiftGCENodeClass` and `NodePool`) are created by the user in the guest cluster.
The third (`GCENodeClass`) is auto-generated by the karpenter-operator -- users never create or edit it.

#### 1. OpenshiftGCENodeClass (user creates in guest cluster)

```yaml
# This is what the user creates and manages.
# It defines GCP-specific node configuration.
# The karpenter-operator mirrors it to an upstream GCENodeClass
# with service-managed fields (RHCOS image, ignition, service account) injected.
apiVersion: karpenter.hypershift.openshift.io/v1
kind: OpenshiftGCENodeClass
metadata:
  name: general-purpose                # cluster-scoped, no namespace
spec:
  # OpenShift version for nodes provisioned with this class.
  # The karpenter-operator resolves this to a RHCOS GCE image from the release payload.
  version: "4.18.3"

  # Boot disk configuration
  disks:
    - boot: true
      sizeGiB: 128
      category: pd-balanced            # pd-standard, pd-ssd, pd-balanced, pd-extreme, hyperdisk-balanced, etc.

  # GCE network tags applied to instances (used for firewall rule targeting)
  networkTags:
    - allow-internal
    - allow-health-checks

  # GCE VM instance labels (for cost attribution, org policies, etc.)
  labels:
    team: platform
    cost-center: engineering

  # Custom instance metadata (key-value pairs set on the GCE instance)
  # Reserved keys (user-data, kube-env, startup-script, etc.) are blocked.
  metadata:
    enable-oslogin: "false"

  # Shielded VM settings
  shieldedInstanceConfig:
    enableSecureBoot: true
    enableVtpm: true
    enableIntegrityMonitoring: true

  # Kubelet overrides
  kubelet:
    maxPods: 110
    systemReserved:
      cpu: "100m"
      memory: "512Mi"
    kubeReserved:
      cpu: "200m"
      memory: "1Gi"
    evictionHard:
      memory.available: "100Mi"
      nodefs.available: "10%"
```

```yaml
# A second OpenshiftGCENodeClass for GPU workloads
apiVersion: karpenter.hypershift.openshift.io/v1
kind: OpenshiftGCENodeClass
metadata:
  name: gpu-nodes
spec:
  version: "4.18.3"
  disks:
    - boot: true
      sizeGiB: 256
      category: pd-ssd
  networkTags:
    - allow-internal
    - gpu-workloads
  labels:
    team: ml-infra
    workload-type: gpu
  gpuDriverVersion: "default"          # default, latest, or disabled
  autoGPUTaint: true                   # adds nvidia.com/gpu=present:NoSchedule
  shieldedInstanceConfig:
    enableSecureBoot: true
    enableVtpm: true
    enableIntegrityMonitoring: true
```

#### 2. NodePool (user creates in guest cluster)

```yaml
# General-purpose NodePool for mixed workloads.
# References the GCENodeClass by name -- the karpenter-operator ensures a
# GCENodeClass with this name exists (mirrored from OpenshiftGCENodeClass).
#
# NOTE: If PoC B (unified NodeClassRef UX) succeeds, the nodeClassRef group/kind
# will change to karpenter.hypershift.openshift.io/OpenshiftGCENodeClass.
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: general-purpose
spec:
  template:
    spec:
      nodeClassRef:
        group: karpenter.k8s.gcp       # upstream type (may change per PoC B)
        kind: GCENodeClass              # upstream type (may change per PoC B)
        name: general-purpose           # matches OpenshiftGCENodeClass name
      requirements:
        # Instance families -- restrict to cost-effective general-purpose types
        - key: karpenter.k8s.gcp/instance-family
          operator: In
          values: ["n4", "n2", "e2"]
        # Architecture
        - key: kubernetes.io/arch
          operator: In
          values: ["amd64"]
        # Capacity type -- allow both on-demand and spot for cost savings
        - key: karpenter.sh/capacity-type
          operator: In
          values: ["on-demand", "spot"]
        # Zones -- restrict to zones in the cluster's region
        - key: topology.kubernetes.io/zone
          operator: In
          values: ["us-central1-a", "us-central1-b", "us-central1-f"]
      # Nodes expire after 30 days (forces rotation for updates)
      expireAfter: 720h
  # Cluster-wide resource limits
  limits:
    cpu: "200"
    memory: "800Gi"
  # Disruption policy
  disruption:
    consolidationPolicy: WhenEmptyOrUnderutilized
    consolidateAfter: 30s
  # Weight for multi-NodePool priority (higher = preferred)
  weight: 50
```

```yaml
# GPU NodePool for ML training workloads.
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: gpu-training
spec:
  template:
    spec:
      nodeClassRef:
        group: karpenter.k8s.gcp
        kind: GCENodeClass
        name: gpu-nodes                 # matches the GPU OpenshiftGCENodeClass
      requirements:
        # GPU instance families only
        - key: karpenter.k8s.gcp/instance-family
          operator: In
          values: ["a2", "g2"]
        # Must have GPUs
        - key: karpenter.k8s.gcp/instance-gpu-count
          operator: Gt
          values: ["0"]
        - key: kubernetes.io/arch
          operator: In
          values: ["amd64"]
        # On-demand only for GPU (spot preemption is disruptive for training)
        - key: karpenter.sh/capacity-type
          operator: In
          values: ["on-demand"]
      expireAfter: 720h
  limits:
    nvidia.com/gpu: "16"
  disruption:
    consolidationPolicy: WhenEmpty
    consolidateAfter: 5m
  weight: 10
```

#### 3. GCENodeClass (auto-generated by karpenter-operator -- NOT user-created)

This is what the karpenter-operator creates in the guest cluster by mirroring the
`OpenshiftGCENodeClass` and injecting service-managed fields. Users cannot edit it (a VAP blocks
direct modification). Shown here for reference only.

```yaml
# AUTO-GENERATED by karpenter-operator. DO NOT EDIT.
# Mirrored from OpenshiftGCENodeClass "general-purpose" with service-managed fields injected.
apiVersion: karpenter.k8s.gcp/v1alpha1
kind: GCENodeClass
metadata:
  name: general-purpose                # same name as the OpenshiftGCENodeClass
  ownerReferences:
    - apiVersion: karpenter.hypershift.openshift.io/v1
      kind: OpenshiftGCENodeClass
      name: general-purpose
      uid: <uid>
spec:
  # --- SERVICE-MANAGED FIELDS (injected by karpenter-operator) ---

  # RHCOS image resolved from the OCP 4.18.3 release payload.
  # Users cannot control this -- it is determined by spec.version on the OpenshiftGCENodeClass.
  imageSelectorTerms:
    - id: projects/rhcos-cloud/global/images/rhcos-418-93-20260715-0-gcp-x86-64

  # Service account for the provisioned GCE instances.
  # From hcp.Spec.AutoNode.Provisioner.Karpenter.GCP.ServiceAccountEmail
  # or hcp.Spec.Platform.GCP.WorkloadIdentity.ServiceAccountEmails.NodePool
  serviceAccount: karpenter-node@my-project.iam.gserviceaccount.com

  # Ignition bootstrap metadata. The karpenter-operator injects the ignition pointer
  # config (ignition-server URL + bearer token) into the user-data key.
  # The upstream provider passes this through to the GCE instance metadata.
  metadata:
    user-data: |
      {"ignition":{"version":"3.2.0","config":{"merge":[{"source":"https://ignition-server.clusters-my-cluster.svc:443/ignition","httpHeaders":[{"name":"Authorization","value":"Bearer eyJ..."}]}]}}}
    enable-oslogin: "false"            # from user's OpenshiftGCENodeClass.spec.metadata

  # --- USER-CONFIGURED FIELDS (copied from OpenshiftGCENodeClass) ---

  disks:
    - boot: true
      sizeGiB: 128
      category: pd-balanced

  networkTags:
    - allow-internal
    - allow-health-checks

  # Merged: user labels + platform labels (cluster ID, infra ID)
  labels:
    team: platform
    cost-center: engineering
    goog-k8s-cluster-name: my-cluster
    hypershift-openshift-io-cluster: my-cluster
    hypershift-openshift-io-infra-id: my-cluster-abc123

  shieldedInstanceConfig:
    enableSecureBoot: true
    enableVtpm: true
    enableIntegrityMonitoring: true

  kubeletConfiguration:
    maxPods: 110
    systemReserved:
      cpu: "100m"
      memory: "512Mi"
    kubeReserved:
      cpu: "200m"
      memory: "1Gi"
    evictionHard:
      memory.available: "100Mi"
      nodefs.available: "10%"

status:
  # Resolved images (populated by the upstream karpenter-provider-gcp after image resolution)
  images:
    - sourceImage: projects/rhcos-cloud/global/images/rhcos-418-93-20260715-0-gcp-x86-64
      requirements:
        - key: kubernetes.io/arch
          operator: In
          values: ["amd64"]
  conditions:
    - type: Ready
      status: "True"
      lastTransitionTime: "2026-08-12T10:00:00Z"
```

#### Relationship Diagram

```
USER CREATES:                          KARPENTER-OPERATOR CREATES:

OpenshiftGCENodeClass                  GCENodeClass
"general-purpose"                      "general-purpose"
  spec:                                  spec:
    version: "4.18.3"        ──────>       imageSelectorTerms: [{id: rhcos-...}]
    disks: [pd-balanced]     ──────>       disks: [pd-balanced]
    networkTags: [...]       ──────>       networkTags: [...]
    labels: {team: ...}      ──merge──>    labels: {team: ..., goog-k8s-...: ...}
    metadata: {oslogin: ..}  ──merge──>    metadata: {user-data: <ignition>, oslogin: ..}
    kubelet: {maxPods: 110}  ──────>       kubeletConfiguration: {maxPods: 110}
                                           serviceAccount: karpenter-node@...  (injected)

NodePool "general-purpose"
  spec:
    nodeClassRef:
      name: general-purpose  ──refs──> GCENodeClass "general-purpose"
    requirements: [n4, n2, e2, spot+on-demand]
```

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
│                                  │          │    NodePool (refs GCENodeClass│
│  karpenter-provider-gcp          │          │      NOT OpenshiftGCENodeClass│
│  (upstream GCP binary)           │──guest──>│                              │
│  - watches NodePool/NodeClaim    │  API     │  Karpenter creates:          │
│  - provisions GCE instances      │          │    NodeClaim -> GCE Instance │
│  - manages instance lifecycle    │──GCP───> │                              │
│                                  │  API     │                              │
└──────────────────────────────────┘          └──────────────────────────────┘
```

### Complete CR Flow

```
User creates HostedCluster with Spec.AutoNode (platform: GCP)
  │
  ▼  hypershift-operator
HostedControlPlane (copies AutoNode spec)
  │
  ├──▶ ControlPlaneComponent "karpenter-operator" ──▶ Deployment + SA + Role + RoleBinding + PodMonitor
  │                                                    Secret "karpenter-credentials" (GCP WIF JSON)
  │
  │    karpenter-operator starts, runs 4 controllers:
  │
  │    [karpenter_controller]
  │      ├── installs CRDs in guest: nodepools.karpenter.sh, nodeclaims.karpenter.sh,
  │      │   gcenodeclasses.karpenter.k8s.gcp, openshiftgcenodeclasses.karpenter.hypershift.openshift.io
  │      ├── creates ConfigMap "set-karpenter-taint" (mgmt)
  │      ├── creates OpenshiftGCENodeClass "default" (guest)
  │      ├── creates ControlPlaneComponent "karpenter" ──▶ Deployment + SA + Role + RoleBinding + PodMonitor
  │      └── updates HCP.Status.AutoNode (node/nodeclaim counts, vCPUs)
  │
  │    [gce_nodeclass_controller]
  │      ├── watches OpenshiftGCENodeClass (guest), reads user-data Secret (mgmt)
  │      ├── creates upstream GCENodeClass (guest) with injected RHCOS image + ignition metadata
  │      ├── creates VAP + VAPBinding (guest) to protect GCENodeClass from direct edits
  │      └── syncs GCENodeClass.status back to OpenshiftGCENodeClass.status
  │
  │    [karpenterignition_controller]
  │      ├── watches OpenshiftGCENodeClass (guest)
  │      ├── resolves spec.version -> release image via Cincinnati
  │      ├── resolves release image -> RHCOS GCE image
  │      ├── creates Secret "token-<nc>-karpenter-<hash>" (mgmt) -- ignition token
  │      ├── creates Secret "user-data-<nc>-karpenter-<hash>" (mgmt) -- ignition payload + image IDs
  │      ├── creates ConfigMap "karpenter-kubelet-<nc>" (mgmt) -- per-NodeClass kubelet config
  │      └── updates OpenshiftGCENodeClass.status (version, conditions)
  │
  │    [machine_approver]
  │      └── watches CSRs (guest), validates against GCE instances via Compute API, approves
  │
  ▼  User creates in guest cluster:
OpenshiftGCENodeClass "my-class" ──(triggers controllers above)
NodePool (karpenter.sh/v1, refs GCENodeClass "my-class")
  │
  ▼  karpenter-provider-gcp (upstream, Deployment "karpenter" in HCP ns)
NodeClaim created ──▶ GCE instance provisioned ──▶ Node registers ──▶ CSR approved
  │
  ▼  hypershift-operator
HostedCluster.Status.AutoNode updated (node count, vCPUs)
```

---

## Phase 0: Proof-of-Concept and Dependency Alignment

**Goal**: Validate the critical design assumptions, get all three repos on compatible versions, and resolve risks before writing any Hypershift code.

The steps within Phase 0 must be executed in order: PoC A requires no fork (manual test), the
forks must exist before PoC B can run, and the go.mod integration comes last.

### 0.1 PoC A: RHCOS Boot via GCE Metadata (gate for entire project)

This PoC requires no fork -- it can be done manually against a stock GCE project.

1. Manually create a GCE instance using a RHCOS image with ignition delivered via the `user-data` metadata key
2. Confirm RHCOS reads and applies the ignition config from GCE metadata
3. Confirm the two-stage ignition flow works (small pointer config in metadata -> fetch full config from ignition-server)
4. Measure the metadata payload size to confirm it fits within GCE's 512 KB limit
5. Test with the unmodified upstream karpenter-provider-gcp: create a `GCENodeClass` with `imageSelectorTerms: [{id: "<rhcos-image-url>"}]` and `metadata: {user-data: "<ignition-json>"}`, verify the instance boots correctly

If this fails, the entire approach needs rethinking (e.g., using a startup-script that fetches ignition, or a custom image with embedded ignition support). **Do not proceed to 0.2 until this passes.**

### 0.2 Update OpenShift karpenter core fork to v1.14.0

- Rebase `github.com/openshift/kubernetes-sigs-karpenter` from v1.13.0 to v1.14.0
- Update the replace directive in Hypershift's `go.mod`:
  ```
  replace sigs.k8s.io/karpenter => github.com/openshift/kubernetes-sigs-karpenter v0.0.0-<new-date>-<new-commit>
  ```

### 0.3 Update OpenShift karpenter-provider-aws fork

- Rebase `github.com/openshift/aws-karpenter-provider-aws` against the upstream version that uses karpenter core v1.14.0
- Update the replace directive in Hypershift's `go.mod`

### 0.4 Create OpenShift fork of karpenter-provider-gcp

- Fork `github.com/cloudpilot-ai/karpenter-provider-gcp` to `github.com/openshift/karpenter-provider-gcp` (or equivalent)
- Ensure it compiles against the same OpenShift karpenter core fork
- **Add "external bootstrap" mode**: modify the fork to skip GKE bootstrap metadata injection when the `GCENodeClass.spec.metadata` contains a `user-data` key (or equivalent mechanism). This is required because OpenShift uses ignition, not GKE's `kube-env`/`configure-sh` bootstrap. See Risk 1 above.
- Add a replace directive in Hypershift's `go.mod`:
  ```
  github.com/cloudpilot-ai/karpenter-provider-gcp => github.com/openshift/karpenter-provider-gcp v0.0.0-<date>-<commit>
  ```

### 0.5 PoC B: Unified NodeClassRef UX (requires fork from 0.4)

Validate whether users can reference `OpenshiftGCENodeClass` directly in their NodePool `nodeClassRef`
instead of the upstream `GCENodeClass`. See "Design Consideration: Unified NodeClassRef UX" section
for full analysis. This PoC requires the fork from 0.4 to exist.

1. In the OpenShift fork, modify `GetSupportedNodeClasses()` to return both `GCENodeClass` and `OpenshiftGCENodeClass`
2. Register `OpenshiftGCENodeClass` in the provider's scheme
3. Create a test setup with both CRDs, create a NodePool referencing `OpenshiftGCENodeClass`, and verify:
   - The provider picks up the NodePool (passes `IsManaged()`)
   - `resolveNodeClassFromNodePool` resolves the `GCENodeClass` by name correctly
   - No errors from the core's watch setup, label generation, or field indexes
   - NodeClaim creation and instance provisioning work end-to-end
4. If successful, adopt the unified UX. If blocked, fall back to the AWS pattern (user references `GCENodeClass`).

### 0.6 Add the GCP provider dependency to Hypershift's go.mod

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

  **Design note:** `KarpenterGCPConfig` is intentionally thin. Fields like `projectID`, `region`,
  and WIF pool/provider configuration already exist on `GCPPlatformSpec` (at
  `hcp.Spec.Platform.GCP`). The karpenter-operator reads those from the platform spec at runtime
  (see Phase 4.1, 4.2, 4.3). Only fields that are **unique to Karpenter** and not shared with the
  platform belong here. Currently that is just the Karpenter-specific IAM service account. If a
  future use case requires Karpenter to target a different GCP project than the platform, a
  `projectID` override could be added here.
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
| `networkTags` | `[]string` | GCE network tags for firewall rules |
| `shieldedInstanceConfig` | `*ShieldedInstanceConfig` | Secure Boot, vTPM, Integrity Monitoring |
| `confidentialInstanceType` | `*string` | SEV, SEV_SNP, TDX |
| `labels` | `map[string]string` | GCE VM instance labels |
| `metadata` | `map[string]string` | Custom instance metadata (reserved keys blocked) |
| `version` | `string` | OpenShift version (resolved to release image via Cincinnati) |
| `kubelet` | `*KubeletConfiguration` | Kubelet settings (reuse from `kubelet_config.go`) |
| `autoGPUTaint` | `*bool` | Auto-apply nvidia.com/gpu taint |
| `gpuDriverVersion` | `*string` | default / latest / disabled |

**Note: No `imageSelectorTerms` field.** Unlike the upstream `GCENodeClass` (which selects GKE node
images like COS/Ubuntu), OpenShift uses RHCOS images. The RHCOS GCE image is resolved automatically
by the karpenter-operator from the OpenShift release payload based on `spec.version`. The
`imageSelectorTerms` on the upstream `GCENodeClass` is a **service-managed field** injected by the
GCENodeClass controller -- users cannot control it.

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
| `spec.networkTags` | `spec.networkTags` | Direct mapping |
| `spec.shieldedInstanceConfig` | `spec.shieldedInstanceConfig` | Direct mapping |
| `spec.confidentialInstanceType` | `spec.confidentialInstanceType` | Direct mapping |
| `spec.labels` | `spec.labels` | Merged with platform labels (cluster ID, infra ID) |
| `spec.metadata` | `spec.metadata` | **Merged with ignition bootstrap metadata** (see below) |
| `spec.kubelet` | `spec.kubeletConfiguration` | Type conversion |
| `spec.autoGPUTaint` | `spec.autoGPUTaint` | Direct mapping |
| `spec.gpuDriverVersion` | `spec.gpuDriverVersion` | Direct mapping |
| (service-managed) | `spec.imageSelectorTerms` | **RHCOS image from release payload** (not user-configurable) |
| (service-managed) | `spec.serviceAccount` | From HCP GCP platform config or KarpenterGCPConfig |

**Critical: Ignition injection into `spec.metadata`.**
The upstream `GCENodeClass` does not have a `spec.userData` field (unlike AWS `EC2NodeClass`).
Instead, GCE instances receive bootstrap data via instance metadata key-value pairs. The
GCENodeClass controller must inject the ignition payload into `spec.metadata` with the key
`user-data` (RHCOS on GCP reads ignition from this metadata key). The user's custom `spec.metadata`
entries are merged alongside, with reserved keys (including `user-data`) blocked from user input.

**Critical: Upstream bootstrap conflict.**
The upstream karpenter-provider-gcp's `metadata_bootstrap.go` constructs its own GKE-specific
bootstrap metadata (`kube-env`, `configure-sh`, `startup-script`, etc.) from a GKE node pool
template. For OpenShift, this GKE bootstrap must be **suppressed or overridden** because RHCOS
uses ignition, not GKE's bootstrap scripts. This likely requires changes to the upstream
karpenter-provider-gcp fork (Phase 0.3) to support an "external bootstrap" mode where the
operator fully controls the metadata and the provider does not inject its own bootstrap.

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
- **New method** `reconcileOpenshiftGCENodeClassDefault()`: creates a default `OpenshiftGCENodeClass` with GCP-appropriate defaults (pd-balanced boot disk, 120 GiB). Note: no `imageSelectorTerms` -- RHCOS images are service-managed by the ignition controller, not user-selected.
- Add `crdNodeOverlay` to the CRD list if not already present. The upstream karpenter core ships `karpenter.sh_nodeoverlays.yaml` -- confirm it is included in the existing CRD reconciliation loop (it likely already is, but must be verified for GCP parity)

### 3.3 GCP Machine Approver

**New file**: `karpenter-operator/controllers/karpenter/gcp_machine_approver.go`

Replace the EC2 `DescribeInstances` call with a GCE equivalent. Two options for DNS name validation:

**Option A (preferred): Derive DNS names from NodeClaim providerID without an API call.**
The GCP Karpenter provider sets `providerID` on NodeClaims in the format `gce://<project>/<zone>/<instance-name>`.
GCE internal DNS names follow the pattern `<instance-name>.<zone>.c.<project>.internal`.
The machine approver can parse the providerID from existing NodeClaims (already fetched for the AWS
implementation at `machine_approver.go:162-164`) and construct the expected DNS name without calling
the Compute API. This is faster, avoids rate limits, and requires no cloud credentials.

**Option B: Call the Compute API.**
Use `compute.Instances.AggregatedList()` filtered by Karpenter cluster labels to get instance
details including network interfaces and DNS names. This is more robust (handles edge cases where
DNS names don't follow the standard pattern) but requires Compute API access and credentials.

```go
func (r *GCPMachineApproverController) getExpectedDNSNames(ctx context.Context, nodeClaims []*karpenterv1.NodeClaim) ([]string, error) {
    var dnsNames []string
    for _, nc := range nodeClaims {
        if nc.Status.ProviderID == "" {
            continue
        }
        // Parse gce://<project>/<zone>/<instance-name>
        project, zone, name := parseGCEProviderID(nc.Status.ProviderID)
        // GCE internal DNS: <name>.<zone>.c.<project>.internal
        dnsNames = append(dnsNames, fmt.Sprintf("%s.%s.c.%s.internal", name, zone, project))
    }
    return dnsNames, nil
}
```

Start with Option A. Fall back to Option B only if DNS name derivation proves unreliable in testing.

### 3.4 Extend Ignition Controller for GCP

**File**: `karpenter-operator/controllers/karpenterignition/karpenterignition_controller.go`

This controller is ~95% provider-neutral. The challenge is that controller-runtime's `For()` accepts
a single primary type. We need to support both `OpenshiftEC2NodeClass` and `OpenshiftGCENodeClass`.

**Recommended approach: two controller instances, one shared reconciler.**

Create a `NodeClassAccessor` interface in `api/karpenter/v1/`:
```go
type NodeClassAccessor interface {
    client.Object
    GetVersion() string
    GetKubelet() *KubeletConfiguration
    GetName() string
}
```

Both `OpenshiftEC2NodeClass` and `OpenshiftGCENodeClass` implement this interface (add methods to
each type).

Refactor the reconciler to be generic over `NodeClassAccessor`:
```go
type KarpenterIgnitionReconciler[T NodeClassAccessor] struct {
    // ... existing fields
    NewNodeClass func() T  // factory: returns &OpenshiftEC2NodeClass{} or &OpenshiftGCENodeClass{}
}
```

In `main.go`, instantiate the appropriate controller based on platform:
```go
switch platform {
case hyperv1.AWSPlatform:
    ignReconciler := &KarpenterIgnitionReconciler[*hyperkarpenterv1.OpenshiftEC2NodeClass]{
        NewNodeClass: func() *hyperkarpenterv1.OpenshiftEC2NodeClass { return &hyperkarpenterv1.OpenshiftEC2NodeClass{} },
        // ...
    }
case hyperv1.GCPPlatform:
    ignReconciler := &KarpenterIgnitionReconciler[*hyperkarpenterv1.OpenshiftGCENodeClass]{
        NewNodeClass: func() *hyperkarpenterv1.OpenshiftGCENodeClass { return &hyperkarpenterv1.OpenshiftGCENodeClass{} },
        // ...
    }
}
```

This avoids duplicating the controller code while keeping `For()` typed correctly. The
karpenter-operator binary already knows its platform at startup (from the HCP), so only one
controller instance is created per binary invocation -- never both simultaneously.

**Alternative (simpler, acceptable):** Since the karpenter-operator only runs for one platform at a
time, the simplest approach is to duplicate the controller (`gcp_karpenterignition_controller.go`)
with `OpenshiftGCENodeClass` hardcoded. This is more code but avoids generics complexity. The AWS
and GCP controllers would share helper functions but have separate `Reconcile()` and
`SetupWithManager()` methods.

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

The existing `assets/karpenter/deployment.yaml` is AWS-specific (hardcoded `AWS_SHARED_CREDENTIALS_FILE`,
`AWS_SDK_LOAD_CONFIG` env vars, `provider-creds` volume).

**Decision: make the YAML cloud-neutral.** Remove all cloud-specific env vars, volume mounts, and
volumes from the YAML template. Move them entirely into the Go `adaptDeployment` function
(`karpenter/deployment.go`), which already has a platform switch. This avoids maintaining separate
YAML files per platform and eliminates drift risk. The YAML will contain only cloud-neutral fields:
replicas, labels, kubeconfig volume, serviceaccount-token volume, health probes, ports, and
resource requests.

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
| **Phase 0** (PoC + Dependencies) | **Large** -- includes PoC validation + 3 fork rebases | External (fork management) | No -- must be first, PoC is a gate |
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

## Design Consideration: Unified NodeClassRef UX

### The Problem

In the current AWS Karpenter integration, users face a confusing indirection: they create an
`OpenshiftEC2NodeClass` but must reference the upstream `EC2NodeClass` (a different API group and
kind) in their `NodePool.spec.template.spec.nodeClassRef`. The names match by convention, but the
types differ. Users cannot touch `EC2NodeClass` directly (a VAP blocks it), yet they must reference
it. This is a known UX friction point.

### Proposed Improvement for GCP

We will investigate whether users can reference `OpenshiftGCENodeClass` directly in their NodePool,
eliminating the need to know about the upstream `GCENodeClass` type:

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
spec:
  template:
    spec:
      nodeClassRef:
        group: karpenter.hypershift.openshift.io      # the OpenShift type
        kind: OpenshiftGCENodeClass                    # the OpenShift type
        name: default                                  # the resource they created
```

Instead of the current pattern:
```yaml
      nodeClassRef:
        group: karpenter.k8s.gcp                      # upstream type (user can't edit)
        kind: GCENodeClass                             # upstream type (user can't edit)
        name: default                                  # must match OpenshiftGCENodeClass name
```

### How This Could Work

Analysis of the karpenter core's `nodeClassRef` resolution reveals a feasible path:

1. **`IsManaged()` is the only gate.** The core's `IsManaged()` predicate
   (`pkg/utils/nodepool/nodepool.go:37-41`) compares `nodeClassRef.GroupKind()` against the types
   returned by `CloudProvider.GetSupportedNodeClasses()`. If the GroupKind is not in the list, the
   NodePool is silently ignored -- not rejected.

2. **The provider only uses `Name` for resolution.** The `resolveNodeClassFromNodePool()` and
   `resolveNodeClassFromNodeClaim()` methods in the GCP provider (`cloudprovider.go:221-230,
   376-386`) hardcode the target Go type (`*v1alpha1.GCENodeClass{}`) and only read
   `nodeClassRef.Name`. They do not re-check `Group` or `Kind`.

3. **Adding `OpenshiftGCENodeClass` to `GetSupportedNodeClasses()` opens the gate.** In the
   OpenShift fork of karpenter-provider-gcp, changing `GetSupportedNodeClasses()` to return both
   types would make `IsManaged()` accept NodePools referencing either:

   ```go
   func (c *CloudProvider) GetSupportedNodeClasses() []status.Object {
       return []status.Object{
           &v1alpha1.GCENodeClass{},
           &openshiftv1.OpenshiftGCENodeClass{},  // added
       }
   }
   ```

   The `resolveNodeClassFrom*` methods continue to work unchanged -- they fetch `GCENodeClass` by
   name regardless of what `nodeClassRef.Group`/`Kind` say, and the karpenter-operator guarantees
   a `GCENodeClass` with the same name exists for every `OpenshiftGCENodeClass`.

### Side Effects to Evaluate

Adding `OpenshiftGCENodeClass` to `GetSupportedNodeClasses()` has ripple effects in the core:

| Core usage of `GetSupportedNodeClasses()` | Effect |
|---|---|
| `IsManaged()` predicate filtering | Intended: accepts both types |
| Controller watch setup (readiness, disruption, nodeoverlay) | Core will set up watches on `OpenshiftGCENodeClass` in addition to `GCENodeClass`. This is harmless but means both types must be registered in the scheme. |
| `NodeClassLabelKey()` label generation | Core may generate labels like `karpenter.hypershift.openshift.io/openshiftgcenodeclass: default` on NodeClaims/Nodes. Need to verify this doesn't conflict. |
| `ForNodeClass()` field index lookups | May attempt to list NodeClaims/NodePools by `OpenshiftGCENodeClass` group/kind. Need to verify correctness when the actual stored `nodeClassRef` uses the OpenShift group/kind but the underlying `GCENodeClass` is what the provider fetches. |
| Status metrics controllers | May create metrics labels for both types. Minor, likely harmless. |

### Alternative Approaches

| Approach | Description | Pros | Cons |
|---|---|---|---|
| **A. Extend `GetSupportedNodeClasses()`** (preferred) | Add `OpenshiftGCENodeClass` to the list in the OpenShift fork | Clean UX, minimal code change, user references the type they own | Side effects on watches/labels/indexes need validation |
| **B. Mutating webhook** | Karpenter-operator installs a webhook that rewrites `nodeClassRef` from `OpenshiftGCENodeClass` -> `GCENodeClass` on NodePool create | No fork change needed | Webhook complexity, interaction with CEL immutability rules (`nodeClassRef.group is immutable`), webhook availability during cluster bootstrap |
| **C. Keep the AWS pattern** | User references `GCENodeClass` in NodePool (current AWS behavior) | Consistency with AWS, no fork change | Confusing UX: user creates one type, references another |

### Plan

We will include a **proof-of-concept** in Phase 0 to assess the viability of Approach A:

1. In the OpenShift fork of karpenter-provider-gcp, modify `GetSupportedNodeClasses()` to return
   both `GCENodeClass` and `OpenshiftGCENodeClass`
2. Register `OpenshiftGCENodeClass` in the provider's scheme
3. Create a test cluster with both CRDs installed, create a NodePool referencing
   `OpenshiftGCENodeClass`, and verify:
   - The upstream provider correctly picks up the NodePool (passes `IsManaged()`)
   - `resolveNodeClassFromNodePool` correctly resolves the `GCENodeClass` by name
   - No errors from the core's watch setup, label generation, or field indexes
   - NodeClaim creation and instance provisioning work end-to-end
4. If the PoC succeeds, adopt Approach A for the GCP implementation
5. If the PoC reveals blocking issues, fall back to Approach C (the AWS pattern)

---

## Key Risks

### Risk 1 (Critical): OpenShift Node Bootstrap on GCP

The GCP Karpenter provider's bootstrap mechanism (`karpenter-provider-gcp/pkg/providers/instance/metadata_bootstrap.go`) is designed for standard GKE nodes using `kube-env` metadata for kubelet configuration. OpenShift nodes use **ignition-based provisioning** with MachineConfig. These two bootstrap mechanisms **conflict**.

**The problem in detail:**

The upstream karpenter-provider-gcp does the following when creating a GCE instance:
1. Reads a GKE node pool template (the "bootstrap pool")
2. Extracts GKE-specific metadata keys (`kube-env`, `configure-sh`, `startup-script`, `containerd-configure-sh`, etc.)
3. Merges user-provided `spec.metadata` with these GKE bootstrap keys
4. Creates the GCE instance with the combined metadata

For OpenShift, we need:
1. **RHCOS image** instead of COS/Ubuntu -- the karpenter-operator resolves this from the release payload
2. **Ignition config** in the `user-data` metadata key -- not GKE's `kube-env`/`configure-sh`
3. **No GKE bootstrap** -- the `startup-script`, `kube-env`, and other GKE keys must not be present

**The AWS integration avoids this problem** because the upstream karpenter-provider-aws has a clean `spec.userData` field that the operator controls entirely. The GCP provider has no such field -- it constructs metadata from a GKE template.

**Resolution options:**

| Option | Description | Effort |
|---|---|---|
| A. Fork modification (recommended) | In the OpenShift fork of karpenter-provider-gcp (Phase 0.3), add an "external bootstrap" mode. When `spec.metadata` contains a `user-data` key (or a flag is set), skip the GKE bootstrap template entirely and pass metadata through as-is. | Medium -- upstream fork change |
| B. Metadata override | Let the GKE bootstrap run but ensure the operator's `user-data` key takes precedence. RHCOS ignores `kube-env`/`startup-script`. Risk: extra unnecessary metadata, possible size limits. | Small -- but fragile |
| C. Upstream PR | Contribute an "external bootstrap" feature upstream to karpenter-provider-gcp. | Large -- requires upstream alignment |

**This must be validated in Phase 0 as a proof-of-concept** before any other work begins.

### Risk 2 (High): RHCOS Image Resolution on GCP

OpenShift uses RHCOS images, not GKE node images (COS/Ubuntu). The upstream `GCENodeClass.spec.imageSelectorTerms` expects GKE image families and channels. For OpenShift:

- The karpenter-operator must resolve the RHCOS GCE image from the OpenShift release payload (same pattern as AWS AMI resolution)
- The resolved RHCOS image ID must be injected into `GCENodeClass.spec.imageSelectorTerms` as an `id`-based selector (bypassing the upstream family/channel resolution)
- The upstream provider must accept and use this `id`-based image selector without trying to resolve it through GKE image families

The AWS integration handles this with `AMISelectorTerms` containing specific AMI IDs labeled on the user-data Secret. The GCP equivalent would use `imageSelectorTerms` with `id` set to the full RHCOS image URL (e.g., `projects/<project>/global/images/<rhcos-image>`).

**Validation needed:** Confirm that the upstream karpenter-provider-gcp correctly handles `imageSelectorTerms` with only an `id` field (no `family` or `channel`). The upstream CRD validation allows this (`exactly one of alias, id, or family`), but the image resolution code path needs verification.

### Risk 3 (Medium): GCE Instance Metadata Size Limits

GCE instance metadata has a **512 KB total limit** across all key-value pairs. OpenShift ignition configs can be large (especially with custom MachineConfigs). The AWS integration avoids this because EC2 user-data supports up to 16 KB (compressed), and larger configs are fetched from the ignition-server via a pointer.

The existing Hypershift ignition flow uses a **two-stage approach**: the user-data contains a small ignition config with just the ignition-server URL and a bearer token. The node fetches the full config from the ignition-server at boot. This two-stage approach should work within GCE metadata limits, but it needs validation.

---

## Open Questions

1. **Upstream fork bootstrap suppression**: What is the cleanest way to suppress GKE bootstrap in the karpenter-provider-gcp fork? A feature gate? A sentinel metadata key? Removing the bootstrap pool dependency entirely?

2. **RHCOS image location**: Where are RHCOS GCE images published? Are they in a public GCP project (like `rhcos-cloud`)? How does the release payload reference them? The ignition controller needs to extract the GCE image ID from the release image metadata.

3. **Feature gate**: Should GCP Karpenter be behind its own feature gate (e.g., `KarpenterGCP`) or share the existing `KarpenterOperator` + `GCPPlatform` gates?

4. **Network config**: The default `OpenshiftGCENodeClass` needs sensible defaults for the GCP subnetwork. Should it inherit from the HostedCluster's GCP platform config (PSC subnet, primary subnet)?

5. **Service account**: Should the Karpenter node service account be the same as the one used by the HostedCluster's CAPG node pool, or a separate dedicated one?

6. **`karpenter-subnets` ConfigMap**: The AWS integration creates a `karpenter-subnets` ConfigMap aggregating subnet IDs for VPC endpoint management. Is there a GCP equivalent need? GCP uses Private Service Connect (PSC), not VPC endpoints. This ConfigMap may not be needed for GCP, or it may need a different form (subnetwork self-links).

7. **NodePool `nodeClassRef` UX**: See the "Design Consideration: Unified NodeClassRef UX" section above. A PoC will determine whether users can reference `OpenshiftGCENodeClass` directly or must use the upstream `GCENodeClass` type.

---

## Upstream Contribution Strategy

The OpenShift fork of `karpenter-provider-gcp` (Phase 0.4) will carry at least two modifications:

1. **External bootstrap mode** -- suppresses GKE bootstrap metadata when `user-data` is present
2. **Extended `GetSupportedNodeClasses()`** -- returns `OpenshiftGCENodeClass` alongside `GCENodeClass` (if PoC B succeeds)

**Strategy:**

| Change | Upstream viability | Timeline |
|---|---|---|
| External bootstrap mode | **High.** This is a general-purpose feature useful to anyone running non-GKE workloads (e.g., Talos, Flatcar, custom images). Propose upstream as a "custom bootstrap" or "external userData" feature. | Propose upstream PR within 1 month of fork creation. If accepted, removes fork maintenance burden for this change. |
| Extended `GetSupportedNodeClasses()` | **Low.** This is OpenShift-specific (the upstream provider has no reason to know about `OpenshiftGCENodeClass`). Keep as a fork-only change. | No upstream PR. Maintain in fork indefinitely. |

**Fork maintenance plan:**
- The fork will be rebased against upstream releases on a monthly cadence (matching the upstream's release schedule)
- Automated CI (dependabot or equivalent) will track upstream dependency updates
- The external bootstrap change should be kept as a minimal, well-isolated patch to minimize rebase conflicts

---

## Rough Timeline Estimates

Assumes 2 engineers working in parallel after Phase 0/1 complete.

| Phase | Calendar time | FTE-weeks | Notes |
|---|---|---|---|
| **Phase 0** (PoC + Dependencies) | 3-4 weeks | 4-6 | PoC A: ~3 days. Fork rebases: ~1 week each (3 forks). PoC B: ~3 days. go.mod integration: ~2 days. Calendar time dominated by sequential fork work + iteration. |
| **Phase 1** (API Types) | 1-2 weeks | 2-3 | CRD design, code generation, client generation. Requires design review before proceeding. |
| **Phase 2** (Support Utils) | 2-3 days | 0.5 | Small, well-scoped changes to existing files. |
| **Phase 3** (Operator Controllers) | 4-6 weeks | 6-8 | GCENodeClass controller (~2 weeks), machine approver (~1 week), ignition controller extension (~1-2 weeks), main controller changes (~1 week). This is the bulk of the work. |
| **Phase 4** (CPO Components) | 1-2 weeks | 2-3 | Deployment adaptation, credential generation, image plumbing. Can run in parallel with Phase 3. |
| **Phase 5** (HC Controller) | 1-2 days | 0.3 | Mostly no-ops due to good abstractions. |
| **Phase 6** (CLI) | 2-3 days | 0.5 | Flag additions, IAM binding updates. |
| **Phase 7** (Testing) | 2-3 weeks | 3-4 | Unit tests with each phase (~1 week cumulative). E2E test development and stabilization (~2 weeks). |
| **Total** | **~12-16 weeks** | **~19-26 FTE-weeks** | Phases 3+4 run in parallel. E2E stabilization often takes longer than estimated. |

**Critical path:** Phase 0 (PoC gate) -> Phase 1 (API types) -> Phase 3 (operator controllers) -> Phase 7 (E2E tests).

Phases 2, 4, 5, 6 can be done in parallel with Phase 3 once Phase 1 is complete.
