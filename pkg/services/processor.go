package services

import (
	"context"
	"fmt"
	log "log/slog"
	"reflect"
	"sync"

	"github.com/kube-vip/kube-vip/pkg/arp"
	"github.com/kube-vip/kube-vip/pkg/bgp"
	"github.com/kube-vip/kube-vip/pkg/endpoints"
	"github.com/kube-vip/kube-vip/pkg/endpoints/providers"
	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/networkinterface"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	"github.com/kube-vip/kube-vip/pkg/vip"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/vishvananda/netlink"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
)

type Processor struct {
	config        *kubevip.Config
	lbClassFilter func(svc *v1.Service, config *kubevip.Config) bool
	svcMap        sync.Map

	// Keeps track of all running instances
	ServiceInstances []*instance.Instance

	mutex     sync.Mutex
	bgpServer *bgp.Server

	clientSet   *kubernetes.Clientset
	rwClientSet *kubernetes.Clientset

	shutdownChan chan struct{}

	// This is a prometheus counter used to count the number of events received
	// from the service watcher
	CountServiceWatchEvent *prometheus.CounterVec

	intfMgr *networkinterface.Manager
	arpMgr  *arp.Manager

	leaseMgr *lease.Manager

	// nodeLabelManager is the manager for the node labels
	nodeLabelManager labelManager
}

// labelManager is the interface for the node label manager to add/remove labels
type labelManager interface {
	AddLabel(ctx context.Context, svc *v1.Service) error
	RemoveLabel(ctx context.Context, svc *v1.Service) error
}

func NewServicesProcessor(config *kubevip.Config, bgpServer *bgp.Server,
	clientSet *kubernetes.Clientset, rwClientSet *kubernetes.Clientset, shutdownChan chan struct{},
	intfMgr *networkinterface.Manager, arpMgr *arp.Manager, nodeLabelManager labelManager) *Processor {
	lbClassFilterFunc := lbClassFilter
	if config.LoadBalancerClassLegacyHandling {
		lbClassFilterFunc = lbClassFilterLegacy
	}

	return &Processor{
		config:           config,
		lbClassFilter:    lbClassFilterFunc,
		ServiceInstances: []*instance.Instance{},
		bgpServer:        bgpServer,
		clientSet:        clientSet,
		rwClientSet:      rwClientSet,
		shutdownChan:     shutdownChan,
		CountServiceWatchEvent: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "kube_vip",
			Subsystem: "manager",
			Name:      "all_services_events",
			Help:      "Count all events fired by the service watcher categorised by event type",
		}, []string{"type"}),

		intfMgr:          intfMgr,
		arpMgr:           arpMgr,
		leaseMgr:         lease.NewManager(),
		nodeLabelManager: nodeLabelManager,
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
	if svc.Annotations[kubevip.LoadbalancerIgnore] == "true" {
		log.Info("ignore annotation for kube-vip", "service name", svc.Name)
		return true, nil
	}

	// Check the loadBalancer class
	if p.lbClassFilter(svc, p.config) {
		return true, nil
	}

	svcAddresses, svcHostnames := instance.FetchServiceAddresses(svc)

	// We only care about LoadBalancer services that have been allocated an address
	if len(svcAddresses) <= 0 {
		return true, nil
	}

	_, usesCommonLease := svc.Annotations[kubevip.ServiceLease]
	if usesCommonLease && svc.Spec.ExternalTrafficPolicy != v1.ServiceExternalTrafficPolicyTypeCluster {
		return false, fmt.Errorf("annotation %q cannot be used with service traffic policy other than %q",
			kubevip.ServiceLease, v1.ServiceExternalTrafficPolicyTypeCluster)
	}

	svcCtx, err := p.getServiceContext(svc.UID)
	if err != nil {
		return false, fmt.Errorf("failed to get service context: %w", err)
	}

	// The modified event should only be triggered if the service has been modified (i.e. moved somewhere else)
	if event.Type == watch.Modified {
		i := instance.FindServiceInstance(svc, p.ServiceInstances)
		shouldGarbageCollect := true
		if i != nil {
			originalServiceAddresses, originalServiceHostnames := instance.FetchServiceAddresses(i.ServiceSnapshot)
			shouldGarbageCollect =
				// Service addresses changed
				!reflect.DeepEqual(originalServiceAddresses, svcAddresses) ||
					// Service hostnames changed
					!reflect.DeepEqual(originalServiceHostnames, svcHostnames) ||
					// ExternalTrafficPolicy changed
					svc.Spec.ExternalTrafficPolicy != i.ServiceSnapshot.Spec.ExternalTrafficPolicy
		}
		if shouldGarbageCollect {
			for _, addr := range svcAddresses {
				// log.Debugf("(svcs) Retrieving local addresses, to ensure that this modified address doesn't exist: %s", addr)
				f, err := vip.GarbageCollect(p.config.Interface, addr, p.intfMgr)
				if err != nil {
					log.Error("(svcs) cleaning existing address error", "err", err)
				}
				if f {
					log.Warn("(svcs) already found existing config", "address", addr, "adapter", p.config.Interface)
				}
			}
			// This service has been modified, but it was also active.
			if svcCtx != nil && svcCtx.IsActive {
				log.Warn("(svcs) The load balancer has changed, cancelling original load balancer")
				svcCtx.Cancel()
				log.Warn("(svcs) waiting for load balancer to finish")
				<-svcCtx.Ctx.Done()

				if err := p.deleteService(svc.UID); err != nil {
					log.Error("(svc) unable to remove", "service", svc.UID)
				}
				// in theory this should never fail
				p.svcMap.Delete(svc.UID)
			}
		}
	}

	// Architecture walkthrough: (Had to do this as this code path is making my head hurt)

	// Is the service active (bool), if not then process this new service
	// Does this service use an election per service?
	//

	if svcCtx == nil || svcCtx != nil && !svcCtx.IsActive {
		ips, hostnames := instance.FetchServiceAddresses(svc)
		log.Debug("(svcs) has been added/modified with addresses", "service name", svc.Name, "ips", ips, "hostnames", hostnames)

		if svcCtx == nil {
			svcCtx = servicecontext.New(ctx)
			p.svcMap.Store(svc.UID, svcCtx)
		}

		if p.config.EnableServicesElection || // Service Election
			((p.config.EnableRoutingTable || p.config.EnableBGP) && // Routing table mode or BGP
				(!p.config.EnableLeaderElection && !p.config.EnableServicesElection)) { // No leaderelection or services election

			// If this load balancer Traffic Policy is "local"
			if svc.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyTypeLocal {

				// Start an endpoint watcher if we're not watching it already
				if !svcCtx.IsWatched {
					// background the endpoint watcher
					if (p.config.EnableRoutingTable || p.config.EnableBGP) && (!p.config.EnableLeaderElection && !p.config.EnableServicesElection) {
						err = serviceFunc(svcCtx.Ctx, svc)
						if err != nil {
							log.Error(err.Error())
						}
					}

					go func() {
						if svc.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyTypeLocal {
							// Add Endpoint or EndpointSlices watcher
							var provider providers.Provider
							if p.config.EnableEndpoints {
								provider = providers.NewEndpoints()
							} else {
								provider = providers.NewEndpointslices()
							}
							if err = p.watchEndpoint(svcCtx, p.config.NodeName, svc, provider); err != nil {
								log.Error(err.Error())
							}
						}
					}()

					// We're now watching this service
					svcCtx.IsWatched = true
				}
			} else if (p.config.EnableBGP || p.config.EnableRoutingTable) && (!p.config.EnableLeaderElection && !p.config.EnableServicesElection) {
				err = serviceFunc(svcCtx.Ctx, svc)
				if err != nil {
					log.Error(err.Error())
				}

				go func() {
					if svc.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyTypeCluster {
						// Add Endpoint watcher
						var provider providers.Provider
						if p.config.EnableEndpoints {
							provider = providers.NewEndpoints()
						} else {
							provider = providers.NewEndpointslices()
						}
						if err = p.watchEndpoint(svcCtx, p.config.NodeName, svc, provider); err != nil {
							log.Error(err.Error())
						}
					}
				}()
				// We're now watching this service
				svcCtx.IsWatched = true
			} else {

				go func() {
					for {
						select {
						case <-svcCtx.Ctx.Done():
							log.Warn("(svcs) restartable service watcher ending", "uid", svc.UID)
							return
						default:
							log.Info("(svcs) restartable service watcher starting", "uid", svc.UID)
							err = serviceFunc(svcCtx.Ctx, svc)

							if err != nil {
								log.Error(err.Error())
							}
						}
					}

				}()
			}
		} else {
			// Increment the waitGroup before the service Func is called (Done is completed in there)
			err = serviceFunc(svcCtx.Ctx, svc)
			if err != nil {
				log.Error(err.Error())
			}
		}
		svcCtx.IsActive = true
	}

	return false, nil
}

