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
	config         *kubevip.Config
	provider       providers.Provider
	bgpServer      *bgp.Server
	leaseMgr       *lease.Manager
	tunnelMgr      *wireguard.TunnelManager
	applyDNAT      func(string, string, uint16, []nftables.DNATTarget, string, v1.Protocol, bool, int) error
	deleteDNAT     func(bool, string) error
	deleteDNATRule func(string, bool, string) error
}

func newWireguardWorker(config *kubevip.Config, provider providers.Provider, bgpServer *bgp.Server,
	leaseMgr *lease.Manager, tunnelMgr *wireguard.TunnelManager) *wireguardWorker {
	return &wireguardWorker{
		config:    config,
		provider:  provider,
		bgpServer: bgpServer,
		leaseMgr:  leaseMgr,
		tunnelMgr: tunnelMgr,
	}
}

// processInstance updates nftables DNAT rules when endpoints change
// This is called by the endpoint watcher when endpoints are added/modified
func (w *wireguardWorker) processInstance(svcCtx *servicecontext.Context, service *v1.Service, inst *instance.Instance) error {
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
		w.clear(svcCtx, nil, service, inst)
		return nil
	}

	// Find the service processor to call updateServiceWireguardEndpoints
	// Note: This requires access to the service processor which we don't have here
	// So we'll recreate the DNAT rules directly

	// Get service VIPs
	serviceIPs, err := utils.FetchServiceIPs(service)
	if err != nil {
		log.Error("[wireguard] failed to get service IPs; clearing DNAT rules",
			"service", service.Name,
			"namespace", service.Namespace,
			"err", err)
		w.clearDNAT(service)
		return nil
	}

	log.Info("[wireguard] updating DNAT rules for endpoint change",
		"service", service.Name,
		"namespace", service.Namespace,
		"endpoints", endpoints,
		"vips", serviceIPs)

	type dnatReplacement struct {
		wgInterface string
		vipAddr     string
		port        v1.ServicePort
		targets     []nftables.DNATTarget
		serviceID   string
		legacyID    string
		listenPort  int
	}
	replacements := make([]dnatReplacement, 0, len(service.Spec.Ports)*len(serviceIPs))

	// Validate the complete replacement before changing any existing rules.
	for _, port := range service.Spec.Ports {
		// Determine target port (resolve named ports if necessary)
		targetPort := w.provider.ResolvePort(port)
		log.Info("[wireguard] resolved port", "service", service.Name, "servicePort", port.Port, "targetPort", targetPort, "targetPortName", port.TargetPort.StrVal)

		portServiceID, legacyServiceID := wireguard.ServicePortIDs(service.Namespace, service.Name, port)
		for _, vip := range serviceIPs {
			// Strip CIDR notation if present
			vipAddr := utils.StripCIDR(vip)
			targets := make([]nftables.DNATTarget, 0, len(endpoints))
			for _, ep := range endpoints {
				if isIPv6Address(ep) == isIPv6Address(vipAddr) {
					targets = append(targets, nftables.DNATTarget{IP: ep, Port: uint16(targetPort)}) //nolint:gosec // Port range validated by Kubernetes
				}
			}

			// Get WireGuard interface name from TunnelManager for this VIP
			if w.tunnelMgr == nil {
				log.Error("[wireguard] TunnelManager not configured; cannot update DNAT rules",
					"service", service.Name,
					"namespace", service.Namespace)
				w.clearDNAT(service)
				return nil
			}
			tunnelConfig := w.tunnelMgr.GetConfigForVIP(vipAddr)
			if tunnelConfig == nil {
				log.Error("[wireguard] WireGuard interface name not configured; cannot update DNAT rules",
					"service", service.Name,
					"namespace", service.Namespace,
					"vip", vipAddr)
				w.clearDNAT(service)
				return nil
			}
			replacements = append(replacements, dnatReplacement{
				wgInterface: tunnelConfig.InterfaceName,
				vipAddr:     vipAddr,
				port:        port,
				targets:     targets,
				serviceID:   portServiceID,
				legacyID:    legacyServiceID,
				listenPort:  tunnelConfig.ListenPort,
			})
		}
	}

	for _, replacement := range replacements {

		log.Info("[wireguard] applying DNAT rule with load balancing",
			"service", service.Name,
			"vip", replacement.vipAddr,
			"interface", replacement.wgInterface,
			"sourcePort", replacement.port.Port,
			"targets", replacement.targets,
			"chainID", replacement.serviceID)

		// Apply the DNAT rule with load balancing across all endpoints
		// localEndpoint=true when using ExternalTrafficPolicy=Local, which preserves client source IP
		isLocalEndpoint := service.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyTypeLocal
		applyDNAT := w.applyDNAT
		if applyDNAT == nil {
			applyDNAT = nftables.ApplyDNAT
		}
		deleteDNATRule := w.deleteDNATRule
		if deleteDNATRule == nil {
			deleteDNATRule = nftables.DeleteDNATRule
		}
		if err := deleteDNATRule(replacement.wgInterface, isIPv6Address(replacement.vipAddr), replacement.legacyID); err != nil {
			log.Warn("[wireguard] failed to clear legacy DNAT rule",
				"service", service.Name,
				"vip", replacement.vipAddr,
				"port", replacement.port.Port,
				"err", err)
		}
		err := applyDNAT(
			replacement.wgInterface,
			replacement.vipAddr,
			uint16(replacement.port.Port), //nolint:gosec // Port range validated by Kubernetes
			replacement.targets,
			replacement.serviceID,
			replacement.port.Protocol,
			isLocalEndpoint,
			replacement.listenPort,
		)
		if err != nil {
			log.Error("[wireguard] failed to update DNAT rule",
				"service", service.Name,
				"vip", replacement.vipAddr,
				"port", replacement.port.Port,
				"err", err)
			if deleteErr := deleteDNATRule(replacement.wgInterface, isIPv6Address(replacement.vipAddr), replacement.serviceID); deleteErr != nil {
				log.Warn("[wireguard] failed to clear DNAT rule after update failure",
					"service", service.Name,
					"vip", replacement.vipAddr,
					"port", replacement.port.Port,
					"err", deleteErr)
			}
			continue
		}

		log.Debug("[wireguard] DNAT rule updated successfully",
			"service", service.Name,
			"vip", replacement.vipAddr,
			"port", replacement.port.Port,
			"targetCount", len(replacement.targets))
	}

	return nil
}

