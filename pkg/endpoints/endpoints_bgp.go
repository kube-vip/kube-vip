package endpoints

import (
	"context"
	"errors"
	"fmt"
	log "log/slog"
	"net/netip"
	"slices"
	"sync"

	"github.com/kube-vip/kube-vip/pkg/bgp"
	"github.com/kube-vip/kube-vip/pkg/endpoints/providers"
	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/lease"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

type bgpRouteManager interface {
	bgp.BGPManager
	UpdateRoute(context.Context, bgp.Route) error
}

type BGP struct {
	generic
	bgpServer bgpRouteManager
	client    kubernetes.Interface
}

func newBGP(generic generic, bgpServer *bgp.Server) endpointWorker {
	return &BGP{
		generic:   generic,
		bgpServer: bgpServer,
	}
}

func (b *BGP) processInstance(ctx context.Context, configuredNetworks *sync.Map, service *v1.Service, inst *instance.Instance) error {
	if service.Annotations[kubevip.BGPForwardingNode] == "true" {
		return b.processForwardingNode(ctx, configuredNetworks, service, inst)
	}
	if inst != nil {
		for _, cluster := range inst.Clusters {
			for i := range cluster.Network {
				if _, configured := configuredNetworks.Load(cluster.Network[i].IP()); !configured {
					log.Debug("attempting to advertise BGP service", "provider", b.provider.GetLabel(), "ip", cluster.Network[i].IP())
					err := b.bgpServer.AddHost(ctx, cluster.Network[i].CIDR(), lease.ServiceNamespacedName(service))
					if err != nil {
						log.Error("error adding BGP host", "provider", b.provider.GetLabel(), "err", err)
					} else {
						log.Info("added BGP host", "provider",
							b.provider.GetLabel(), "ip", cluster.Network[i].CIDR(), "service name", service.Name, "namespace", service.Namespace)
						configuredNetworks.Store(cluster.Network[i].IP(), true)
					}
				}
			}
		}
	}
	return nil
}

func (b *BGP) clear(ctx context.Context, configuredNetworks *sync.Map, lastKnownGoodEndpoint *string, service *v1.Service, inst *instance.Instance) {
	if !kubevip.UsesServiceElection(b.config, service) && !b.config.EnableLeaderElection {
		// If BGP mode is enabled - routes should be deleted
		if inst != nil {
			for _, cluster := range inst.Clusters {
				for i := range cluster.Network {
					err := b.bgpServer.DelHost(ctx, cluster.Network[i].CIDR(), lease.ServiceNamespacedName(service))
					if err != nil {
						log.Error("deleting BGP host", "provider", b.provider.GetLabel(), "ip", cluster.Network[i].IP(), "err", err)
					} else {
						log.Info("deleted BGP host", "provider",
							b.provider.GetLabel(), "ip", cluster.Network[i].IP(), "service name", service.Name, "namespace", service.Namespace)
						configuredNetworks.Delete(cluster.Network[i].IP())
					}
				}
			}

		}
	}

	b.clearEgress(lastKnownGoodEndpoint, service)
}

func (b *BGP) getEndpoints(service *v1.Service, id string) ([]string, error) {
	if service.Annotations[kubevip.BGPForwardingNode] == "true" {
		provider, ok := b.provider.(providers.EndpointNodeProvider)
		if !ok {
			return nil, fmt.Errorf("endpoint provider does not expose node names")
		}
		var endpoints []string
		for _, addresses := range provider.GetEndpointNodes() {
			endpoints = append(endpoints, addresses...)
		}
		return endpoints, nil
	}
	return b.getAllEndpoints(service, id)
}

func (b *BGP) processForwardingNode(ctx context.Context, configuredNetworks *sync.Map, service *v1.Service, inst *instance.Instance) error {
	if inst == nil {
		return nil
	}
	provider, ok := b.provider.(providers.EndpointNodeProvider)
	if !ok || b.client == nil {
		return fmt.Errorf("BGP forwarding-node mode requires endpoint node information and a Kubernetes client")
	}
	nodes := provider.GetEndpointNodes()
	// Sorting ensures every speaker chooses the same node regardless of watch
	// order or EndpointSlice map iteration order.
	names := make([]string, 0, len(nodes))
	for name := range nodes {
		names = append(names, name)
	}
	slices.Sort(names)
	var result error
	for _, cluster := range inst.Clusters {
		for _, network := range cluster.Network {
			vip, err := netip.ParseAddr(network.IP())
			if err != nil {
				return err
			}
			nextHop, err := b.forwardingNodeAddress(ctx, names, nodes, vip.Is4())
			if err != nil {
				// Never leave a route pointing at a node whose endpoint vanished.
				if deleteErr := b.bgpServer.DelHost(ctx, network.CIDR(), lease.ServiceNamespacedName(service)); deleteErr != nil {
					err = fmt.Errorf("%w; withdrawing stale route: %v", err, deleteErr)
				} else {
					configuredNetworks.Delete(network.IP())
				}
				result = errors.Join(result, err)
				continue
			}
			if err := b.bgpServer.UpdateRoute(ctx, bgp.Route{
				Prefix: network.CIDR(), NextHop: nextHop, Object: lease.ServiceNamespacedName(service),
			}); err != nil {
				result = errors.Join(result, err)
				continue
			}
			configuredNetworks.Store(network.IP(), true)
		}
	}
	return result
}

func (b *BGP) forwardingNodeAddress(ctx context.Context, names []string, nodes map[string][]string, ipv4 bool) (string, error) {
	for _, name := range names {
		matching := false
		for _, address := range nodes[name] {
			ip, err := netip.ParseAddr(address)
			if err == nil && ip.Is4() == ipv4 {
				matching = true
				break
			}
		}
		if !matching {
			continue
		}
		node, err := b.client.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return "", fmt.Errorf("resolving BGP forwarding node %s: %w", name, err)
		}
		var addresses []string
		for _, address := range node.Status.Addresses {
			ip, err := netip.ParseAddr(address.Address)
			if address.Type == v1.NodeInternalIP && err == nil && ip.IsGlobalUnicast() && ip.Is4() == ipv4 {
				addresses = append(addresses, ip.String())
			}
		}
		slices.Sort(addresses)
		if len(addresses) > 0 {
			return addresses[0], nil
		}
	}
	return "", fmt.Errorf("no endpoint node has a matching InternalIP (IPv4=%t)", ipv4)
}

func (b *BGP) setInstanceEndpointsStatus(_ *v1.Service, _ *instance.Instance, _ []string) error {
	return nil
}

func ClearBGPHosts(ctx context.Context, service *v1.Service, instances *[]*instance.Instance, bgpServer *bgp.Server) {
	if instance := instance.FindServiceInstance(service, *instances); instance != nil {
		ClearBGPHostsByInstance(ctx, instance, bgpServer)
	}
}

func ClearBGPHostsByInstance(ctx context.Context, instance *instance.Instance, bgpServer *bgp.Server) {
	if instance == nil {
		log.Error("failed to clear BGP host for nil instance")
		return
	}
	for _, cluster := range instance.Clusters {
		for i := range cluster.Network {
			network := cluster.Network[i]
			err := bgpServer.DelHost(ctx, network.CIDR(), lease.ServiceNamespacedName(instance.ServiceSnapshot))
			if err != nil {
				log.Error("[endpoint] error deleting BGP host", "err", err)
			} else {
				log.Debug("[endpoint] deleted BGP host", "ip",
					network.CIDR(), "service name", instance.ServiceSnapshot.Name, "namespace", instance.ServiceSnapshot.Namespace)
			}
		}
	}
}
