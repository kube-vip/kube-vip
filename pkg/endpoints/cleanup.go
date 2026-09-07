package endpoints

import (
	"context"
	"fmt"
	"sync"

	log "log/slog"

	v1 "k8s.io/api/core/v1"

	"github.com/kube-vip/kube-vip/pkg/bgp"
	"github.com/kube-vip/kube-vip/pkg/egress"
	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/nftables"
	"github.com/kube-vip/kube-vip/pkg/route"
	"github.com/kube-vip/kube-vip/pkg/utils"
	"github.com/kube-vip/kube-vip/pkg/wireguard"
)

// CleanupService stops one Service's datapath before its instance is detached.
// The service processor owns labels and instance bookkeeping; this package owns
// endpoint-dependent networking and waits for worker shutdown to complete.
func CleanupService(ctx context.Context, config *kubevip.Config, bgpServer *bgp.Server, routeMgr *route.Manager,
	tunnelMgr *wireguard.TunnelManager, serviceInstance *instance.Instance, remaining []*instance.Instance) error {
	if serviceInstance == nil || serviceInstance.ServiceSnapshot == nil {
		return nil
	}
	service := serviceInstance.ServiceSnapshot
	for _, serviceCluster := range serviceInstance.Clusters {
		for _, network := range serviceCluster.Network {
			network.SetHasEndpoints(false)
		}
	}

	if config.EnableBGP {
		ClearBGPHostsByInstance(ctx, serviceInstance, bgpServer)
	}
	if config.EnableRoutingTable {
		for _, err := range ClearRoutesByInstance(service, serviceInstance, &remaining, routeMgr) {
			log.Error("unable to clear routes", "err", err)
		}
	}

	internalNftablesEgress := service.Annotations[kubevip.EgressInternal] != "" || config.EgressWithNftables
	if service.Annotations[kubevip.Egress] == "true" && internalNftablesEgress {
		if err := nftables.DeleteSNATFromAllTables(string(serviceInstance.UID())); err != nil {
			log.Error("[service] nftables egress teardown", "service", service.Name, "err", err)
		}
	}

	sharedVIPs := sharedServiceVIPs(config, serviceInstance, remaining)
	for _, serviceCluster := range serviceInstance.Clusters {
		preserve := make([]string, 0, len(serviceCluster.Network))
		for _, network := range serviceCluster.Network {
			if _, shared := sharedVIPs[network.IP()]; shared {
				preserve = append(preserve, network.IP())
			}
		}
		if len(preserve) != 0 {
			serviceCluster.StopAndWaitPreserving(preserve...)
		} else {
			serviceCluster.StopAndWait()
		}
	}
	if err := serviceInstance.CleanupLinkAttachments(remaining...); err != nil {
		return fmt.Errorf("clean Service link attachments: %w", err)
	}
	if service.Annotations[kubevip.Egress] == "true" && !internalNftablesEgress && service.Annotations[kubevip.ActiveEndpoint] != "" {
		if err := egress.Teardown(service.Annotations[kubevip.ActiveEndpoint], service.Spec.LoadBalancerIP, service.Namespace,
			string(serviceInstance.UID()), service.Annotations, config.EgressWithNftables); err != nil {
			log.Error("[service] egress teardown", "err", err)
		}
	}
	if config.EnableWireguard {
		cleanupWireguardService(tunnelMgr, service)
	}
	return nil
}

// StartService starts a Service's cluster datapath after endpoint handling has
// made the Service eligible for activation.
func StartService(ctx context.Context, service *v1.Service, serviceInstance *instance.Instance, bgpServer *bgp.Server,
	wg *sync.WaitGroup) error {
	if serviceInstance == nil {
		return fmt.Errorf("missing service instance for %s/%s", service.Namespace, service.Name)
	}
	for index := range serviceInstance.VIPConfigs {
		if err := serviceInstance.Clusters[index].StartLoadBalancerService(ctx, serviceInstance.VIPConfigs[index], bgpServer,
			lease.ServiceNamespacedName(service), wg); err != nil {
			return fmt.Errorf("start load balancer: %w", err)
		}
	}
	return nil
}

func sharedServiceVIPs(config *kubevip.Config, serviceInstance *instance.Instance, remaining []*instance.Instance) map[string]struct{} {
	shared := make(map[string]struct{})
	if serviceInstance.ServiceSnapshot == nil ||
		serviceInstance.ServiceSnapshot.Spec.ExternalTrafficPolicy != v1.ServiceExternalTrafficPolicyTypeCluster {
		return shared
	}
	serviceNamespace, serviceLeaseName := lease.ServiceName(serviceInstance.ServiceSnapshot)
	serviceLease := lease.NewID(config.LeaderElectionType, serviceNamespace, serviceLeaseName).NamespacedName()
	addresses := serviceInstance.Addresses()
	for _, candidate := range remaining {
		candidateInfo, ok := candidate.CleanupInfo()
		if !ok || candidateInfo.ExternalTrafficPolicy != v1.ServiceExternalTrafficPolicyTypeCluster {
			continue
		}
		candidateNamespace, candidateLeaseName := lease.ServiceNameFor(candidateInfo.Namespace, candidateInfo.Name, candidateInfo.Lease)
		candidateLease := lease.NewID(config.LeaderElectionType, candidateNamespace, candidateLeaseName).NamespacedName()
		if config.EnableServicesElection && candidateLease != serviceLease {
			continue
		}
		for _, address := range candidate.Addresses() {
			for _, serviceAddress := range addresses {
				if address == serviceAddress {
					shared[address] = struct{}{}
				}
			}
		}
	}
	return shared
}

func cleanupWireguardService(tunnelMgr *wireguard.TunnelManager, service *v1.Service) {
	if tunnelMgr == nil {
		return
	}
	forEachServiceDNATChain(service, func(ipv6 bool, serviceID string) {
		if err := nftables.DeleteIngressChains(ipv6, serviceID); err != nil {
			log.Error("[wireguard] failed to delete DNAT chains", "ipv6", ipv6, "service", service.Name, "err", err)
		}
	})
	releaseWireguardServiceTunnels(tunnelMgr, service)
}

type wireguardTunnelReleaser interface {
	ReleaseTunnelForVIP(vip, owner string) error
}

func releaseWireguardServiceTunnels(tunnelMgr wireguardTunnelReleaser, service *v1.Service) {
	serviceIPs, _ := utils.FetchServiceIPs(service)
	for _, serviceIP := range serviceIPs {
		if err := tunnelMgr.ReleaseTunnelForVIP(serviceIP, string(service.UID)); err != nil {
			log.Error("[wireguard] failed to tear down tunnel", "service", service.Name, "vip", serviceIP, "err", err)
		}
	}
}
