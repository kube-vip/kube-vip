package endpoints

import (
	"context"
	"fmt"
	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/bgp"
	"github.com/kube-vip/kube-vip/pkg/egress"
	"github.com/kube-vip/kube-vip/pkg/endpoints/providers"
	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	v1 "k8s.io/api/core/v1"
)

type endpointWorker interface {
	processInstance(svcCtx *servicecontext.Context, service *v1.Service, leaderElectionActive *bool) error
	clear(svcCtx *servicecontext.Context, lastKnownGoodEndpoint *string, service *v1.Service, cancel context.CancelFunc, leaderElectionActive *bool)
	getEndpoints(service *v1.Service, id string) ([]string, error)
	removeEgress(service *v1.Service, lastKnownGoodEndpoint *string)
	delete(service *v1.Service, id string) error
	setInstanceEndpointsStatus(service *v1.Service, endpoints []string) error
}

func newEndpointWorker(config *kubevip.Config, provider providers.Provider, bgpServer *bgp.Server, instances *[]*instance.Instance) endpointWorker {
	generic := newGeneric(config, provider, instances)

	if config.EnableRoutingTable {
		return newRoutingTable(generic)
	}
	if config.EnableBGP {
		return newBGP(generic, bgpServer)
	}

	return &generic
}

type generic struct {
	config    *kubevip.Config
	provider  providers.Provider
	instances *[]*instance.Instance
}

func newGeneric(config *kubevip.Config, provider providers.Provider, instances *[]*instance.Instance) generic {
	return generic{
		config:    config,
		provider:  provider,
		instances: instances,
	}
}

func (g *generic) processInstance(_ *servicecontext.Context, _ *v1.Service, _ *bool) error {
	return nil
}

func (g *generic) clear(_ *servicecontext.Context, lastKnownGoodEndpoint *string, service *v1.Service, cancel context.CancelFunc, leaderElectionActive *bool) {
	g.clearEgress(lastKnownGoodEndpoint, service, cancel, leaderElectionActive)
}

func (g *generic) clearEgress(lastKnownGoodEndpoint *string, service *v1.Service, cancel context.CancelFunc, leaderElectionActive *bool) {
	if *lastKnownGoodEndpoint != "" {
		log.Warn("existing  endpoint has been removed, no remaining endpoints for leaderElection", "provider", g.provider.GetLabel(), "endpoint", lastKnownGoodEndpoint)
		if err := egress.Teardown(*lastKnownGoodEndpoint, service.Spec.LoadBalancerIP, service.Namespace, string(service.UID), service.Annotations, g.config.EgressWithNftables); err != nil {
			log.Error("error removing redundant egress rules", "err", err)
		}

		*lastKnownGoodEndpoint = "" // reset endpoint
		if g.config.EnableServicesElection || g.config.EnableLeaderElection {
			cancel() // stop services watcher
		}
		*leaderElectionActive = false
	}
}

func (g *generic) getEndpoints(_ *v1.Service, id string) ([]string, error) {
	return g.getLocalEndpoints(id)
}

func (g *generic) getLocalEndpoints(id string) ([]string, error) {
	// Build endpoints
	var endpoints []string
	var err error
	if endpoints, err = g.provider.GetLocalEndpoints(id, g.config); err != nil {
		return nil, fmt.Errorf("[%s] error getting local endpoints: %w", g.provider.GetLabel(), err)
	}

	return endpoints, nil
}

func (g *generic) getAllEndpoints(service *v1.Service, id string) ([]string, error) {
	// Build endpoints
	var err error
	var endpoints []string
	if !g.config.EnableLeaderElection && !g.config.EnableServicesElection &&
		service.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyTypeCluster {
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

func (g *generic) delete(_ *v1.Service, _ string) error {
	return nil
}

func (g *generic) setInstanceEndpointsStatus(_ *v1.Service, _ []string) error {
	return nil
}
