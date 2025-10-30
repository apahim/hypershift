package gcp

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"google.golang.org/api/compute/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	gcputil "github.com/openshift/hypershift/cmd/infra/gcp/util"
)

// FirewallManager handles firewall rule operations for GCP
type FirewallManager struct {
	clients   *gcputil.GCPClients
	projectID string
	region    string
	logger    logr.Logger
}

// FirewallRuleDetails represents firewall rule information for output
type FirewallRuleDetails struct {
	Name              string            `json:"name"`
	Type              string            `json:"type"` // "ingress" or "egress"
	Direction         string            `json:"direction"`
	VPCName           string            `json:"vpcName"`
	SourceRanges      []string          `json:"sourceRanges,omitempty"`
	TargetTags        []string          `json:"targetTags,omitempty"`
	Allowed           []string          `json:"allowed"`
	Labels            map[string]string `json:"labels"`
	CreationTimestamp string            `json:"creationTimestamp"`
}

// NewFirewallManager creates a new firewall manager
func NewFirewallManager(clients *gcputil.GCPClients, projectID, region string, logger logr.Logger) *FirewallManager {
	return &FirewallManager{
		clients:   clients,
		projectID: projectID,
		region:    region,
		logger:    logger,
	}
}

// CreateFirewallRules creates all necessary firewall rules for the cluster
func (fm *FirewallManager) CreateFirewallRules(ctx context.Context, infraID, vpcName, vpcCIDR string, labels map[string]string) ([]*FirewallRuleDetails, error) {
	fm.logger.Info("Creating firewall rules", "infraID", infraID, "vpc", vpcName, "vpcCIDR", vpcCIDR)

	var rules []*FirewallRuleDetails

	// Build VPC network reference
	vpcSelfLink := fmt.Sprintf("projects/%s/global/networks/%s", fm.projectID, vpcName)

	// Create internal cluster communication rule
	internalRule, err := fm.createInternalClusterRule(ctx, infraID, vpcSelfLink, vpcCIDR, labels)
	if err != nil {
		return nil, fmt.Errorf("failed to create internal cluster rule: %w", err)
	}
	rules = append(rules, internalRule)

	// Create SSH access rule
	sshRule, err := fm.createSSHAccessRule(ctx, infraID, vpcSelfLink, labels)
	if err != nil {
		return nil, fmt.Errorf("failed to create SSH access rule: %w", err)
	}
	rules = append(rules, sshRule)

	// Create API server access rule
	apiRule, err := fm.createAPIServerRule(ctx, infraID, vpcSelfLink, labels)
	if err != nil {
		return nil, fmt.Errorf("failed to create API server rule: %w", err)
	}
	rules = append(rules, apiRule)

	// Create ingress controller rule
	ingressRule, err := fm.createIngressRule(ctx, infraID, vpcSelfLink, labels)
	if err != nil {
		return nil, fmt.Errorf("failed to create ingress rule: %w", err)
	}
	rules = append(rules, ingressRule)

	// Create node port range rule
	nodePortRule, err := fm.createNodePortRule(ctx, infraID, vpcSelfLink, labels)
	if err != nil {
		return nil, fmt.Errorf("failed to create node port rule: %w", err)
	}
	rules = append(rules, nodePortRule)

	// Create ICMP rule for health checks
	icmpRule, err := fm.createICMPRule(ctx, infraID, vpcSelfLink, vpcCIDR, labels)
	if err != nil {
		return nil, fmt.Errorf("failed to create ICMP rule: %w", err)
	}
	rules = append(rules, icmpRule)

	fm.logger.Info("Successfully created all firewall rules", "count", len(rules))
	return rules, nil
}

