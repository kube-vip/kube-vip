package services

import (
	"context"
	"fmt"
	log "log/slog"
	"reflect"
	"sync"

	"github.com/kube-vip/kube-vip/pkg/bgp"
	"github.com/kube-vip/kube-vip/pkg/endpoints"
	"github.com/kube-vip/kube-vip/pkg/endpoints/providers"
	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/vip"
	"github.com/prometheus/client_golang/prometheus"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
)

type Processor struct {
	config        *kubevip.Config
	lbClassFilter func(svc *v1.Service, config *kubevip.Config) bool

	// TODO: Fix the naming of these contexts

	// activeServiceLoadBalancer keeps track of services that already have a leaderElection in place
	activeServiceLoadBalancer map[string]context.Context

	// activeServiceLoadBalancer keeps track of services that already have a leaderElection in place
	activeServiceLoadBalancerCancel map[string]func()

	// activeService keeps track of services that already have a leaderElection in place
	activeService map[string]bool

	// watchedService keeps track of services that are already being watched
	watchedService map[string]bool

	// Keeps track of all running instances
	ServiceInstances []*instance.Instance

	mutex sync.Mutex

	bgpServer *bgp.Server

	// watchedService keeps track of routes that has been configured on the node
	configuredLocalRoutes sync.Map

	clientSet *kubernetes.Clientset

	rwClientSet *kubernetes.Clientset

	shutdownChan chan struct{}

	svcLocks map[string]*sync.Mutex

	// This is a prometheus counter used to count the number of events received
	// from the service watcher
	CountServiceWatchEvent *prometheus.CounterVec
}

func NewServicesProcessor(config *kubevip.Config, bgpServer *bgp.Server,
	clientSet *kubernetes.Clientset, rwClientSet *kubernetes.Clientset, shutdownChan chan struct{}) *Processor {
	lbClassFilterFunc := lbClassFilter
	if config.LoadBalancerClassLegacyHandling {
		lbClassFilterFunc = lbClassFilterLegacy
	}
	return &Processor{
		config:                          config,
		lbClassFilter:                   lbClassFilterFunc,
		activeServiceLoadBalancer:       make(map[string]context.Context),
		activeServiceLoadBalancerCancel: make(map[string]func()),
		activeService:                   make(map[string]bool),
		watchedService:                  make(map[string]bool),
		ServiceInstances:                []*instance.Instance{},
		bgpServer:                       bgpServer,
		clientSet:                       clientSet,
		rwClientSet:                     rwClientSet,
		shutdownChan:                    shutdownChan,
		svcLocks:                        make(map[string]*sync.Mutex),
		CountServiceWatchEvent: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "kube_vip",
			Subsystem: "manager",
			Name:      "all_services_events",
			Help:      "Count all events fired by the service watcher categorised by event type",
		}, []string{"type"}),
	}
}

