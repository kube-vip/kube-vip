package endpoints

import (
	"context"
	"fmt"

	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/bgp"
	"github.com/kube-vip/kube-vip/pkg/endpoints/providers"
	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/nftables"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	"github.com/kube-vip/kube-vip/pkg/utils"
	"github.com/kube-vip/kube-vip/pkg/wireguard"
	v1 "k8s.io/api/core/v1"
)

// wireguardWorker handles endpoint changes for WireGuard-based services
type wireguardWorker struct {
	config    *kubevip.Config
	provider  providers.Provider
	bgpServer *bgp.Server
	instances *[]*instance.Instance
	leaseMgr  *lease.Manager
	tunnelMgr *wireguard.TunnelManager
}

func newWireguardWorker(config *kubevip.Config, provider providers.Provider, bgpServer *bgp.Server,
	instances *[]*instance.Instance, leaseMgr *lease.Manager, tunnelMgr *wireguard.TunnelManager) *wireguardWorker {
	return &wireguardWorker{
		config:    config,
		provider:  provider,
		bgpServer: bgpServer,
		instances: instances,
		leaseMgr:  leaseMgr,
		tunnelMgr: tunnelMgr,
	}
}

// processInstance updates nftables DNAT rules when endpoints change
// This is called by the endpoint watcher when endpoints are added/modified
func (w *wireguardWorker) processInstance(svcCtx *servicecontext.Context, service *v1.Service) error {
	log.Debug("[wireguard] processing instance for endpoint change", "service", service.Name, "namespace", service.Namespace)

	// Get the target endpoint for this service
	// For ExternalTrafficPolicy=Local, only use local endpoints
	// For ExternalTrafficPolicy=Cluster, use all endpoints
	var endpoints []string
	var err error
	if service.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyTypeLocal {
		endpoints, err = w.provider.GetLocalEndpoints(w.config.NodeName, w.config)
	} else {
		endpoints, err = w.provider.GetAllEndpoints()
	}
	if err != nil {
		return fmt.Errorf("failed to get endpoints: %w", err)
	}

	if len(endpoints) == 0 {
		log.Debug("[wireguard] no endpoints available", "service", service.Name)
		w.clear(svcCtx, nil, service)
		return nil
	}

	// Find the service processor to call updateServiceWireguardEndpoints
	// Note: This requires access to the service processor which we don't have here
	// So we'll recreate the DNAT rules directly

	// First, clear existing rules
	w.clear(svcCtx, nil, service)

	// Get the first endpoint (simple round-robin could be added later)
	targetIP := endpoints[0]

	// Get service VIPs
	serviceIPs, err := utils.FetchServiceIPs(service)
	if err != nil {
		return fmt.Errorf("failed to get service IPs: %w", err)
	}

	// Create service identifier
	serviceID := utils.SanitizeServiceID(fmt.Sprintf("%s_%s", service.Namespace, service.Name))

	log.Info("[wireguard] updating DNAT rules for endpoint change",
		"service", service.Name,
		"namespace", service.Namespace,
		"targetIP", targetIP,
		"vips", serviceIPs)

	// Update DNAT rules for each port
	for _, port := range service.Spec.Ports {
		// Determine protocol
		var protocol string
		switch port.Protocol {
		case v1.ProtocolTCP:
			protocol = "TCP"
		case v1.ProtocolUDP:
			protocol = "UDP"
		default:
			log.Warn("[wireguard] skipping unsupported protocol", "service", service.Name, "port", port.Port, "protocol", port.Protocol)
			continue
		}

		// Determine target port (use TargetPort if set, otherwise use Port)
		targetPort := port.Port
		if port.TargetPort.IntVal != 0 {
			targetPort = port.TargetPort.IntVal
		}

		for _, vip := range serviceIPs {
			isIPv6 := isIPv6Address(vip)

			// Strip CIDR notation if present
			vipAddr := utils.StripCIDR(vip)

			// Get WireGuard interface name from TunnelManager for this VIP
			if w.tunnelMgr == nil {
				log.Error("[wireguard] TunnelManager not configured; cannot update DNAT rules",
					"service", service.Name,
					"namespace", service.Namespace)
				return fmt.Errorf("TunnelManager not configured")
			}
			tunnelConfig := w.tunnelMgr.GetConfigForVIP(vipAddr)
			if tunnelConfig == nil {
				log.Error("[wireguard] WireGuard interface name not configured; cannot update DNAT rules",
					"service", service.Name,
					"namespace", service.Namespace,
					"vip", vipAddr)
				return fmt.Errorf("wireguard interface name not configured for VIP %s", vipAddr)
			}
			wgInterface := tunnelConfig.InterfaceName

			portServiceID := fmt.Sprintf("%s_p%d", serviceID, port.Port)

			log.Debug("[wireguard] updating DNAT rule",
				"service", service.Name,
				"vip", vipAddr,
				"interface", wgInterface,
				"sourcePort", port.Port,
				"target", targetIP,
				"targetPort", targetPort,
				"chainID", portServiceID)

			// Apply the DNAT rule
			err := nftables.ApplyDNAT(
				wgInterface,
				vipAddr,
				targetIP,
				uint16(port.Port),  //nolint:gosec // Port range validated by Kubernetes
				uint16(targetPort), //nolint:gosec // Port range validated by Kubernetes
				portServiceID,
				isIPv6,
				protocol,
			)
			if err != nil {
				log.Error("[wireguard] failed to update DNAT rule",
					"service", service.Name,
					"vip", vipAddr,
					"port", port.Port,
					"err", err)
				continue
			}

			log.Debug("[wireguard] DNAT rule updated successfully",
				"service", service.Name,
				"vip", vipAddr,
				"port", port.Port,
				"target", fmt.Sprintf("%s:%d", targetIP, targetPort))
		}
	}

	return nil
}