func (p *Processor) Delete(event watch.Event) (bool, error) {
	svc, ok := event.Object.(*v1.Service)
	if !ok {
		return false, fmt.Errorf("(svcs) unable to parse Kubernetes services from API watcher")
	}
	svcCtx, err := p.getServiceContext(svc.UID)
	if err != nil {
		return false, fmt.Errorf("(svcs) unable to get context: %w", err)
	}

	if svcCtx != nil {
		// We only care about LoadBalancer services
		if svc.Spec.Type != v1.ServiceTypeLoadBalancer {
			return true, nil
		}

		// We can ignore this service
		if svc.Annotations[kubevip.LoadbalancerIgnore] == "true" {
			log.Info("(svcs) ignore annotation for kube-vip", "service name", svc.Name)
			return true, nil
		}

		// If no leader election is enabled, delete routes here
		if !p.config.EnableLeaderElection && !p.config.EnableServicesElection &&
			p.config.EnableRoutingTable && svcCtx.HasConfiguredNetworks() {
			if errs := endpoints.ClearRoutes(svc, &p.ServiceInstances); len(errs) == 0 {
				svcCtx.ConfiguredNetworks.Clear()
			}
		}

		// If this is an active service then and additional leaderElection will handle stopping
		err = p.deleteService(svc.UID)
		if err != nil {
			log.Error(err.Error())
		}

		// Calls the cancel function of the context
		log.Warn("(svcs) The load balancer was deleted, cancelling context")
		svcCtx.Cancel()
		log.Warn("(svcs) waiting for load balancer to finish")
		<-svcCtx.Ctx.Done()
		p.svcMap.Delete(svc.UID)
	}

	log.Info("(svcs) deleted", "service name", svc.Name, "namespace", svc.Namespace)

	return true, nil
}

func (p *Processor) Stop() {
	for _, instance := range p.ServiceInstances {
		for _, cluster := range instance.Clusters {
			cluster.Stop()
		}
	}
}

func (p *Processor) getServiceContext(uid types.UID) (*servicecontext.Context, error) {
	svcCtx, ok := p.svcMap.Load(uid)
	if !ok {
		return nil, nil
	}
	ctx, ok := svcCtx.(*servicecontext.Context)
	if !ok {
		return nil, fmt.Errorf("failed to cast service context pointer - UID: %s", uid)
	}
	return ctx, nil
}

func (p *Processor) CountRouteReferences(route *netlink.Route) int {
	return endpoints.CountRouteReferences(route, &p.ServiceInstances)
}