// CreateProxyFirewallRule creates a firewall rule for proxy VM access
func (fm *FirewallManager) CreateProxyFirewallRule(ctx context.Context, infraID, vpcName, vpcCIDR string, labels map[string]string) (*FirewallRuleDetails, error) {
	fm.logger.Info("Creating proxy firewall rule", "infraID", infraID, "vpc", vpcName)

	vpcSelfLink := fmt.Sprintf("projects/%s/global/networks/%s", fm.projectID, vpcName)
	proxyTag := fmt.Sprintf("%s-proxy", infraID)
	ruleName := fmt.Sprintf("%s-proxy-access", infraID)

	// Check if rule already exists
	existingRule, err := fm.getExistingFirewallRule(ctx, ruleName)
	if err != nil {
		return nil, err
	}

	if existingRule != nil {
		fm.logger.Info("Found existing proxy firewall rule", "name", ruleName)
		return fm.convertToRuleDetails(existingRule, "proxy"), nil
	}

	// Create proxy access rule - allows access to proxy port from VPC CIDR
	rule := &compute.Firewall{
		Name:         ruleName,
		Network:      vpcSelfLink,
		Description:  fmt.Sprintf("Allow proxy access for cluster %s", infraID),
		Direction:    "INGRESS",
		Priority:     1000,
		SourceRanges: []string{vpcCIDR}, // Allow from VPC CIDR
		TargetTags:   []string{proxyTag},
		Allowed: []*compute.FirewallAllowed{
			{
				IPProtocol: "tcp",
				Ports:      []string{"3128"}, // Standard proxy port
			},
			{
				IPProtocol: "tcp",
				Ports:      []string{"22"}, // SSH access for management
			},
		},
	}

	// Note: Labels are not supported on GCP firewall rules directly
	// They will be included in the returned FirewallRuleDetails struct

	// Insert the firewall rule
	operation, err := fm.clients.Compute.Firewalls.Insert(fm.projectID, rule).Context(ctx).Do()
	if err != nil {
		return nil, gcputil.HandleResourceCreationError(err, "proxy firewall rule", ruleName)
	}

	// Wait for the operation to complete
	err = fm.waitForGlobalOperation(ctx, operation.Name, fmt.Sprintf("proxy firewall rule %s creation", ruleName))
	if err != nil {
		return nil, fmt.Errorf("failed waiting for proxy firewall rule creation: %w", err)
	}

	// Get the created rule
	createdRule, err := fm.clients.Compute.Firewalls.Get(fm.projectID, ruleName).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("failed to get created proxy firewall rule: %w", err)
	}

	fm.logger.Info("Successfully created proxy firewall rule", "name", ruleName)
	return fm.convertToRuleDetails(createdRule, "proxy"), nil
}

// DeleteProxyFirewallRule deletes the proxy firewall rule
func (fm *FirewallManager) DeleteProxyFirewallRule(ctx context.Context, infraID string) error {
	ruleName := fmt.Sprintf("%s-proxy-access", infraID)
	fm.logger.Info("Deleting proxy firewall rule", "name", ruleName)

	operation, err := fm.clients.Compute.Firewalls.Delete(fm.projectID, ruleName).Context(ctx).Do()
	if err != nil {
		return gcputil.HandleResourceDeletionError(err, "proxy firewall rule", ruleName)
	}

	err = fm.waitForGlobalOperation(ctx, operation.Name, fmt.Sprintf("proxy firewall rule %s deletion", ruleName))
	if err != nil {
		return fmt.Errorf("failed waiting for proxy firewall rule deletion: %w", err)
	}

	fm.logger.Info("Successfully deleted proxy firewall rule", "name", ruleName)
	return nil
}

// createInternalClusterRule creates a rule allowing internal cluster communication
func (fm *FirewallManager) createInternalClusterRule(ctx context.Context, infraID, vpcSelfLink, vpcCIDR string, labels map[string]string) (*FirewallRuleDetails, error) {
	ruleName := fmt.Sprintf("%s-internal", infraID)

	// Check if rule already exists
	existingRule, err := fm.getExistingFirewallRule(ctx, ruleName)
	if err != nil {
		return nil, err
	}

	if existingRule != nil {
		fm.logger.Info("Found existing internal cluster rule", "name", ruleName)
		return fm.convertToRuleDetails(existingRule, "internal"), nil
	}

	rule := &compute.Firewall{
		Name:         ruleName,
		Network:      vpcSelfLink,
		Description:  fmt.Sprintf("HyperShift internal cluster communication for %s", infraID),
		Direction:    "INGRESS",
		SourceRanges: []string{vpcCIDR},
		TargetTags:   []string{fmt.Sprintf("hypershift-%s", infraID)},
		Allowed: []*compute.FirewallAllowed{
			{
				IPProtocol: "tcp",
				Ports:      []string{"0-65535"},
			},
			{
				IPProtocol: "udp",
				Ports:      []string{"0-65535"},
			},
		},
		LogConfig: &compute.FirewallLogConfig{
			Enable: false, // Disable logging for internal traffic to reduce costs
		},
	}

	fm.logger.Info("Creating internal cluster firewall rule", "name", ruleName)

	op, err := fm.clients.Compute.Firewalls.Insert(fm.projectID, rule).Context(ctx).Do()
	if err != nil {
		return nil, gcputil.HandleResourceCreationError(err, "firewall rule", ruleName)
	}

	// Wait for operation to complete
	err = fm.waitForGlobalOperation(ctx, op.Name, fmt.Sprintf("firewall rule %s creation", ruleName))
	if err != nil {
		return nil, fmt.Errorf("failed waiting for firewall rule creation: %w", err)
	}

	fm.logger.Info("Successfully created internal cluster firewall rule", "name", ruleName)

	return &FirewallRuleDetails{
		Name:         ruleName,
		Type:         "internal",
		Direction:    "INGRESS",
		VPCName:      extractVPCNameFromSelfLink(vpcSelfLink),
		SourceRanges: []string{vpcCIDR},
		TargetTags:   []string{fmt.Sprintf("hypershift-%s", infraID)},
		Allowed:      []string{"tcp:0-65535", "udp:0-65535"},
		Labels:       labels,
	}, nil
}

