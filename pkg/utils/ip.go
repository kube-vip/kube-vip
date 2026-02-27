package utils

import (
	"fmt"
	"net"
	"strings"

	v1 "k8s.io/api/core/v1"
)

// FormatIPWithSubnetMask takes a raw IP address and a subnet mask, and returns a formatted string in CIDR notation.
func FormatIPWithSubnetMask(rawIP string, subnetMask string) (string, error) {

	addr := fmt.Sprintf("%s/%s", rawIP, subnetMask)
	// Check if the input is valid
	_, _, err := net.ParseCIDR(addr)
	if err != nil {
		return "", fmt.Errorf("invalid CIDR: %q, %w", addr, err)
	}
	return addr, nil
}

// IsIP returns if address is an IP or not
func IsIP(address string) bool {
	ip := net.ParseIP(address)
	return ip != nil
}

// IsIPv4 returns true only if address is a valid IPv4 address
func IsIPv4(address string) bool {
	ip := net.ParseIP(address)
	if ip == nil {
		return false
	}
	return ip.To4() != nil
}

// IsIPv6 returns true only if address is a valid IPv6 address
func IsIPv6(address string) bool {
	ip := net.ParseIP(address)
	if ip == nil {
		return false
	}
	return ip.To4() == nil
}

func IsIPv4CIDR(cidr string) bool {
	ip, _, _ := net.ParseCIDR(cidr)
	if ip == nil {
		return false
	}
	return ip.To4() != nil
}

func IsIPv6CIDR(cidr string) bool {
	ip, _, _ := net.ParseCIDR(cidr)
	if ip == nil {
		return false
	}
	return ip.To4() == nil
}

// StripCIDR removes the CIDR notation (e.g., "/24") from an IP address string.
// If no CIDR notation is present, the original string is returned unchanged.
func StripCIDR(ip string) string {
	if idx := strings.Index(ip, "/"); idx >= 0 {
		return ip[:idx]
	}
	return ip
}

// SanitizeServiceID sanitizes a service ID to be valid for nftables chain names.
// Only alphanumeric characters and underscores are allowed; other characters are replaced with underscores.
// The result is truncated to 50 characters to respect nftables name length limits.
func SanitizeServiceID(id string) string {
	var result strings.Builder
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			result.WriteRune(r)
		} else {
			result.WriteRune('_')
		}
	}
	sanitized := result.String()

	// Ensure it doesn't exceed nftables name length limit
	if len(sanitized) > 50 {
		sanitized = sanitized[:50]
	}

	return sanitized
}

// FetchServiceIPs extracts IP addresses from a Kubernetes Service.
// It checks the following sources in order:
// 1. kube-vip.io/loadbalancerIPs annotation (comma-separated list)
// 2. spec.LoadBalancerIP (deprecated but still used)
// 3. status.loadBalancer.ingress
// Returns an error if no IPs are found.
func FetchServiceIPs(service *v1.Service) ([]string, error) {
	var ips []string

	// Check for loadBalancerIPs annotation first (new style)
	if loadBalancerIPs, ok := service.Annotations["kube-vip.io/loadbalancerIPs"]; ok {
		for _, ip := range strings.Split(loadBalancerIPs, ",") {
			ip = strings.TrimSpace(ip)
			if ip != "" {
				ips = append(ips, ip)
			}
		}
	}

	// Fallback to spec.LoadBalancerIP (deprecated but still used)
	if len(ips) == 0 && service.Spec.LoadBalancerIP != "" {
		ips = append(ips, service.Spec.LoadBalancerIP)
	}

	// Check status.loadBalancer.ingress as well
	if len(ips) == 0 {
		for _, ingress := range service.Status.LoadBalancer.Ingress {
			if ingress.IP != "" {
				ips = append(ips, ingress.IP)
			}
		}
	}

	if len(ips) == 0 {
		return nil, fmt.Errorf("no IPs found for service")
	}

	return ips, nil
}