// clear removes DNAT rules when no endpoints are available
func (w *wireguardWorker) clear(svcCtx *servicecontext.Context, lastKnownGoodEndpoint *string, service *v1.Service, _ *instance.Instance) {
	log.Info("[wireguard] clearing DNAT rules (no endpoints)", "service", service.Name, "namespace", service.Namespace)
	w.clearDNAT(service)
}

func (w *wireguardWorker) clearDNAT(service *v1.Service) {
	// Get service IPs to determine IPv4 vs IPv6
	serviceIPs, _ := utils.FetchServiceIPs(service)
	familyUnknown := len(serviceIPs) == 0
	hasIPv4, hasIPv6 := familyUnknown, familyUnknown
	for _, vip := range serviceIPs {
		if isIPv6Address(vip) {
			hasIPv6 = true
		} else {
			hasIPv4 = true
		}
	}
	deleteDNAT := w.deleteDNAT
	if deleteDNAT == nil {
		deleteDNAT = nftables.DeleteIngressChains
	}

	// Delete DNAT chains for each port
	for _, port := range service.Spec.Ports {
		portServiceID, legacyServiceID := wireguard.ServicePortIDs(service.Namespace, service.Name, port)
		for _, serviceID := range []string{portServiceID, legacyServiceID} {
			if hasIPv4 {
				if err := deleteDNAT(false, serviceID); err != nil {
					log.Warn("[wireguard] failed to delete IPv4 DNAT chains",
						"service", service.Name,
						"port", port.Port,
						"err", err)
				}
			}

			if hasIPv6 {
				if err := deleteDNAT(true, serviceID); err != nil {
					log.Warn("[wireguard] failed to delete IPv6 DNAT chains",
						"service", service.Name,
						"port", port.Port,
						"err", err)
				}
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

// setInstanceEndpointsStatus updates the endpoint status on the service instance
func (w *wireguardWorker) setInstanceEndpointsStatus(_ context.Context, service *v1.Service, inst *instance.Instance, endpoints []string) error {
	hasEndpoints := len(endpoints) > 0

	log.Debug("[wireguard] setting instance endpoint status",
		"service", service.Name,
		"hasEndpoints", hasEndpoints,
		"endpointCount", len(endpoints))

	if inst != nil {
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

	log.Debug("[wireguard] instance not found for endpoint status update", "service", service.Name)
	return nil
}

func isIPv6Address(ip string) bool {
	// Strip CIDR notation if present before checking
	addr := utils.StripCIDR(ip)
	return utils.IsIPv6(addr)
}
