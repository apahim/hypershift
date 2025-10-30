package gcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"golang.org/x/crypto/ssh"
	"google.golang.org/api/compute/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"

	gcputil "github.com/openshift/hypershift/cmd/infra/gcp/util"
	"github.com/openshift/hypershift/cmd/util"
)

// ComputeManager handles GCP Compute Engine operations for proxy VMs
type ComputeManager struct {
	clients   *gcputil.GCPClients
	projectID string
	region    string
	logger    logr.Logger
}

// ProxyVMDetails contains information about the created proxy VM
type ProxyVMDetails struct {
	Name           string `json:"name"`
	PrivateIP      string `json:"privateIP"`
	PublicIP       string `json:"publicIP,omitempty"`
	HTTPProxyURL   string `json:"httpProxyURL"`
	HTTPSProxyURL  string `json:"httpsProxyURL,omitempty"`
	PrivateSSHKey  string `json:"privateSSHKey"`
	CA             string `json:"ca,omitempty"`
	Zone           string `json:"zone"`
	MachineType    string `json:"machineType"`
}

// NewComputeManager creates a new compute manager
func NewComputeManager(clients *gcputil.GCPClients, projectID, region string, logger logr.Logger) *ComputeManager {
	return &ComputeManager{
		clients:   clients,
		projectID: projectID,
		region:    region,
		logger:    logger,
	}
}

// CreateProxyVM creates a proxy VM in the specified subnet with appropriate firewall rules
func (cm *ComputeManager) CreateProxyVM(ctx context.Context, infraID, subnetName, firewallTagName string, isSecure bool, additionalLabels map[string]string) (*ProxyVMDetails, error) {
	cm.logger.Info("Creating proxy VM", "infraID", infraID, "subnet", subnetName, "secure", isSecure)

	// Generate SSH key pair for VM access
	publicSSHKey, privateSSHKey, err := util.GenerateSSHKeys()
	if err != nil {
		return nil, fmt.Errorf("failed to generate SSH keys: %w", err)
	}

	// Get the first zone in the region for VM placement
	zone, err := cm.getFirstZoneInRegion(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get zone: %w", err)
	}

	// Create the VM instance
	instanceName := fmt.Sprintf("%s-proxy", infraID)
	instance, err := cm.createInstance(ctx, instanceName, zone, subnetName, firewallTagName, isSecure, string(publicSSHKey), additionalLabels)
	if err != nil {
		return nil, fmt.Errorf("failed to create proxy instance: %w", err)
	}

	// Wait for the instance to be running
	err = cm.waitForInstanceRunning(ctx, zone, instanceName)
	if err != nil {
		return nil, fmt.Errorf("failed to wait for instance to be running: %w", err)
	}

	// Get instance details
	privateIP := instance.NetworkInterfaces[0].NetworkIP
	var publicIP string
	if len(instance.NetworkInterfaces[0].AccessConfigs) > 0 {
		publicIP = instance.NetworkInterfaces[0].AccessConfigs[0].NatIP
	}

	result := &ProxyVMDetails{
		Name:          instanceName,
		PrivateIP:     privateIP,
		PublicIP:      publicIP,
		HTTPProxyURL:  fmt.Sprintf("http://%s:3128", privateIP),
		PrivateSSHKey: string(privateSSHKey),
		Zone:          zone,
		MachineType:   "e2-micro", // Default machine type
	}

	// For secure proxy, fetch the CA certificate
	if isSecure {
		if publicIP == "" {
			return nil, fmt.Errorf("secure proxy requires public IP for CA certificate retrieval")
		}

		ca, err := cm.fetchProxyCA(publicIP, privateIP, string(privateSSHKey))
		if err != nil {
			return nil, fmt.Errorf("failed to fetch proxy CA: %w", err)
		}

		result.HTTPSProxyURL = fmt.Sprintf("https://%s:3128", privateIP)
		result.CA = ca
	}

	cm.logger.Info("Successfully created proxy VM", "name", instanceName, "privateIP", privateIP, "publicIP", publicIP)
	return result, nil
}