func (p *Processor) AddOrModify(ctx context.Context, event watch.Event, serviceFunc func(context.Context, *v1.Service) error) (bool, error) {

	// log.Debugf("Endpoints for service [%s] have been Created or modified", s.service.ServiceName)
	svc, ok := event.Object.(*v1.Service)
	if !ok {
		return false, fmt.Errorf("unable to parse Kubernetes services from API watcher")
	}

	// We only care about LoadBalancer services
	if svc.Spec.Type != v1.ServiceTypeLoadBalancer {
		return true, nil
	}

	// Check if we ignore this service
	if svc.Annotations["kube-vip.io/ignore"] == "true" {
		log.Info("ignore annotation for kube-vip", "service name", svc.Name)
		return true, nil
	}

	// Select loadbalancer class filtering function

	// Check the loadBalancer class
	if p.filterLBClass(svc) {
		return true, nil
	}

	svcAddresses := instance.FetchServiceAddresses(svc)

	// We only care about LoadBalancer services that have been allocated an address
	if len(svcAddresses) <= 0 {
		return true, nil
	}

	// The modified event should only be triggered if the service has been modified (i.e. moved somewhere else)
	if event.Type == watch.Modified {
		for _, addr := range svcAddresses {
			// log.Debugf("(svcs) Retreiving local addresses, to ensure that this modified address doesn't exist: %s", addr)
			f, err := vip.GarbageCollect(p.config.Interface, addr)
			if err != nil {
				log.Error("(svcs) cleaning existing address error", "err", err)
			}
			if f {
				log.Warn("(svcs) already found existing config", "address", addr, "adapter", p.config.Interface)
			}
		}
		// This service has been modified, but it was also active..
		if p.activeService[string(svc.UID)] {

			i := instance.FindServiceInstance(svc, p.ServiceInstances)
			if i != nil {
				originalService := instance.FetchServiceAddresses(i.ServiceSnapshot)
				newService := instance.FetchServiceAddresses(svc)
				if !reflect.DeepEqual(originalService, newService) {

					// Calls the cancel function of the context
					if p.activeServiceLoadBalancerCancel[string(svc.UID)] != nil {
						log.Warn("(svcs) The load balancer has changed, cancelling original load balancer")
						p.activeServiceLoadBalancerCancel[string(svc.UID)]()
						<-p.activeServiceLoadBalancer[string(svc.UID)].Done()
						log.Warn("(svcs) waiting for load balancer to finish")
					}
					err := p.deleteService(svc.UID)
					if err != nil {
						log.Error("(svc) unable to remove", "service", svc.UID)
					}
					p.activeService[string(svc.UID)] = false
					p.watchedService[string(svc.UID)] = false
					delete(p.activeServiceLoadBalancer, string(svc.UID))
					p.configuredLocalRoutes.Store(string(svc.UID), false)
				}
				// in theory this should never fail

			}

		}
	}

	// Architecture walkthrough: (Had to do this as this code path is making my head hurt)

	// Is the service active (bool), if not then process this new service
	// Does this service use an election per service?
	//

	if !p.activeService[string(svc.UID)] {
		log.Debug("(svcs) has been added/modified with addresses", "service name", svc.Name, "ip", instance.FetchServiceAddresses(svc))

		p.activeServiceLoadBalancer[string(svc.UID)], p.activeServiceLoadBalancerCancel[string(svc.UID)] = context.WithCancel(ctx)

		if p.config.EnableServicesElection || // Service Election
			((p.config.EnableRoutingTable || p.config.EnableBGP) && // Routing table mode or BGP
				(!p.config.EnableLeaderElection && !p.config.EnableServicesElection)) { // No leaderelection or services election

			// If this load balancer Traffic Policy is "local"
			if svc.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyTypeLocal {

				// Start an endpoint watcher if we're not watching it already
				if !p.watchedService[string(svc.UID)] {
					// background the endpoint watcher
					go func() {
						if svc.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyTypeLocal {
							// Add Endpoint or EndpointSlices watcher
							var provider providers.Provider
							if !p.config.EnableEndpointSlices {
								provider = providers.NewEndpoints()
							} else {
								provider = providers.NewEndpointslices()
							}
							if err := p.watchEndpoint(p.activeServiceLoadBalancer[string(svc.UID)], p.config.NodeName, svc, provider); err != nil {
								log.Error(err.Error())
							}
						}
					}()

					if (p.config.EnableRoutingTable || p.config.EnableBGP) && (!p.config.EnableLeaderElection && !p.config.EnableServicesElection) {
						go func() {
							err := serviceFunc(p.activeServiceLoadBalancer[string(svc.UID)], svc)
							if err != nil {
								log.Error(err.Error())
							}
						}()
					}
					// We're now watching this service
					p.watchedService[string(svc.UID)] = true
				}
			} else if (p.config.EnableBGP || p.config.EnableRoutingTable) && (!p.config.EnableLeaderElection && !p.config.EnableServicesElection) {
				go func() {
					if svc.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyTypeCluster {
						// Add Endpoint watcher
						var provider providers.Provider
						if !p.config.EnableEndpointSlices {
							provider = providers.NewEndpoints()
						} else {
							provider = providers.NewEndpointslices()
						}
						if err := p.watchEndpoint(p.activeServiceLoadBalancer[string(svc.UID)], p.config.NodeName, svc, provider); err != nil {
							log.Error(err.Error())
						}
					}
				}()

				go func() {
					err := serviceFunc(p.activeServiceLoadBalancer[string(svc.UID)], svc)
					if err != nil {
						log.Error(err.Error())
					}
				}()
			} else {

				go func() {
					for {
						select {
						case <-p.activeServiceLoadBalancer[string(svc.UID)].Done():
							log.Warn("(svcs) restartable service watcher ending", "uid", svc.UID)
							return
						default:
							log.Info("(svcs) restartable service watcher starting", "uid", svc.UID)
							err := serviceFunc(p.activeServiceLoadBalancer[string(svc.UID)], svc)

							if err != nil {
								log.Error(err.Error())
							}
						}
					}

				}()
			}
		} else {
			// Increment the waitGroup before the service Func is called (Done is completed in there)
			err := serviceFunc(p.activeServiceLoadBalancer[string(svc.UID)], svc)
			if err != nil {
				log.Error(err.Error())
			}
		}
		p.activeService[string(svc.UID)] = true
	}

	return false, nil
}