// createSSHAccessRule creates a rule allowing SSH access
func (fm *FirewallManager) createSSHAccessRule(ctx context.Context, infraID, vpcSelfLink string, labels map[string]string) (*FirewallRuleDetails, error) {
	ruleName := fmt.Sprintf("%s-ssh", infraID)

	// Check if rule already exists
	existingRule, err := fm.getExistingFirewallRule(ctx, ruleName)
	if err != nil {
		return nil, err
	}

	if existingRule != nil {
		fm.logger.Info("Found existing SSH access rule", "name", ruleName)
		return fm.convertToRuleDetails(existingRule, "ssh"), nil
	}

	rule := &compute.Firewall{
		Name:         ruleName,
		Network:      vpcSelfLink,
		Description:  fmt.Sprintf("HyperShift SSH access for %s", infraID),
		Direction:    "INGRESS",
		SourceRanges: []string{"0.0.0.0/0"}, // Allow from anywhere - can be restricted later
		TargetTags:   []string{fmt.Sprintf("hypershift-%s", infraID)},
		Allowed: []*compute.FirewallAllowed{
			{
				IPProtocol: "tcp",
				Ports:      []string{"22"},
			},
		},
	}

	fm.logger.Info("Creating SSH access firewall rule", "name", ruleName)

	op, err := fm.clients.Compute.Firewalls.Insert(fm.projectID, rule).Context(ctx).Do()
	if err != nil {
		return nil, gcputil.HandleResourceCreationError(err, "firewall rule", ruleName)
	}

	// Wait for operation to complete
	err = fm.waitForGlobalOperation(ctx, op.Name, fmt.Sprintf("firewall rule %s creation", ruleName))
	if err != nil {
		return nil, fmt.Errorf("failed waiting for firewall rule creation: %w", err)
	}

	fm.logger.Info("Successfully created SSH access firewall rule", "name", ruleName)

	return &FirewallRuleDetails{
		Name:         ruleName,
		Type:         "ssh",
		Direction:    "INGRESS",
		VPCName:      extractVPCNameFromSelfLink(vpcSelfLink),
		SourceRanges: []string{"0.0.0.0/0"},
		TargetTags:   []string{fmt.Sprintf("hypershift-%s", infraID)},
		Allowed:      []string{"tcp:22"},
		Labels:       labels,
	}, nil
}