// createInstance creates a GCP Compute Engine instance for the proxy
func (cm *ComputeManager) createInstance(ctx context.Context, instanceName, zone, subnetName, firewallTag string, isSecure bool, publicSSHKey string, additionalLabels map[string]string) (*compute.Instance, error) {
	// Prepare startup script
	startupScript := proxyConfigScript(isSecure, publicSSHKey)
	encodedScript := base64.StdEncoding.EncodeToString([]byte(startupScript))

	// Prepare labels
	labels := make(map[string]string)
	for k, v := range additionalLabels {
		labels[k] = v
	}
	labels["hypershift-proxy"] = "true"

	// Determine if we need external IP (required for secure proxy CA retrieval)
	var accessConfigs []*compute.AccessConfig
	if isSecure {
		accessConfigs = []*compute.AccessConfig{
			{
				Type: "ONE_TO_ONE_NAT",
				Name: "External NAT",
			},
		}
	}

	// Create instance configuration
	instance := &compute.Instance{
		Name:        instanceName,
		MachineType: fmt.Sprintf("zones/%s/machineTypes/e2-micro", zone),
		Labels:      labels,
		Tags: &compute.Tags{
			Items: []string{firewallTag},
		},
		Disks: []*compute.AttachedDisk{
			{
				Boot:       true,
				AutoDelete: true,
				InitializeParams: &compute.AttachedDiskInitializeParams{
					SourceImage: "projects/centos-cloud/global/images/family/centos-7",
					DiskSizeGb:  20,
					DiskType:    fmt.Sprintf("zones/%s/diskTypes/pd-standard", zone),
				},
			},
		},
		NetworkInterfaces: []*compute.NetworkInterface{
			{
				Subnetwork:    fmt.Sprintf("projects/%s/regions/%s/subnetworks/%s", cm.projectID, cm.region, subnetName),
				AccessConfigs: accessConfigs,
			},
		},
		Metadata: &compute.Metadata{
			Items: []*compute.MetadataItems{
				{
					Key:   "startup-script",
					Value: &encodedScript,
				},
				{
					Key:   "enable-oslogin",
					Value: stringPtr("FALSE"),
				},
			},
		},
		ServiceAccounts: []*compute.ServiceAccount{
			{
				Email: "default",
				Scopes: []string{
					"https://www.googleapis.com/auth/devstorage.read_only",
					"https://www.googleapis.com/auth/logging.write",
					"https://www.googleapis.com/auth/monitoring.write",
				},
			},
		},
	}

	// Insert the instance
	operation, err := cm.clients.Compute.Instances.Insert(cm.projectID, zone, instance).Context(ctx).Do()
	if err != nil {
		return nil, gcputil.HandleResourceCreationError(err, "proxy instance", instanceName)
	}

	// Wait for the operation to complete
	err = cm.waitForZoneOperation(ctx, zone, operation.Name, fmt.Sprintf("proxy instance %s creation", instanceName))
	if err != nil {
		return nil, fmt.Errorf("failed waiting for instance creation: %w", err)
	}

	// Get the created instance
	createdInstance, err := cm.clients.Compute.Instances.Get(cm.projectID, zone, instanceName).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("failed to get created instance: %w", err)
	}

	return createdInstance, nil
}

// getFirstZoneInRegion returns the first available zone in the specified region
func (cm *ComputeManager) getFirstZoneInRegion(ctx context.Context) (string, error) {
	zoneList, err := cm.clients.Compute.Zones.List(cm.projectID).Filter(fmt.Sprintf("region eq .*%s$", cm.region)).Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("failed to list zones: %w", err)
	}

	if len(zoneList.Items) == 0 {
		return "", fmt.Errorf("no zones found in region %s", cm.region)
	}

	// Return the first zone
	return zoneList.Items[0].Name, nil
}

// waitForInstanceRunning waits for the instance to reach RUNNING state
func (cm *ComputeManager) waitForInstanceRunning(ctx context.Context, zone, instanceName string) error {
	cm.logger.Info("Waiting for instance to be running", "instance", instanceName, "zone", zone)

	return wait.PollUntilContextTimeout(ctx, 10*time.Second, 10*time.Minute, true, func(ctx context.Context) (bool, error) {
		instance, err := cm.clients.Compute.Instances.Get(cm.projectID, zone, instanceName).Context(ctx).Do()
		if err != nil {
			return false, fmt.Errorf("failed to get instance status: %w", err)
		}

		cm.logger.V(1).Info("Instance status", "instance", instanceName, "status", instance.Status)

		if instance.Status == "RUNNING" {
			return true, nil
		}

		if instance.Status == "TERMINATED" || instance.Status == "STOPPING" {
			return false, fmt.Errorf("instance %s entered unexpected state: %s", instanceName, instance.Status)
		}

		return false, nil
	})
}

// waitForZoneOperation waits for a zone operation to complete
func (cm *ComputeManager) waitForZoneOperation(ctx context.Context, zone, operationName, description string) error {
	cm.logger.Info("Waiting for zone operation", "operation", operationName, "description", description)

	return wait.PollUntilContextTimeout(ctx, 5*time.Second, 10*time.Minute, true, func(ctx context.Context) (bool, error) {
		operation, err := cm.clients.Compute.ZoneOperations.Get(cm.projectID, zone, operationName).Context(ctx).Do()
		if err != nil {
			return false, fmt.Errorf("failed to get operation status: %w", err)
		}

		cm.logger.V(1).Info("Operation status", "operation", operationName, "status", operation.Status)

		if operation.Status == "DONE" {
			if operation.Error != nil {
				return false, fmt.Errorf("operation failed: %v", operation.Error)
			}
			return true, nil
		}

		return false, nil
	})
}

