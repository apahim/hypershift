package util

import (
	"fmt"
	"net"
	"strings"
)

// ValidateVPCCIDR validates a VPC CIDR block
func ValidateVPCCIDR(cidr string) error {
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return fmt.Errorf("invalid VPC CIDR: %w", err)
	}

	// Check if it's a valid private network range
	if !ipNet.IP.IsPrivate() {
		return fmt.Errorf("VPC CIDR must be a private network range")
	}

	// Check minimum network size (typically /8 to /29)
	ones, _ := ipNet.Mask.Size()
	if ones < 8 || ones > 29 {
		return fmt.Errorf("VPC CIDR mask must be between /8 and /29")
	}

	return nil
}

// EnsureTrailingDot ensures a domain name has a trailing dot for DNS
func EnsureTrailingDot(domain string) string {
	if !strings.HasSuffix(domain, ".") {
		return domain + "."
	}
	return domain
}

// ExtractZoneFromURL extracts zone name from GCP resource URL
func ExtractZoneFromURL(zoneURL string) string {
	parts := strings.Split(zoneURL, "/")
	return parts[len(parts)-1]
}

// ExtractRegionFromURL extracts region name from GCP resource URL
func ExtractRegionFromURL(regionURL string) string {
	parts := strings.Split(regionURL, "/")
	return parts[len(parts)-1]
}

// CopyIPNet creates a copy of an IPNet
func CopyIPNet(in *net.IPNet) *net.IPNet {
	result := *in
	resultIP := make(net.IP, len(in.IP))
	copy(resultIP, in.IP)
	result.IP = resultIP
	return &result
}

// GenerateSubnetCIDR generates subnet CIDRs from a VPC CIDR
func GenerateSubnetCIDR(vpcCIDR string, subnetIndex, subnetBits int) (string, error) {
	_, ipNet, err := net.ParseCIDR(vpcCIDR)
	if err != nil {
		return "", fmt.Errorf("invalid VPC CIDR: %w", err)
	}

	// Calculate new subnet mask
	ones, totalBits := ipNet.Mask.Size()
	newOnes := ones + subnetBits

	if newOnes > totalBits {
		return "", fmt.Errorf("subnet mask too large")
	}

	// Create subnet IP by adding offset
	subnetIP := make(net.IP, len(ipNet.IP))
	copy(subnetIP, ipNet.IP)

	// Add subnet index to the appropriate byte
	byteIndex := ones / 8
	if byteIndex < len(subnetIP) {
		subnetIP[byteIndex] += byte(subnetIndex << (8 - (subnetBits % 8)))
	}

	return fmt.Sprintf("%s/%d", subnetIP.String(), newOnes), nil
}

// ParseAdditionalLabels parses label strings in key=value format
func ParseAdditionalLabels(labelStrings []string) (map[string]string, error) {
	labels := make(map[string]string)
	for _, label := range labelStrings {
		parts := strings.SplitN(label, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid label format: %s (expected key=value)", label)
		}
		labels[parts[0]] = parts[1]
	}
	return labels, nil
}

// MergeLabels merges multiple label maps, with later maps taking precedence
func MergeLabels(labelMaps ...map[string]string) map[string]string {
	result := make(map[string]string)
	for _, labels := range labelMaps {
		for k, v := range labels {
			result[k] = v
		}
	}
	return result
}
