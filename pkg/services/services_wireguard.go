package services

import (
	"context"
	"fmt"
	"net"
	"strings"

	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/nftables"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// addServiceWireguard configures a WireGuard tunnel and nftables DNAT rules for a service
// Each service gets its own WireGuard tunnel based on its VIP
func (p *Processor) addServiceWireguard(ctx context.Context, svc *v1.Service) error {
	if !p.config.EnableWireguard {
		return nil
	}

	// Get service VIPs
	serviceIPs, err := p.fetchServiceIPs(svc)
	if err != nil {
		return fmt.Errorf("failed to get service IPs for %s/%s: %w", svc.Namespace, svc.Name, err)
	}

	if len(serviceIPs) == 0 {
		return fmt.Errorf("no service IPs found for service %s/%s", svc.Namespace, svc.Name)
	}

	// For each VIP, set up the WireGuard tunnel and DNAT rules
	for _, vip := range serviceIPs {
		if err := p.setupServiceWireguardTunnel(ctx, svc, vip); err != nil {
			log.Error("[wireguard] failed to setup tunnel for VIP",
				"service", svc.Name,
				"namespace", svc.Namespace,
				"vip", vip,
				"err", err)
			// Continue with other VIPs even if one fails
			continue
		}
	}

	return nil
}

// setupServiceWireguardTunnel sets up the WireGuard tunnel and DNAT rules for a single VIP
func (p *Processor) setupServiceWireguardTunnel(ctx context.Context, svc *v1.Service, vip string) error {
	// Check if we have a tunnel configuration for this VIP
	if !p.TunnelMgr.HasConfigForVIP(vip) {
		return fmt.Errorf("no WireGuard tunnel configuration found for VIP %s", vip)
	}

	// Get the tunnel configuration to determine the interface name
	tunnelConfig := p.TunnelMgr.GetConfigForVIP(vip)
	if tunnelConfig == nil {
		return fmt.Errorf("failed to get tunnel configuration for VIP %s", vip)
	}

	// Bring up the WireGuard tunnel for this VIP
	if err := p.TunnelMgr.BringUpTunnelForVIP(vip); err != nil {
		return fmt.Errorf("failed to bring up WireGuard tunnel for VIP %s: %w", vip, err)
	}

	log.Info("[wireguard] brought up tunnel for service",
		"namespace", svc.Namespace,
		"name", svc.Name,
		"vip", vip,
		"interface", tunnelConfig.InterfaceName)

	// Get the target endpoint for this service
	targetEndpoint, targetPort, err := p.getServiceTargetEndpoint(ctx, svc)
	if err != nil {
		return fmt.Errorf("failed to get target endpoint for service %s/%s: %w", svc.Namespace, svc.Name, err)
	}

	// Create a unique service identifier for nftables chains
	serviceID := fmt.Sprintf("%s_%s", svc.Namespace, svc.Name)
	serviceID = sanitizeServiceID(serviceID)

	log.Info("[wireguard] configuring DNAT rules for service",
		"namespace", svc.Namespace,
		"name", svc.Name,
		"vip", vip,
		"interface", tunnelConfig.InterfaceName,
		"target", targetEndpoint,
		"targetPort", targetPort)

	// Apply DNAT rules for each service port
	for _, port := range svc.Spec.Ports {
		if port.Protocol != v1.ProtocolTCP {
			log.Warn("[wireguard] skipping non-TCP port", "service", svc.Name, "port", port.Port, "protocol", port.Protocol)
			continue
		}

		isIPv6 := strings.Contains(vip, ":")

		// Strip CIDR notation if present
		vipAddr := stripCIDR(vip)

		// Parse VIP to ensure it's valid
		vipIP := net.ParseIP(vipAddr)
		if vipIP == nil {
			log.Error("[wireguard] invalid VIP", "vip", vipAddr)
			continue
		}

		// Determine the actual target port
		actualTargetPort := targetPort
		if port.NodePort != 0 && p.config.EnableServicesElection {
			actualTargetPort = port.NodePort
		}

		// Create unique service identifier including port to handle multiple ports
		portServiceID := fmt.Sprintf("%s_p%d", serviceID, port.Port)

		log.Debug("[wireguard] applying DNAT rule",
			"service", svc.Name,
			"vip", vipAddr,
			"interface", tunnelConfig.InterfaceName,
			"sourcePort", port.Port,
			"target", targetEndpoint,
			"targetPort", actualTargetPort,
			"chainID", portServiceID)

		// Apply the DNAT rule using nftables
		err := nftables.ApplyAPIServerDNAT(
			tunnelConfig.InterfaceName,
			vipAddr,
			targetEndpoint,
			uint16(port.Port),        //nolint:gosec // Port range validated by Kubernetes
			uint16(actualTargetPort), //nolint:gosec // Port range validated by Kubernetes
			portServiceID,
			isIPv6,
		)
		if err != nil {
			log.Error("[wireguard] failed to apply DNAT rule",
				"service", svc.Name,
				"vip", vipAddr,
				"port", port.Port,
				"err", err)
			continue
		}

		log.Info("[wireguard] DNAT rule applied successfully",
			"service", svc.Name,
			"vip", vipAddr,
			"interface", tunnelConfig.InterfaceName,
			"port", port.Port,
			"target", fmt.Sprintf("%s:%d", targetEndpoint, actualTargetPort))
	}

	return nil
}

// deleteServiceWireguard removes nftables DNAT rules and tears down WireGuard tunnel for a service
func (p *Processor) deleteServiceWireguard(_ context.Context, svc *v1.Service) {
	if !p.config.EnableWireguard {
		return
	}

	serviceID := fmt.Sprintf("%s_%s", svc.Namespace, svc.Name)
	serviceID = sanitizeServiceID(serviceID)

	log.Info("[wireguard] deleting DNAT rules and tunnel for service",
		"namespace", svc.Namespace,
		"name", svc.Name,
		"serviceID", serviceID)

	// Get service IPs
	serviceIPs, _ := p.fetchServiceIPs(svc)

	// Delete DNAT chains for each port
	for _, port := range svc.Spec.Ports {
		if port.Protocol != v1.ProtocolTCP {
			continue
		}

		portServiceID := fmt.Sprintf("%s_p%d", serviceID, port.Port)

		// Try to delete for both IPv4 and IPv6 if we have mixed IPs
		hasIPv4 := false
		hasIPv6 := false
		for _, vip := range serviceIPs {
			if strings.Contains(vip, ":") {
				hasIPv6 = true
			} else {
				hasIPv4 = true
			}
		}

		if hasIPv4 {
			if err := nftables.DeleteIngressChains(false, portServiceID); err != nil {
				log.Error("[wireguard] failed to delete IPv4 DNAT chains",
					"service", svc.Name,
					"port", port.Port,
					"err", err)
			} else {
				log.Debug("[wireguard] deleted IPv4 DNAT chains",
					"service", svc.Name,
					"port", port.Port)
			}
		}

		if hasIPv6 {
			if err := nftables.DeleteIngressChains(true, portServiceID); err != nil {
				log.Error("[wireguard] failed to delete IPv6 DNAT chains",
					"service", svc.Name,
					"port", port.Port,
					"err", err)
			} else {
				log.Debug("[wireguard] deleted IPv6 DNAT chains",
					"service", svc.Name,
					"port", port.Port)
			}
		}
	}

	// Tear down the WireGuard tunnel for each VIP
	for _, vip := range serviceIPs {
		if err := p.TunnelMgr.TearDownTunnelForVIP(vip); err != nil {
			log.Error("[wireguard] failed to tear down tunnel",
				"service", svc.Name,
				"vip", vip,
				"err", err)
		} else {
			log.Info("[wireguard] tore down tunnel",
				"service", svc.Name,
				"vip", vip)
		}
	}

	log.Info("[wireguard] DNAT rules deleted and tunnels torn down for service",
		"namespace", svc.Namespace,
		"name", svc.Name)
}

// getServiceTargetEndpoint determines the target endpoint for a service
func (p *Processor) getServiceTargetEndpoint(ctx context.Context, svc *v1.Service) (string, int32, error) {
	// Get endpoints for this service
	endpoints, err := p.clientSet.CoreV1().Endpoints(svc.Namespace).Get(ctx, svc.Name, metav1.GetOptions{})
	if err != nil {
		return "", 0, fmt.Errorf("failed to get endpoints: %w", err)
	}

	if len(endpoints.Subsets) == 0 {
		return "", 0, fmt.Errorf("no endpoint subsets found")
	}

	// Find first available endpoint
	for _, subset := range endpoints.Subsets {
		if len(subset.Addresses) == 0 {
			continue
		}

		if len(subset.Ports) == 0 {
			continue
		}

		// For ExternalTrafficPolicy=Local, filter by node
		if svc.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyTypeLocal {
			for _, addr := range subset.Addresses {
				if addr.NodeName != nil && *addr.NodeName == p.config.NodeName {
					return addr.IP, subset.Ports[0].Port, nil
				}
			}
		} else {
			// Use first available endpoint for Cluster policy
			return subset.Addresses[0].IP, subset.Ports[0].Port, nil
		}
	}

	return "", 0, fmt.Errorf("no suitable endpoints found")
}

// fetchServiceIPs extracts IP addresses from the service
func (p *Processor) fetchServiceIPs(svc *v1.Service) ([]string, error) {
	var ips []string

	// Check for loadBalancerIPs annotation first (new style)
	if loadBalancerIPs, ok := svc.Annotations["kube-vip.io/loadbalancerIPs"]; ok {
		for _, ip := range strings.Split(loadBalancerIPs, ",") {
			ip = strings.TrimSpace(ip)
			if ip != "" {
				ips = append(ips, ip)
			}
		}
	}

	// Fallback to spec.LoadBalancerIP (deprecated but still used)
	if len(ips) == 0 && svc.Spec.LoadBalancerIP != "" {
		ips = append(ips, svc.Spec.LoadBalancerIP)
	}

	// Check status.loadBalancer.ingress as well
	if len(ips) == 0 {
		for _, ingress := range svc.Status.LoadBalancer.Ingress {
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

// sanitizeServiceID sanitizes a service ID to be valid for nftables chain names
func sanitizeServiceID(id string) string {
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

// stripCIDR removes CIDR notation from an IP address
func stripCIDR(ip string) string {
	if idx := strings.Index(ip, "/"); idx >= 0 {
		return ip[:idx]
	}
	return ip
}
