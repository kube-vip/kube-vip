package endpoints

import (
	"context"
	"fmt"
	"sync"

	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/endpoints/providers"
	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/nftables"
	"github.com/kube-vip/kube-vip/pkg/utils"
	"github.com/kube-vip/kube-vip/pkg/wireguard"
	v1 "k8s.io/api/core/v1"
)

// wireguardWorker handles endpoint changes for WireGuard-based services
type wireguardWorker struct {
	config    *kubevip.Config
	provider  providers.Provider
	tunnelMgr *wireguard.TunnelManager
}

func newWireguardWorker(config *kubevip.Config, provider providers.Provider, tunnelMgr *wireguard.TunnelManager) *wireguardWorker {
	return &wireguardWorker{
		config:    config,
		provider:  provider,
		tunnelMgr: tunnelMgr,
	}
}

// processInstance updates nftables DNAT rules when endpoints change
// This is called by the endpoint watcher when endpoints are added/modified
func (w *wireguardWorker) processInstance(ctx context.Context, _ *sync.Map, service *v1.Service, inst *instance.Instance) error {
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
		w.clear(ctx, nil, nil, service, inst)
		return nil
	}

	// Get service VIPs
	serviceIPs, err := utils.FetchServiceIPs(service)
	if err != nil {
		return fmt.Errorf("failed to get service IPs: %w", err)
	}
	if len(service.Spec.Ports) != 0 {
		if err := w.ensureTunnels(service, serviceIPs); err != nil {
			return err
		}
	}
	w.clearDNAT(service)

	log.Info("[wireguard] updating DNAT rules for endpoint change",
		"service", service.Name,
		"namespace", service.Namespace,
		"endpoints", endpoints,
		"vips", serviceIPs)

	for _, port := range service.Spec.Ports {
		if port.Protocol != v1.ProtocolTCP && port.Protocol != v1.ProtocolUDP {
			continue
		}
		targetPort := w.provider.ResolvePort(port)
		log.Info("[wireguard] resolved port", "service", service.Name, "servicePort", port.Port, "targetPort", targetPort, "targetPortName", port.TargetPort.StrVal)

		targets := make([]nftables.DNATTarget, len(endpoints))
		for index, endpoint := range endpoints {
			targets[index] = nftables.DNATTarget{
				IP:   endpoint,
				Port: uint16(targetPort), //nolint:gosec // Port range validated by Kubernetes
			}
		}
		portServiceID, _ := wireguard.ServicePortIDs(service.Namespace, service.Name, port)

		for _, serviceIP := range serviceIPs {
			vipAddress := utils.StripCIDR(serviceIP)
			if w.tunnelMgr == nil {
				return fmt.Errorf("WireGuard tunnel manager not configured")
			}
			tunnelConfig := w.tunnelMgr.GetConfigForVIP(vipAddress)
			if tunnelConfig == nil {
				return fmt.Errorf("wireguard interface name not configured for VIP %s", vipAddress)
			}
			if err := nftables.ApplyDNAT(
				tunnelConfig.InterfaceName,
				vipAddress,
				uint16(port.Port), //nolint:gosec // Port range validated by Kubernetes
				targets,
				portServiceID,
				port.Protocol,
				service.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyTypeLocal,
				tunnelConfig.ListenPort,
			); err != nil {
				log.Error("[wireguard] failed to update DNAT rule", "service", service.Name, "vip", vipAddress, "port", port.Port, "err", err)
				continue
			}
		}
	}

	return nil
}

func (w *wireguardWorker) ensureTunnels(service *v1.Service, serviceIPs []string) error {
	if w.tunnelMgr == nil {
		return fmt.Errorf("WireGuard tunnel manager not configured")
	}
	if len(serviceIPs) == 0 {
		return fmt.Errorf("no service IPs found for service %s/%s", service.Namespace, service.Name)
	}
	var successCount int
	var lastErr error
	for _, serviceIP := range serviceIPs {
		if !w.tunnelMgr.HasConfigForVIP(serviceIP) {
			lastErr = fmt.Errorf("no WireGuard tunnel configuration found for VIP %s", serviceIP)
			continue
		}
		if err := w.tunnelMgr.AcquireTunnelForVIP(serviceIP, string(service.UID)); err != nil {
			lastErr = fmt.Errorf("bring up WireGuard tunnel for VIP %s: %w", serviceIP, err)
			continue
		}
		successCount++
	}
	if successCount == 0 {
		return fmt.Errorf("failed to setup WireGuard tunnel for any VIP in service %s/%s: %w", service.Namespace, service.Name, lastErr)
	}
	return nil
}

// clear removes DNAT rules when no endpoints are available
func (w *wireguardWorker) clear(_ context.Context, _ *sync.Map, _ *string, service *v1.Service, _ *instance.Instance) {
	w.clearDNAT(service)
}

func (w *wireguardWorker) clearDNAT(service *v1.Service) {
	log.Info("[wireguard] clearing DNAT rules (no endpoints)", "service", service.Name, "namespace", service.Namespace)
	forEachServiceDNATChain(service, func(ipv6 bool, serviceID string) {
		if err := nftables.DeleteIngressChains(ipv6, serviceID); err != nil {
			family := utils.IPv4Family
			if ipv6 {
				family = utils.IPv6Family
			}
			log.Warn("[wireguard] failed to delete DNAT chains", "family", family, "service", service.Name, "err", err)
		}
	})
}

func forEachServiceDNATChain(service *v1.Service, visit func(bool, string)) {
	if service == nil {
		return
	}
	serviceIPs, _ := utils.FetchServiceIPs(service)
	familyUnknown := len(serviceIPs) == 0
	hasIPv4, hasIPv6 := familyUnknown, familyUnknown
	for _, serviceIP := range serviceIPs {
		if isIPv6Address(serviceIP) {
			hasIPv6 = true
		} else {
			hasIPv4 = true
		}
	}

	for _, port := range service.Spec.Ports {
		if port.Protocol != v1.ProtocolTCP && port.Protocol != v1.ProtocolUDP {
			continue
		}
		portServiceID, legacyServiceID := wireguard.ServicePortIDs(service.Namespace, service.Name, port)
		// The legacy identifier is visited so an upgrade removes chains written
		// before rule IDs carried the protocol.
		for _, serviceID := range []string{portServiceID, legacyServiceID} {
			if hasIPv4 {
				visit(false, serviceID)
			}
			if hasIPv6 {
				visit(true, serviceID)
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
func (w *wireguardWorker) setInstanceEndpointsStatus(service *v1.Service, inst *instance.Instance, endpoints []string) error {
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
