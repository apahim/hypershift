# GCP Platform Examples

This directory contains example manifests for deploying HyperShift HostedClusters on Google Cloud Platform (GCP).

## Prerequisites

### 1. GCP Infrastructure
- GCP project with billing enabled
- VPC network configured for Private Service Connect
- Appropriate IAM permissions for CAPG

### 2. Workload Identity Setup
HyperShift GCP integration uses Workload Identity for authentication:

```bash
# Enable Workload Identity on your GKE management cluster
gcloud container clusters update CLUSTER_NAME \
    --workload-pool=PROJECT_ID.svc.id.goog

# Create GCP service account for CAPG
gcloud iam service-accounts create capg-controller \
    --project=PROJECT_ID

# Grant necessary permissions
gcloud projects add-iam-policy-binding PROJECT_ID \
    --member="serviceAccount:capg-controller@PROJECT_ID.iam.gserviceaccount.com" \
    --role="roles/compute.instanceAdmin.v1"

gcloud projects add-iam-policy-binding PROJECT_ID \
    --member="serviceAccount:capg-controller@PROJECT_ID.iam.gserviceaccount.com" \
    --role="roles/iam.serviceAccountUser"

# Bind Kubernetes ServiceAccount to GCP ServiceAccount
gcloud iam service-accounts add-iam-policy-binding \
    --role roles/iam.workloadIdentityUser \
    --member "serviceAccount:PROJECT_ID.svc.id.goog[NAMESPACE/capi-provider]" \
    capg-controller@PROJECT_ID.iam.gserviceaccount.com
```

### 3. Required Secrets

Create pull secret and SSH key:

```bash
# Create pull secret
kubectl create secret generic pull-secret \
  --from-file=.dockerconfigjson=$HOME/.docker/config.json \
  --type=kubernetes.io/dockerconfigjson \
  --namespace=clusters

# Create SSH key secret
kubectl create secret generic ssh-key \
  --from-file=id_rsa.pub=$HOME/.ssh/id_rsa.pub \
  --namespace=clusters
```

## Usage

### 1. Create HostedCluster

Edit `hostedcluster.yaml` with your GCP project and network details:

```bash
# Update the configuration
vim hostedcluster.yaml

# Apply the HostedCluster
kubectl apply -f hostedcluster.yaml
```

### 2. Create NodePool

Edit `nodepool.yaml` with your desired instance configuration:

```bash
# Update the configuration
vim nodepool.yaml

# Apply the NodePool
kubectl apply -f nodepool.yaml
```

### 3. Monitor Deployment

```bash
# Watch HostedCluster status
kubectl get hostedcluster example-gcp-cluster -w

# Check GCPCluster creation (our CAPI infrastructure)
kubectl get gcpcluster -A

# Watch NodePool provisioning
kubectl get nodepool example-gcp-workers -w

# Check CAPG resources
kubectl get gcpmachine,gcpmachinetemplate -A
```

## Architecture

The GCP platform integration creates the following resources:

1. **GCPCluster**: Infrastructure template in management cluster namespace
2. **GCPMachineTemplate**: Worker node template for CAPG
3. **CAPI Machine Resources**: MachineDeployment → MachineSet → Machine
4. **GCP VM Instances**: Actual compute instances via CAPG

## Troubleshooting

### Common Issues

1. **Scheme Error**: Ensure CAPG types are registered in HyperShift operator
2. **Authentication**: Verify Workload Identity setup is correct
3. **Network**: Confirm VPC and PSC subnet configuration
4. **Permissions**: Check GCP IAM permissions for service accounts

### Useful Commands

```bash
# Check operator logs
kubectl logs -n hypershift deployment/operator -f

# Check CAPG controller logs
kubectl logs -n clusters-example-gcp-cluster deployment/capi-gcp-controller-manager -f

# Inspect GCPCluster configuration
kubectl get gcpcluster example-gcp-cluster -n clusters-example-gcp-cluster -o yaml
```