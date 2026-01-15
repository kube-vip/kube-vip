package endpoints

import (
	"fmt"
	"net"

	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/bgp"
	"github.com/kube-vip/kube-vip/pkg/endpoints/providers"
	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	"github.com/kube-vip/kube-vip/pkg/utils"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/watch"
)

type Processor struct {
	config    *kubevip.Config
	provider  providers.Provider
	bgpServer *bgp.Server
	worker    endpointWorker
	instances *[]*instance.Instance
}

func NewEndpointProcessor(config *kubevip.Config, provider providers.Provider, bgpServer *bgp.Server,
	instances *[]*instance.Instance) *Processor {
	return &Processor{
		config:    config,
		provider:  provider,
		bgpServer: bgpServer,
		instances: instances,
		worker:    newEndpointWorker(config, provider, bgpServer, instances),
	}
}

func (p *Processor) AddOrModify(svcCtx *servicecontext.Context, event watch.Event,
	lastKnownGoodEndpoint *string, service *v1.Service, id string,
	serviceFunc func(*servicecontext.Context, *v1.Service) error) (bool, error) {

	var err error
	if err = p.provider.LoadObject(event.Object, svcCtx.Cancel); err != nil {
		return false, fmt.Errorf("[%s] error loading k8s object: %w", p.provider.GetLabel(), err)
	}

	endpoints, err := p.worker.getEndpoints(service, id)
	if err != nil {
		return false, err
	}

	if err := p.worker.setInstanceEndpointsStatus(service, endpoints); err != nil {
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
		if service.Annotations[kubevip.EgressIPv6] == "true" && !hasV6(endpoints) {
			return true, nil
		}

		p.updateLastKnownGoodEndpoint(svcCtx, lastKnownGoodEndpoint, endpoints, service)
		// start leader election if it's enabled and not already started
		if !svcCtx.IsActive && p.config.EnableServicesElection {
			go func() {
				startLeaderElection(svcCtx, service, serviceFunc)
			}()
		}

		// There are local endpoints available on the node
		if !p.config.EnableServicesElection && !p.config.EnableLeaderElection {
			if err := p.worker.processInstance(svcCtx, service); err != nil {
				return false, fmt.Errorf("failed to process non-empty instance: %w", err)
			}
		}
	} else {
		// There are no local endpoints
		p.worker.clear(svcCtx, lastKnownGoodEndpoint, service)
	}

	// Set the service accordingly
	p.updateAnnotations(service, lastKnownGoodEndpoint)

	log.Debug("watcher", "provider",
		p.provider.GetLabel(), "service name", service.Name, "namespace", service.Namespace, "endpoints", len(endpoints), "last endpoint", *lastKnownGoodEndpoint, "active leader election", svcCtx.IsActive)

	return false, nil
}

func (p *Processor) Delete(service *v1.Service, id string) error {
	if err := p.worker.delete(service, id); err != nil {
		return fmt.Errorf("[%s] error deleting service: %w", p.provider.GetLabel(), err)
	}
	return nil
}

func (p *Processor) updateLastKnownGoodEndpoint(svcCtx *servicecontext.Context, lastKnownGoodEndpoint *string, endpoints []string, service *v1.Service) {
	// if we haven't populated one, then do so
	family := utils.IPv4Family
	if service.Annotations[kubevip.EgressIPv6] == "true" {
		family = utils.IPv6Family
	}

	ep := getEndpoint(endpoints, family)

	if *lastKnownGoodEndpoint == "" {
		*lastKnownGoodEndpoint = ep
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
		if svcCtx.IsActive && (p.config.EnableServicesElection || p.config.EnableLeaderElection) {
			log.Warn("existing endpoint has been removed, restarting leaderElection", "provider", p.provider.GetLabel(), "endpoint", *lastKnownGoodEndpoint)
			// Stop the existing leaderElection
			svcCtx.Cancel()
		}
		// Set our active endpoint to an existing one
		*lastKnownGoodEndpoint = ep
	}
}

func (p *Processor) updateAnnotations(service *v1.Service, lastKnownGoodEndpoint *string) {
	// Set the service accordingly
	if service.Annotations[kubevip.Egress] == "true" {
		ip := net.ParseIP(*lastKnownGoodEndpoint)
		activeEndpointAnnotation := kubevip.ActiveEndpoint
		if ip.To4() == nil && !p.config.EnableEndpoints {
			activeEndpointAnnotation = kubevip.ActiveEndpointIPv6
		}
		service.Annotations[activeEndpointAnnotation] = *lastKnownGoodEndpoint
	}
}

func startLeaderElection(svcCtx *servicecontext.Context, service *v1.Service, serviceFunc func(*servicecontext.Context, *v1.Service) error) {
	// This is a blocking function, that will restart (in the event of failure)
	for {
		select {
		case <-svcCtx.Ctx.Done():
			return
		default:
			err := serviceFunc(svcCtx, service)
			if err != nil {
				log.Error(err.Error())
			}
			// if the context isn't cancelled restart
		}
	}
}

func hasV6(endpoints []string) bool {
	for _, e := range endpoints {
		ip := net.ParseIP(e)
		if ip != nil {
			if ip.To4() == nil {
				return true
			}
		}
	}
	return false
}

func getEndpoint(endpoints []string, family string) string {
	for _, e := range endpoints {
		ip := net.ParseIP(e)
		if family == utils.IPv4Family && ip.To4() != nil {
			return e
		}
		if family == utils.IPv6Family && ip.To4() == nil {
			return e
		}
	}
	return ""
}