// createAPIServerRule creates a rule allowing API server access
func (fm *FirewallManager) createAPIServerRule(ctx context.Context, infraID, vpcSelfLink string, labels map[string]string) (*FirewallRuleDetails, error) {
	ruleName := fmt.Sprintf("%s-api", infraID)

	// Check if rule already exists
	existingRule, err := fm.getExistingFirewallRule(ctx, ruleName)
	if err != nil {
		return nil, err
	}

	if existingRule != nil {
		fm.logger.Info("Found existing API server rule", "name", ruleName)
		return fm.convertToRuleDetails(existingRule, "api"), nil
	}

	rule := &compute.Firewall{
		Name:         ruleName,
		Network:      vpcSelfLink,
		Description:  fmt.Sprintf("HyperShift API server access for %s", infraID),
		Direction:    "INGRESS",
		SourceRanges: []string{"0.0.0.0/0"}, // Allow from anywhere - can be restricted later
		TargetTags:   []string{fmt.Sprintf("hypershift-%s-api", infraID)},
		Allowed: []*compute.FirewallAllowed{
			{
				IPProtocol: "tcp",
				Ports:      []string{"6443"}, // Kubernetes API server port
			},
		},
	}

	fm.logger.Info("Creating API server firewall rule", "name", ruleName)

	op, err := fm.clients.Compute.Firewalls.Insert(fm.projectID, rule).Context(ctx).Do()
	if err != nil {
		return nil, gcputil.HandleResourceCreationError(err, "firewall rule", ruleName)
	}

	// Wait for operation to complete
	err = fm.waitForGlobalOperation(ctx, op.Name, fmt.Sprintf("firewall rule %s creation", ruleName))
	if err != nil {
		return nil, fmt.Errorf("failed waiting for firewall rule creation: %w", err)
	}

	fm.logger.Info("Successfully created API server firewall rule", "name", ruleName)

	return &FirewallRuleDetails{
		Name:         ruleName,
		Type:         "api",
		Direction:    "INGRESS",
		VPCName:      extractVPCNameFromSelfLink(vpcSelfLink),
		SourceRanges: []string{"0.0.0.0/0"},
		TargetTags:   []string{fmt.Sprintf("hypershift-%s-api", infraID)},
		Allowed:      []string{"tcp:6443"},
		Labels:       labels,
	}, nil
}

// createIngressRule creates a rule allowing ingress controller access
func (fm *FirewallManager) createIngressRule(ctx context.Context, infraID, vpcSelfLink string, labels map[string]string) (*FirewallRuleDetails, error) {
	ruleName := fmt.Sprintf("%s-ingress", infraID)

	// Check if rule already exists
	existingRule, err := fm.getExistingFirewallRule(ctx, ruleName)
	if err != nil {
		return nil, err
	}

	if existingRule != nil {
		fm.logger.Info("Found existing ingress rule", "name", ruleName)
		return fm.convertToRuleDetails(existingRule, "ingress"), nil
	}

	rule := &compute.Firewall{
		Name:         ruleName,
		Network:      vpcSelfLink,
		Description:  fmt.Sprintf("HyperShift ingress controller access for %s", infraID),
		Direction:    "INGRESS",
		SourceRanges: []string{"0.0.0.0/0"},
		TargetTags:   []string{fmt.Sprintf("hypershift-%s-ingress", infraID)},
		Allowed: []*compute.FirewallAllowed{
			{
				IPProtocol: "tcp",
				Ports:      []string{"80", "443"},
			},
		},
	}

	fm.logger.Info("Creating ingress firewall rule", "name", ruleName)

	op, err := fm.clients.Compute.Firewalls.Insert(fm.projectID, rule).Context(ctx).Do()
	if err != nil {
		return nil, gcputil.HandleResourceCreationError(err, "firewall rule", ruleName)
	}

	// Wait for operation to complete
	err = fm.waitForGlobalOperation(ctx, op.Name, fmt.Sprintf("firewall rule %s creation", ruleName))
	if err != nil {
		return nil, fmt.Errorf("failed waiting for firewall rule creation: %w", err)
	}

	fm.logger.Info("Successfully created ingress firewall rule", "name", ruleName)

	return &FirewallRuleDetails{
		Name:         ruleName,
		Type:         "ingress",
		Direction:    "INGRESS",
		VPCName:      extractVPCNameFromSelfLink(vpcSelfLink),
		SourceRanges: []string{"0.0.0.0/0"},
		TargetTags:   []string{fmt.Sprintf("hypershift-%s-ingress", infraID)},
		Allowed:      []string{"tcp:80", "tcp:443"},
		Labels:       labels,
	}, nil
}

