package endpoints

import (
	"context"
	"fmt"
	"net"

	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/bgp"
	"github.com/kube-vip/kube-vip/pkg/endpoints/providers"
	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	"github.com/kube-vip/kube-vip/pkg/utils"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
)

type Processor struct {
	config           *kubevip.Config
	provider         providers.Provider
	bgpServer        *bgp.Server
	worker           endpointWorker
	instances        *[]*instance.Instance
	clientSet        *kubernetes.Clientset
	egressUpdateFunc func(context.Context, *v1.Service) error
}

func NewEndpointProcessor(config *kubevip.Config, provider providers.Provider, bgpServer *bgp.Server,
	instances *[]*instance.Instance, leaseMgr *lease.Manager, clientSet *kubernetes.Clientset,
	egressUpdateFunc func(context.Context, *v1.Service) error) *Processor {
	return &Processor{
		config:           config,
		provider:         provider,
		bgpServer:        bgpServer,
		instances:        instances,
		clientSet:        clientSet,
		egressUpdateFunc: egressUpdateFunc,
		worker:           newEndpointWorker(config, provider, bgpServer, instances, leaseMgr),
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
		svcCtx.HasEndpoints.Store(true)
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
		svcCtx.HasEndpoints.Store(false)
		// There are no local endpoints
		p.worker.clear(svcCtx, lastKnownGoodEndpoint, service)
	}

	// Set the service accordingly
	p.updateAnnotations(service, lastKnownGoodEndpoint)

	log.Debug("watcher", "provider",
		p.provider.GetLabel(), "service name", service.Name, "namespace", service.Namespace, "endpoints", len(endpoints), "last endpoint", *lastKnownGoodEndpoint, "active leader election", svcCtx.IsActive)

	return false, nil
}

func (p *Processor) Delete(ctx context.Context, service *v1.Service, id string) error {
	if err := p.worker.delete(ctx, service, id); err != nil {
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
			if svcCtx.Lease != nil {
				svcCtx.Lease.Cancel()
			}
		}
		// Set our active endpoint to an existing one
		*lastKnownGoodEndpoint = ep
	}
}

func (p *Processor) updateAnnotations(service *v1.Service, lastKnownGoodEndpoint *string) {
	// Set the service accordingly
	if service.Annotations[kubevip.Egress] == "true" {
		ip := net.ParseIP(*lastKnownGoodEndpoint)

		// Store old values from ServiceSnapshot to detect if annotation actually changed
		// We use the ServiceSnapshot instead of the service parameter because the service parameter
		// may have stale annotations if the last update failed
		var oldEndpoint, oldEndpointIPv6 string
		if p.instances != nil {
			serviceInstance := instance.FindServiceInstance(service, *p.instances)
			if serviceInstance != nil {
				oldEndpoint = serviceInstance.ServiceSnapshot.Annotations[kubevip.ActiveEndpoint]
				oldEndpointIPv6 = serviceInstance.ServiceSnapshot.Annotations[kubevip.ActiveEndpointIPv6]
			}
		}
		// Fall back to service annotations if we couldn't find the instance
		if oldEndpoint == "" && oldEndpointIPv6 == "" {
			oldEndpoint = service.Annotations[kubevip.ActiveEndpoint]
			oldEndpointIPv6 = service.Annotations[kubevip.ActiveEndpointIPv6]
		}

		// Determine which annotation to update based on IP version
		var endpoint, endpointIPv6 string
		if ip.To4() == nil && !p.config.EnableEndpoints {
			// IPv6
			endpointIPv6 = *lastKnownGoodEndpoint
			endpoint = oldEndpoint // Preserve existing IPv4 if any
		} else {
			// IPv4
			endpoint = *lastKnownGoodEndpoint
			endpointIPv6 = oldEndpointIPv6 // Preserve existing IPv6 if any
		}

		// Check if annotation actually changed
		annotationChanged := (oldEndpoint != endpoint) || (oldEndpointIPv6 != endpointIPv6)
		if !annotationChanged {
			return // Nothing to do
		}

		// Persist to Kubernetes
		ctx := context.Background()
		var provider providers.Provider
		if p.config.EnableEndpoints {
			provider = providers.NewEndpoints()
		} else {
			provider = providers.NewEndpointslices()
		}

		if err := provider.UpdateServiceAnnotation(ctx, endpoint, endpointIPv6, service, p.clientSet); err != nil {
			log.Warn("failed to update service annotation", "service", service.Name, "namespace", service.Namespace, "err", err)
			return
		}

		log.Debug("updated active endpoint annotation", "service", service.Name, "namespace", service.Namespace, "endpoint", *lastKnownGoodEndpoint)

		// Trigger egress reconfiguration
		// For services with leader election, the service watcher doesn't process Modified events
		// after initial setup, so we need to directly call the update function
		if p.egressUpdateFunc != nil {
			// Create a copy of service with updated annotations
			svcCopy := service.DeepCopy()
			svcCopy.Annotations[kubevip.ActiveEndpoint] = endpoint
			svcCopy.Annotations[kubevip.ActiveEndpointIPv6] = endpointIPv6

			if err := p.egressUpdateFunc(ctx, svcCopy); err != nil {
				log.Error("failed to reconfigure egress", "service", service.Name, "namespace", service.Namespace, "err", err)
			}
		}
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
			if !svcCtx.HasEndpoints.Load() {
				log.Debug("there are no available endpoints for this service, exiting watch", "service", service.Name, "uid", service.UID)
				return
			}
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