// fetchProxyCA fetches the CA certificate from a secure proxy using SSH
func (cm *ComputeManager) fetchProxyCA(publicIP, privateIP, privateSSHKey string) (string, error) {
	cm.logger.Info("Fetching proxy CA certificate", "publicIP", publicIP, "privateIP", privateIP)

	// Implement retry mechanism to wait for mitmproxy to be ready
	backoff := wait.Backoff{
		Steps:    10,
		Duration: 30 * time.Second,
		Factor:   1.2,
		Jitter:   0.1,
	}

	var proxyCA string
	err := retry.OnError(backoff, func(error) bool { return true }, func() error {
		cm.logger.V(1).Info("Attempting to fetch proxy CA via SSH", "publicIP", publicIP)

		// Parse the private SSH key
		signer, err := ssh.ParsePrivateKey([]byte(privateSSHKey))
		if err != nil {
			return fmt.Errorf("unable to parse private SSH key: %w", err)
		}

		// Create SSH client configuration
		// Using 'centos' user for CentOS/RHEL images on GCP
		config := &ssh.ClientConfig{
			User: "centos",
			Auth: []ssh.AuthMethod{
				ssh.PublicKeys(signer),
			},
			HostKeyCallback: ssh.InsecureIgnoreHostKey(),
			Timeout:         30 * time.Second,
		}

		// Connect to the VM via SSH
		address := fmt.Sprintf("%s:22", publicIP)
		client, err := ssh.Dial("tcp", address, config)
		if err != nil {
			return fmt.Errorf("failed to dial SSH to %s: %w", address, err)
		}
		defer client.Close()

		// Create SSH session
		session, err := client.NewSession()
		if err != nil {
			return fmt.Errorf("failed to create SSH session: %w", err)
		}
		defer session.Close()

		// Use mitmproxy's built-in endpoint to get the CA certificate
		// This is the same approach as AWS implementation
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		session.Stdout = &stdout
		session.Stderr = &stderr

		// Curl the mitmproxy CA endpoint through the proxy itself
		curlCmd := fmt.Sprintf("curl -s -x http://%s:3128 http://mitm.it/cert/pem", privateIP)
		cm.logger.V(1).Info("Running CA retrieval command", "command", curlCmd)

		if err := session.Run(curlCmd); err != nil {
			stderrStr := stderr.String()
			return fmt.Errorf("failed to run curl command: %w (stderr: %s)", err, stderrStr)
		}

		// Get the CA certificate content
		caContent := stdout.String()
		if len(caContent) == 0 {
			return fmt.Errorf("received empty CA certificate content")
		}

		// Basic validation that we got a certificate
		if !bytes.Contains([]byte(caContent), []byte("BEGIN CERTIFICATE")) {
			return fmt.Errorf("invalid CA certificate format received: %s", caContent[:min(100, len(caContent))])
		}

		proxyCA = caContent
		cm.logger.Info("Successfully retrieved proxy CA certificate", "length", len(proxyCA))
		return nil
	})

	if err != nil {
		return "", fmt.Errorf("failed to fetch proxy CA after retries: %w", err)
	}

	return proxyCA, nil
}

// min returns the minimum of two integers
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// DeleteProxyVM deletes the proxy VM and associated resources
func (cm *ComputeManager) DeleteProxyVM(ctx context.Context, infraID string) error {
	instanceName := fmt.Sprintf("%s-proxy", infraID)
	cm.logger.Info("Deleting proxy VM", "instance", instanceName)

	// Get the zone where the instance is located
	zones, err := cm.findInstanceZones(ctx, instanceName)
	if err != nil {
		return err
	}

	if len(zones) == 0 {
		cm.logger.Info("No proxy VM found to delete", "instance", instanceName)
		return nil
	}

	// Delete the instance from all zones (should only be one)
	for _, zone := range zones {
		operation, err := cm.clients.Compute.Instances.Delete(cm.projectID, zone, instanceName).Context(ctx).Do()
		if err != nil {
			return gcputil.HandleResourceDeletionError(err, "proxy instance", instanceName)
		}

		// Wait for deletion to complete
		err = cm.waitForZoneOperation(ctx, zone, operation.Name, fmt.Sprintf("proxy instance %s deletion", instanceName))
		if err != nil {
			return fmt.Errorf("failed waiting for instance deletion: %w", err)
		}

		cm.logger.Info("Successfully deleted proxy VM", "instance", instanceName, "zone", zone)
	}

	return nil
}

// findInstanceZones finds all zones where an instance with the given name exists
func (cm *ComputeManager) findInstanceZones(ctx context.Context, instanceName string) ([]string, error) {
	var zones []string

	// List all zones in the region
	zoneList, err := cm.clients.Compute.Zones.List(cm.projectID).Filter(fmt.Sprintf("region eq .*%s$", cm.region)).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("failed to list zones: %w", err)
	}

	// Check each zone for the instance
	for _, zone := range zoneList.Items {
		_, err := cm.clients.Compute.Instances.Get(cm.projectID, zone.Name, instanceName).Context(ctx).Do()
		if err != nil {
			if gcputil.IsGCPNotFoundError(err) {
				continue // Instance not in this zone
			}
			return nil, fmt.Errorf("failed to check instance in zone %s: %w", zone.Name, err)
		}
		zones = append(zones, zone.Name)
	}

	return zones, nil
}

// Helper function to get string pointer
func stringPtr(s string) *string {
	return &s
}