// createNodePortRule creates a rule allowing NodePort access
func (fm *FirewallManager) createNodePortRule(ctx context.Context, infraID, vpcSelfLink string, labels map[string]string) (*FirewallRuleDetails, error) {
	ruleName := fmt.Sprintf("%s-nodeport", infraID)

	// Check if rule already exists
	existingRule, err := fm.getExistingFirewallRule(ctx, ruleName)
	if err != nil {
		return nil, err
	}

	if existingRule != nil {
		fm.logger.Info("Found existing NodePort rule", "name", ruleName)
		return fm.convertToRuleDetails(existingRule, "nodeport"), nil
	}

	rule := &compute.Firewall{
		Name:         ruleName,
		Network:      vpcSelfLink,
		Description:  fmt.Sprintf("HyperShift NodePort services access for %s", infraID),
		Direction:    "INGRESS",
		SourceRanges: []string{"0.0.0.0/0"},
		TargetTags:   []string{fmt.Sprintf("hypershift-%s", infraID)},
		Allowed: []*compute.FirewallAllowed{
			{
				IPProtocol: "tcp",
				Ports:      []string{"30000-32767"}, // Standard Kubernetes NodePort range
			},
		},
	}

	fm.logger.Info("Creating NodePort firewall rule", "name", ruleName)

	op, err := fm.clients.Compute.Firewalls.Insert(fm.projectID, rule).Context(ctx).Do()
	if err != nil {
		return nil, gcputil.HandleResourceCreationError(err, "firewall rule", ruleName)
	}

	// Wait for operation to complete
	err = fm.waitForGlobalOperation(ctx, op.Name, fmt.Sprintf("firewall rule %s creation", ruleName))
	if err != nil {
		return nil, fmt.Errorf("failed waiting for firewall rule creation: %w", err)
	}

	fm.logger.Info("Successfully created NodePort firewall rule", "name", ruleName)

	return &FirewallRuleDetails{
		Name:         ruleName,
		Type:         "nodeport",
		Direction:    "INGRESS",
		VPCName:      extractVPCNameFromSelfLink(vpcSelfLink),
		SourceRanges: []string{"0.0.0.0/0"},
		TargetTags:   []string{fmt.Sprintf("hypershift-%s", infraID)},
		Allowed:      []string{"tcp:30000-32767"},
		Labels:       labels,
	}, nil
}

// createICMPRule creates a rule allowing ICMP traffic for health checks
func (fm *FirewallManager) createICMPRule(ctx context.Context, infraID, vpcSelfLink, vpcCIDR string, labels map[string]string) (*FirewallRuleDetails, error) {
	ruleName := fmt.Sprintf("%s-icmp", infraID)

	// Check if rule already exists
	existingRule, err := fm.getExistingFirewallRule(ctx, ruleName)
	if err != nil {
		return nil, err
	}

	if existingRule != nil {
		fm.logger.Info("Found existing ICMP rule", "name", ruleName)
		return fm.convertToRuleDetails(existingRule, "icmp"), nil
	}

	rule := &compute.Firewall{
		Name:         ruleName,
		Network:      vpcSelfLink,
		Description:  fmt.Sprintf("HyperShift ICMP traffic for %s", infraID),
		Direction:    "INGRESS",
		SourceRanges: []string{vpcCIDR},
		TargetTags:   []string{fmt.Sprintf("hypershift-%s", infraID)},
		Allowed: []*compute.FirewallAllowed{
			{
				IPProtocol: "icmp",
			},
		},
	}

	fm.logger.Info("Creating ICMP firewall rule", "name", ruleName)

	op, err := fm.clients.Compute.Firewalls.Insert(fm.projectID, rule).Context(ctx).Do()
	if err != nil {
		return nil, gcputil.HandleResourceCreationError(err, "firewall rule", ruleName)
	}

	// Wait for operation to complete
	err = fm.waitForGlobalOperation(ctx, op.Name, fmt.Sprintf("firewall rule %s creation", ruleName))
	if err != nil {
		return nil, fmt.Errorf("failed waiting for firewall rule creation: %w", err)
	}

	fm.logger.Info("Successfully created ICMP firewall rule", "name", ruleName)

	return &FirewallRuleDetails{
		Name:         ruleName,
		Type:         "icmp",
		Direction:    "INGRESS",
		VPCName:      extractVPCNameFromSelfLink(vpcSelfLink),
		SourceRanges: []string{vpcCIDR},
		TargetTags:   []string{fmt.Sprintf("hypershift-%s", infraID)},
		Allowed:      []string{"icmp"},
		Labels:       labels,
	}, nil
}

