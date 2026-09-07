package endpoints

import (
	"context"
	"fmt"
	log "log/slog"
	"sync"

	"github.com/kube-vip/kube-vip/pkg/bgp"
	"github.com/kube-vip/kube-vip/pkg/egress"
	"github.com/kube-vip/kube-vip/pkg/endpoints/providers"
	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/route"
	"github.com/kube-vip/kube-vip/pkg/wireguard"
	v1 "k8s.io/api/core/v1"
)

type endpointWorker interface {
	processInstance(ctx context.Context, configuredNetworks *sync.Map, service *v1.Service, inst *instance.Instance) error
	clear(ctx context.Context, configuredNetworks *sync.Map, lastKnownGoodEndpoint *string, service *v1.Service, inst *instance.Instance)
	getEndpoints(service *v1.Service, id string) ([]string, error)
	removeEgress(service *v1.Service, lastKnownGoodEndpoint *string)
	setInstanceEndpointsStatus(service *v1.Service, inst *instance.Instance, endpoints []string) error
}

func newEndpointWorker(config *kubevip.Config, provider providers.Provider, bgpServer *bgp.Server,
	leaseMgr *lease.Manager, tunnelMgr *wireguard.TunnelManager, routeMgr *route.Manager) endpointWorker {
	generic := newGeneric(config, provider, leaseMgr)

	if config.EnableWireguard {
		return newWireguardWorker(config, provider, tunnelMgr)
	}
	if config.EnableRoutingTable {
		return newRoutingTable(generic, routeMgr)
	}
	if config.EnableBGP {
		return newBGP(generic, bgpServer)
	}

	return &generic
}

type generic struct {
	config   *kubevip.Config
	provider providers.Provider
	leaseMgr *lease.Manager
}

func newGeneric(config *kubevip.Config, provider providers.Provider, leaseMgr *lease.Manager) generic {
	return generic{
		config:   config,
		provider: provider,
		leaseMgr: leaseMgr,
	}
}

func (g *generic) processInstance(_ context.Context, _ *sync.Map, _ *v1.Service, _ *instance.Instance) error {
	return nil
}

func (g *generic) clear(_ context.Context, _ *sync.Map, lastKnownGoodEndpoint *string, service *v1.Service, _ *instance.Instance) {
	g.clearEgress(lastKnownGoodEndpoint, service)
}

func (g *generic) clearEgress(lastKnownGoodEndpoint *string, service *v1.Service) {
	if *lastKnownGoodEndpoint != "" {
		log.Warn("existing endpoint has been removed, no remaining endpoints for leaderElection", "provider", g.provider.GetLabel(), "endpoint", lastKnownGoodEndpoint)
		if err := egress.Teardown(*lastKnownGoodEndpoint, service.Spec.LoadBalancerIP, service.Namespace, string(service.UID), service.Annotations, g.config.EgressWithNftables); err != nil {
			log.Error("error removing redundant egress rules", "err", err)
		}

		*lastKnownGoodEndpoint = "" // reset endpoint
	}
}

func (g *generic) getEndpoints(service *v1.Service, id string) ([]string, error) {
	return g.getAllEndpoints(service, id)
}

func (g *generic) getAllEndpoints(service *v1.Service, id string) ([]string, error) {
	// Build endpoints
	var err error
	var endpoints []string
	if service.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyTypeCluster {
		if endpoints, err = g.provider.GetAllEndpoints(); err != nil {
			return nil, fmt.Errorf("[%s] error getting all endpoints: %w", g.provider.GetLabel(), err)
		}
	} else {
		if endpoints, err = g.provider.GetLocalEndpoints(id, g.config); err != nil {
			return nil, fmt.Errorf("[%s] error getting local endpoints: %w", g.provider.GetLabel(), err)
		}
	}

	return endpoints, nil
}

func (g *generic) removeEgress(_ *v1.Service, _ *string) {
}

func (g *generic) setInstanceEndpointsStatus(_ *v1.Service, _ *instance.Instance, _ []string) error {
	return nil
}