func (p *Processor) Delete(event watch.Event) (bool, error) {
	svc, ok := event.Object.(*v1.Service)
	if !ok {
		return false, fmt.Errorf("unable to parse Kubernetes services from API watcher")
	}
	if p.activeService[string(svc.UID)] {

		// We only care about LoadBalancer services
		if svc.Spec.Type != v1.ServiceTypeLoadBalancer {
			return true, nil
		}

		// We can ignore this service
		if svc.Annotations["kube-vip.io/ignore"] == "true" {
			log.Info("(svcs)ignore annotation for kube-vip", "service name", svc.Name)
			return true, nil
		}

		isRouteConfigured, err := endpoints.IsRouteConfigured(svc.UID, &p.configuredLocalRoutes)
		if err != nil {
			return false, fmt.Errorf("error while checkig if route is configured: %w", err)
		}
		// If no leader election is enabled, delete routes here
		if !p.config.EnableLeaderElection && !p.config.EnableServicesElection &&
			p.config.EnableRoutingTable && isRouteConfigured {
			if errs := endpoints.ClearRoutes(svc, &p.ServiceInstances); len(errs) == 0 {
				p.configuredLocalRoutes.Store(string(svc.UID), false)
			}
		}

		// If this is an active service then and additional leaderElection will handle stopping
		err = p.deleteService(svc.UID)
		if err != nil {
			log.Error(err.Error())
		}

		// Calls the cancel function of the context
		if p.activeServiceLoadBalancerCancel[string(svc.UID)] != nil {
			p.activeServiceLoadBalancerCancel[string(svc.UID)]()
		}
		p.activeService[string(svc.UID)] = false
		p.watchedService[string(svc.UID)] = false
	}

	if (p.config.EnableBGP || p.config.EnableRoutingTable) && p.config.EnableLeaderElection && !p.config.EnableServicesElection {
		if p.config.EnableBGP {
			instance := instance.FindServiceInstance(svc, p.ServiceInstances)
			for _, vip := range instance.VipConfigs {
				vipCidr := fmt.Sprintf("%s/%s", vip.VIP, vip.VIPCIDR)
				if err := p.bgpServer.DelHost(vipCidr); err != nil {
					log.Error("error deleting host", "ip", vipCidr, "err", err)
				}
			}
		} else {
			endpoints.ClearRoutes(svc, &p.ServiceInstances)
		}
	}

	log.Info("(svcs) deleted", "service name", svc.Name, "namespace", svc.Namespace)

	return false, nil
}

func (p *Processor) filterLBClass(svc *v1.Service) bool {
	return p.lbClassFilter(svc, p.config)
}

func (p *Processor) Stop() {
	for _, instance := range p.ServiceInstances {
		for _, cluster := range instance.Clusters {
			cluster.Stop()
		}
	}
}