// DeleteFirewallRules deletes all firewall rules for a cluster
func (fm *FirewallManager) DeleteFirewallRules(ctx context.Context, infraID string) error {
	fm.logger.Info("Deleting firewall rules", "infraID", infraID)

	ruleNames := []string{
		fmt.Sprintf("%s-internal", infraID),
		fmt.Sprintf("%s-ssh", infraID),
		fmt.Sprintf("%s-api", infraID),
		fmt.Sprintf("%s-ingress", infraID),
		fmt.Sprintf("%s-nodeport", infraID),
		fmt.Sprintf("%s-icmp", infraID),
	}

	for _, ruleName := range ruleNames {
		if err := fm.deleteFirewallRule(ctx, ruleName); err != nil {
			// Don't fail the entire operation if rule deletion fails - it might not exist
			fm.logger.Error(err, "Failed to delete firewall rule, continuing", "rule", ruleName)
		} else {
			fm.logger.Info("Successfully deleted firewall rule", "name", ruleName)
		}
	}

	fm.logger.Info("Successfully deleted all firewall rules")
	return nil
}

// deleteFirewallRule deletes a single firewall rule
func (fm *FirewallManager) deleteFirewallRule(ctx context.Context, ruleName string) error {
	fm.logger.Info("Deleting firewall rule", "name", ruleName)

	op, err := fm.clients.Compute.Firewalls.Delete(fm.projectID, ruleName).Context(ctx).Do()
	if err != nil {
		return gcputil.HandleResourceDeletionError(err, "firewall rule", ruleName)
	}

	// Wait for deletion to complete
	err = fm.waitForGlobalOperation(ctx, op.Name, fmt.Sprintf("firewall rule %s deletion", ruleName))
	if err != nil {
		return fmt.Errorf("failed waiting for firewall rule deletion: %w", err)
	}

	fm.logger.Info("Successfully deleted firewall rule", "name", ruleName)
	return nil
}

// getExistingFirewallRule checks if a firewall rule already exists
func (fm *FirewallManager) getExistingFirewallRule(ctx context.Context, ruleName string) (*compute.Firewall, error) {
	rule, err := fm.clients.Compute.Firewalls.Get(fm.projectID, ruleName).Context(ctx).Do()
	if err != nil {
		if gcputil.IsGCPNotFoundError(err) {
			return nil, nil // Rule doesn't exist
		}
		return nil, fmt.Errorf("failed to check existing firewall rule %s: %w", ruleName, err)
	}
	return rule, nil
}

// waitForGlobalOperation waits for a global operation to complete
func (fm *FirewallManager) waitForGlobalOperation(ctx context.Context, operationName, description string) error {
	fm.logger.Info("Waiting for global operation", "operation", operationName, "description", description)

	return wait.PollUntilContextTimeout(ctx, 5*time.Second, gcpOperationTimeoutMinutes*time.Minute, true, func(ctx context.Context) (bool, error) {
		op, err := fm.clients.Compute.GlobalOperations.Get(fm.projectID, operationName).Context(ctx).Do()
		if err != nil {
			return false, fmt.Errorf("failed to get operation status: %w", err)
		}

		switch op.Status {
		case "DONE":
			if op.Error != nil {
				return false, fmt.Errorf("operation failed: %v", op.Error)
			}
			return true, nil
		case "RUNNING", "PENDING":
			fm.logger.V(1).Info("Operation in progress", "status", op.Status, "progress", op.Progress)
			return false, nil
		default:
			return false, fmt.Errorf("unexpected operation status: %s", op.Status)
		}
	})
}

// convertToRuleDetails converts a GCP firewall rule to our details struct
func (fm *FirewallManager) convertToRuleDetails(rule *compute.Firewall, ruleType string) *FirewallRuleDetails {
	var allowed []string
	for _, allow := range rule.Allowed {
		if len(allow.Ports) > 0 {
			for _, port := range allow.Ports {
				allowed = append(allowed, fmt.Sprintf("%s:%s", allow.IPProtocol, port))
			}
		} else {
			allowed = append(allowed, allow.IPProtocol)
		}
	}

	return &FirewallRuleDetails{
		Name:              rule.Name,
		Type:              ruleType,
		Direction:         rule.Direction,
		VPCName:           extractVPCNameFromSelfLink(rule.Network),
		SourceRanges:      rule.SourceRanges,
		TargetTags:        rule.TargetTags,
		Allowed:           allowed,
		CreationTimestamp: rule.CreationTimestamp,
	}
}

// extractVPCNameFromSelfLink extracts VPC name from a self link
func extractVPCNameFromSelfLink(selfLink string) string {
	// Extract VPC name from: projects/PROJECT_ID/global/networks/VPC_NAME
	parts := strings.Split(selfLink, "/")
	if len(parts) >= 5 {
		return parts[len(parts)-1]
	}
	return ""
}