// clear removes DNAT rules when no endpoints are available
func (w *wireguardWorker) clear(svcCtx *servicecontext.Context, lastKnownGoodEndpoint *string, service *v1.Service) {
	log.Info("[wireguard] clearing DNAT rules (no endpoints)", "service", service.Name, "namespace", service.Namespace)

	serviceID := utils.SanitizeServiceID(fmt.Sprintf("%s_%s", service.Namespace, service.Name))

	// Get service IPs to determine IPv4 vs IPv6
	serviceIPs, _ := utils.FetchServiceIPs(service)

	// Delete DNAT chains for each port
	for _, port := range service.Spec.Ports {
		if port.Protocol != v1.ProtocolTCP && port.Protocol != v1.ProtocolUDP {
			continue
		}

		portServiceID := fmt.Sprintf("%s_p%d", serviceID, port.Port)

		// Determine if we have IPv4 or IPv6
		hasIPv4, hasIPv6 := false, false
		for _, vip := range serviceIPs {
			if isIPv6Address(vip) {
				hasIPv6 = true
			} else {
				hasIPv4 = true
			}
		}

		if hasIPv4 {
			if err := nftables.DeleteIngressChains(false, portServiceID); err != nil {
				log.Warn("[wireguard] failed to delete IPv4 DNAT chains",
					"service", service.Name,
					"port", port.Port,
					"err", err)
			}
		}

		if hasIPv6 {
			if err := nftables.DeleteIngressChains(true, portServiceID); err != nil {
				log.Warn("[wireguard] failed to delete IPv6 DNAT chains",
					"service", service.Name,
					"port", port.Port,
					"err", err)
			}
		}
	}
}

// getEndpoints retrieves the list of endpoints for a service
// For ExternalTrafficPolicy=Local, only local endpoints are returned
// For ExternalTrafficPolicy=Cluster, all endpoints are returned
func (w *wireguardWorker) getEndpoints(service *v1.Service, id string) ([]string, error) {
	var endpoints []string
	var err error
	if service.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyTypeLocal {
		endpoints, err = w.provider.GetLocalEndpoints(id, w.config)
	} else {
		endpoints, err = w.provider.GetAllEndpoints()
	}
	if err != nil {
		return nil, fmt.Errorf("[wireguard] failed to get endpoints: %w", err)
	}

	log.Debug("[wireguard] retrieved endpoints", "service", service.Name, "count", len(endpoints), "endpoints", endpoints)
	return endpoints, nil
}

// removeEgress is a no-op for WireGuard since egress is handled separately
func (w *wireguardWorker) removeEgress(service *v1.Service, lastKnownGoodEndpoint *string) {
	// WireGuard doesn't use egress in the same way as other modes
	log.Debug("[wireguard] removeEgress called (no-op)", "service", service.Name)
}

// delete removes all DNAT rules for a service
func (w *wireguardWorker) delete(ctx context.Context, service *v1.Service, id string) error {
	log.Info("[wireguard] deleting DNAT rules for service", "service", service.Name, "namespace", service.Namespace)

	w.clear(nil, nil, service)
	return nil
}

// setInstanceEndpointsStatus updates the endpoint status on the service instance
func (w *wireguardWorker) setInstanceEndpointsStatus(service *v1.Service, endpoints []string) error {
	hasEndpoints := len(endpoints) > 0

	log.Debug("[wireguard] setting instance endpoint status",
		"service", service.Name,
		"hasEndpoints", hasEndpoints,
		"endpointCount", len(endpoints))

	// Find the service instance
	for _, inst := range *w.instances {
		if inst.ServiceSnapshot == nil {
			continue
		}
		if inst.ServiceSnapshot.UID == service.UID {
			// Update the network status for all clusters
			for _, cluster := range inst.Clusters {
				for i := range cluster.Network {
					cluster.Network[i].SetHasEndpoints(hasEndpoints)
				}
			}
			log.Debug("[wireguard] updated instance endpoint status",
				"service", service.Name,
				"hasEndpoints", hasEndpoints)
			return nil
		}
	}

	log.Debug("[wireguard] instance not found for endpoint status update", "service", service.Name)
	return nil
}

func isIPv6Address(ip string) bool {
	// Strip CIDR notation if present before checking
	addr := utils.StripCIDR(ip)
	return utils.IsIPv6(addr)
}
