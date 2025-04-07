package endpoints

import (
	"context"
	"fmt"
	"net"
	"sync"

	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/bgp"
	"github.com/kube-vip/kube-vip/pkg/endpoints/providers"
	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/kubevip"

	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
)

type Processor struct {
	config                *kubevip.Config
	provider              providers.Provider
	bgpServer             *bgp.Server
	worker                endpointWorker
	instances             *[]*instance.Instance
	configuredLocalRoutes *sync.Map
}

func NewEndpointProcessor(config *kubevip.Config, provider providers.Provider, bgpServer *bgp.Server,
	instances *[]*instance.Instance, configuredLocalRoutes *sync.Map) *Processor {
	return &Processor{
		config:                config,
		provider:              provider,
		bgpServer:             bgpServer,
		instances:             instances,
		configuredLocalRoutes: configuredLocalRoutes,
		worker:                newEndpointWorker(config, provider, bgpServer, instances, configuredLocalRoutes),
	}
}

func (p *Processor) AddOrModify(ctx context.Context, event watch.Event, cancel context.CancelFunc,
	lastKnownGoodEndpoint *string, service *v1.Service, id string, leaderElectionActive *bool,
	serviceFunc func(context.Context, *v1.Service) error) (bool, error) {

	var err error
	if err = p.provider.LoadObject(event.Object, cancel); err != nil {
		return false, fmt.Errorf("[%s] error loading k8s object: %w", p.provider.GetLabel(), err)
	}

	endpoints, err := p.worker.getEndpoints(service, id)
	if err != nil {
		return false, err
	}

	if err := p.worker.setInstanceEndpointsStatus(service, len(endpoints) > 0); err != nil {
		log.Error("updating instance", "err", err)
	}

	// Find out if we have any local endpoints
	// if out endpoint is empty then populate it
	// if not, go through the endpoints and see if ours still exists
	// If we have a local endpoint then begin the leader Election, unless it's already running
	//

	// Check that we have local endpoints
	if len(endpoints) != 0 {
		// Ignore IPv4
		if service.Annotations[kubevip.EgressIPv6] == "true" && net.ParseIP(endpoints[0]).To4() != nil {
			return true, nil
		}

		p.updateLastKnownGoodEndpoint(lastKnownGoodEndpoint, endpoints, service, leaderElectionActive, cancel)

		// start leader election if it's enabled and not already started
		if !*leaderElectionActive && p.config.EnableServicesElection {
			go startLeaderElection(ctx, leaderElectionActive, service, serviceFunc)
		}

		isRouteConfigured, err := IsRouteConfigured(service.UID, p.configuredLocalRoutes)
		if err != nil {
			return false, fmt.Errorf("[%s] error while checking if route is configured: %w", p.provider.GetLabel(), err)
		}

		// There are local endpoints available on the node
		if !p.config.EnableServicesElection && !p.config.EnableLeaderElection && !isRouteConfigured {
			if err := p.worker.processInstance(ctx, service, leaderElectionActive); err != nil {
				return false, fmt.Errorf("failed to process non-empty instance: %w", err)
			}
		}
	} else {
		// There are no local endpoints
		p.worker.clear(lastKnownGoodEndpoint, service, cancel, leaderElectionActive)
	}

	// Set the service accordingly
	p.updateAnnotations(service, lastKnownGoodEndpoint)

	log.Debug("watcher", "provider",
		p.provider.GetLabel(), "service name", service.Name, "namespace", service.Namespace, "endpoints", len(endpoints), "last endpoint", lastKnownGoodEndpoint, "active leader election", leaderElectionActive)

	return false, nil
}

func (p *Processor) Delete(service *v1.Service, id string) error {
	if err := p.worker.delete(service, id); err != nil {
		return fmt.Errorf("[%s] error deleting service: %w", p.provider.GetLabel(), err)
	}
	return nil
}

func (p *Processor) updateLastKnownGoodEndpoint(lastKnownGoodEndpoint *string, endpoints []string, service *v1.Service, leaderElectionActive *bool, cancel context.CancelFunc) {
	// if we haven't populated one, then do so
	if *lastKnownGoodEndpoint == "" {
		*lastKnownGoodEndpoint = endpoints[0]
		return
	}

	// check out previous endpoint exists
	stillExists := false

	for x := range endpoints {
		if endpoints[x] == *lastKnownGoodEndpoint {
			stillExists = true
		}
	}
	// If the last endpoint no longer exists, we cancel our leader Election, and set another endpoint as last known good
	if !stillExists {
		p.worker.removeEgress(service, lastKnownGoodEndpoint)
		if *leaderElectionActive && (p.config.EnableServicesElection || p.config.EnableLeaderElection) {
			log.Warn("existing endpoint has been removed, restarting leaderElection", "provider", p.provider.GetLabel(), "endpoint", lastKnownGoodEndpoint)
			// Stop the existing leaderElection
			cancel()
			// disable last leaderElection flag
			*leaderElectionActive = false
		}
		// Set our active endpoint to an existing one
		*lastKnownGoodEndpoint = endpoints[0]
	}
}

func (p *Processor) updateAnnotations(service *v1.Service, lastKnownGoodEndpoint *string) {
	// Set the service accordingly
	if service.Annotations[kubevip.Egress] == "true" {
		activeEndpointAnnotation := kubevip.ActiveEndpoint

		if p.config.EnableEndpointSlices && p.provider.GetProtocol() == string(discoveryv1.AddressTypeIPv6) {
			activeEndpointAnnotation = kubevip.ActiveEndpointIPv6
		}
		service.Annotations[activeEndpointAnnotation] = *lastKnownGoodEndpoint
	}
}

func startLeaderElection(ctx context.Context, leaderElectionActive *bool, service *v1.Service, serviceFunc func(context.Context, *v1.Service) error) {
	// This is a blocking function, that will restart (in the event of failure)
	for {
		// if the context isn't cancelled restart
		if ctx.Err() != context.Canceled {
			*leaderElectionActive = true
			err := serviceFunc(ctx, service)
			if err != nil {
				log.Error(err.Error())
			}
			*leaderElectionActive = false
		} else {
			*leaderElectionActive = false
			break
		}
	}
}

func IsRouteConfigured(serviceUID types.UID, configuredLocalRoutes *sync.Map) (bool, error) {
	isConfigured := false
	value, ok := configuredLocalRoutes.Load(string(serviceUID))
	if ok {
		isConfigured, ok = value.(bool)
		if !ok {
			return false, fmt.Errorf("error converting configuredLocalRoute item to boolean value")
		}
	}

	return isConfigured, nil
}
