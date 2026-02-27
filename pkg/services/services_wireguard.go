package services

import (
	"context"
	"fmt"

	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/nftables"
	"github.com/kube-vip/kube-vip/pkg/utils"
	v1 "k8s.io/api/core/v1"
)

// addServiceWireguard configures a WireGuard tunnel for a service
// The tunnel is brought up here, but DNAT rules are configured by the endpoint watcher
// via wireguardWorker.processInstance() when endpoints become available
func (p *Processor) addServiceWireguard(_ context.Context, svc *v1.Service) error {
	if !p.config.EnableWireguard {
		return nil
	}

	// Get service VIPs
	serviceIPs, err := utils.FetchServiceIPs(svc)
	if err != nil {
		return fmt.Errorf("failed to get service IPs for %s/%s: %w", svc.Namespace, svc.Name, err)
	}

	if len(serviceIPs) == 0 {
		return fmt.Errorf("no service IPs found for service %s/%s", svc.Namespace, svc.Name)
	}

	// For each VIP, bring up the WireGuard tunnel
	// DNAT rules will be configured by the endpoint watcher when endpoints are available
	var successCount int
	var lastErr error
	for _, vip := range serviceIPs {
		if err := p.setupServiceWireguardTunnel(svc, vip); err != nil {
			log.Error("[wireguard] failed to setup tunnel for VIP",
				"service", svc.Name,
				"namespace", svc.Namespace,
				"vip", vip,
				"err", err)
			lastErr = err
			// Continue with other VIPs even if one fails
			continue
		}
		successCount++
	}

	if successCount == 0 {
		return fmt.Errorf("failed to setup WireGuard tunnel for any VIP in service %s/%s: %w", svc.Namespace, svc.Name, lastErr)
	}

	return nil
}

// setupServiceWireguardTunnel brings up the WireGuard tunnel for a single VIP
// DNAT rules are NOT configured here - they are handled by the endpoint watcher
func (p *Processor) setupServiceWireguardTunnel(svc *v1.Service, vip string) error {
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

	// DNAT rules will be configured by wireguardWorker.processInstance()
	// when the endpoint watcher detects available endpoints

	return nil
}

// deleteServiceWireguard removes nftables DNAT rules and tears down WireGuard tunnel for a service
func (p *Processor) deleteServiceWireguard(_ context.Context, svc *v1.Service) {
	if !p.config.EnableWireguard {
		return
	}

	serviceID := fmt.Sprintf("%s_%s", svc.Namespace, svc.Name)
	serviceID = utils.SanitizeServiceID(serviceID)

	log.Info("[wireguard] deleting DNAT rules and tunnel for service",
		"namespace", svc.Namespace,
		"name", svc.Name,
		"serviceID", serviceID)

	// Get service IPs
	serviceIPs, _ := utils.FetchServiceIPs(svc)

	// Delete DNAT chains for each port
	for _, port := range svc.Spec.Ports {
		if port.Protocol != v1.ProtocolTCP && port.Protocol != v1.ProtocolUDP {
			continue
		}

		portServiceID := fmt.Sprintf("%s_p%d", serviceID, port.Port)

		// Try to delete for both IPv4 and IPv6 if we have mixed IPs
		hasIPv4 := false
		hasIPv6 := false
		for _, vip := range serviceIPs {
			// Strip CIDR notation before checking IP version
			addr := utils.StripCIDR(vip)
			if utils.IsIPv6(addr) {
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
