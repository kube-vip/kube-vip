package manager

import (
	"context"
	"fmt"
	"net"

	log "log/slog"

	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/watch"
)

type Processor struct {
	sm       *Manager
	provider epProvider
	worker   EndpointWorker
}

func NewEndpointProcessor(sm *Manager, provider epProvider) *Processor {
	return &Processor{
		sm:       sm,
		provider: provider,
	}
}

func (p *Processor) AddModify(ctx context.Context, event watch.Event, cancel context.CancelFunc,
	lastKnownGoodEndpoint *string, service *v1.Service, id string, leaderElectionActive *bool) (bool, error) {

	var err error
	if err = p.provider.loadObject(event.Object, cancel); err != nil {
		return false, fmt.Errorf("[%s] error loading k8s object: %w", p.provider.getLabel(), err)
	}

	p.worker = NewEndpointWorker(p.sm, p.provider) // TODO: this could be created on kube-vip's start

	endpoints, err := p.worker.GetEndpoints(service, id)
	if err != nil {
		return false, err
	}

	if err := p.worker.SetInstanceEndpointsStatus(service, len(endpoints) > 0); err != nil {
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
		if service.Annotations[egressIPv6] == "true" && net.ParseIP(endpoints[0]).To4() != nil {
			return true, nil
		}

		p.updateLastKnownGoodEndpoint(lastKnownGoodEndpoint, endpoints, service, leaderElectionActive, cancel)

		// start leader election if it's enabled and not already started
		if !*leaderElectionActive && p.sm.config.EnableServicesElection {
			go startLeaderElection(ctx, p.sm, leaderElectionActive, service)
		}

		isRouteConfigured, err := isRouteConfigured(service.UID)
		if err != nil {
			return false, fmt.Errorf("[%s] error while checking if route is configured: %w", p.provider.getLabel(), err)
		}

		// There are local endpoints available on the node
		if !p.sm.config.EnableServicesElection && !p.sm.config.EnableLeaderElection && !isRouteConfigured {
			if err := p.worker.ProcessInstance(ctx, service, leaderElectionActive); err != nil {
				return false, fmt.Errorf("failed to process non-empty instance: %w", err)
			}
		}
	} else {
		// There are no local endpoints
		p.worker.Clear(lastKnownGoodEndpoint, service, cancel, leaderElectionActive)
	}

	// Set the service accordingly
	p.updateAnnotations(service, lastKnownGoodEndpoint)

	log.Debug("watcher", "provider",
		p.provider.getLabel(), "service name", service.Name, "namespace", service.Namespace, "endpoints", len(endpoints), "last endpoint", lastKnownGoodEndpoint, "active leader election", leaderElectionActive)

	return false, nil
}

func (p *Processor) Delete(service *v1.Service, id string) error {
	if err := p.worker.Delete(service, id); err != nil {
		return fmt.Errorf("[%s] error deleting service: %w", p.provider.getLabel(), err)
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
		p.worker.RemoveEgress(service, lastKnownGoodEndpoint)
		if *leaderElectionActive && (p.sm.config.EnableServicesElection || p.sm.config.EnableLeaderElection) {
			log.Warn("existing endpoint has been removed, restarting leaderElection", "provider", p.provider.getLabel(), "endpoint", lastKnownGoodEndpoint)
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
	if service.Annotations[egress] == "true" {
		activeEndpointAnnotation := activeEndpoint

		if p.sm.config.EnableEndpointSlices && p.provider.getProtocol() == string(discoveryv1.AddressTypeIPv6) {
			activeEndpointAnnotation = activeEndpointIPv6
		}
		service.Annotations[activeEndpointAnnotation] = *lastKnownGoodEndpoint
	}
}

func startLeaderElection(ctx context.Context, sm *Manager, leaderElectionActive *bool, service *v1.Service) {
	// This is a blocking function, that will restart (in the event of failure)
	for {
		// if the context isn't cancelled restart
		if ctx.Err() != context.Canceled {
			*leaderElectionActive = true
			err := sm.StartServicesLeaderElection(ctx, service)
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
