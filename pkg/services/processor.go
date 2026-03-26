package services

import (
	"context"
	"errors"
	"fmt"
	log "log/slog"
	"reflect"
	"sync"
	"time"

	"github.com/kube-vip/kube-vip/pkg/arp"
	"github.com/kube-vip/kube-vip/pkg/bgp"
	"github.com/kube-vip/kube-vip/pkg/election"
	"github.com/kube-vip/kube-vip/pkg/endpoints"
	"github.com/kube-vip/kube-vip/pkg/endpoints/providers"
	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/networkinterface"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	"github.com/kube-vip/kube-vip/pkg/utils"
	"github.com/kube-vip/kube-vip/pkg/vip"
	"github.com/kube-vip/kube-vip/pkg/wireguard"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/vishvananda/netlink"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

	// This is a prometheus counter used to count the number of events received
	// from the service watcher
	CountServiceWatchEvent *prometheus.CounterVec

	intfMgr *networkinterface.Manager
	arpMgr  *arp.Manager

	leaseMgr *lease.Manager

	// nodeLabelManager is the manager for the node labels
	nodeLabelManager labelManager

	electionMgr *election.Manager

	// TunnelMgr manages multiple WireGuard tunnels (one per service VIP)
	TunnelMgr *wireguard.TunnelManager
}

// labelManager is the interface for the node label manager to add/remove labels
type labelManager interface {
	AddLabel(ctx context.Context, svc *v1.Service) error
	RemoveLabel(ctx context.Context, svc *v1.Service) error
}

func NewServicesProcessor(config *kubevip.Config, bgpServer *bgp.Server,
	clientSet *kubernetes.Clientset, rwClientSet *kubernetes.Clientset,
	intfMgr *networkinterface.Manager, arpMgr *arp.Manager, nodeLabelManager labelManager,
	electionMgr *election.Manager, leaseMgr *lease.Manager) *Processor {
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
		CountServiceWatchEvent: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "kube_vip",
			Subsystem: "manager",
			Name:      "all_services_events",
			Help:      "Count all events fired by the service watcher categorised by event type",
		}, []string{"type"}),

		intfMgr:          intfMgr,
		arpMgr:           arpMgr,
		leaseMgr:         leaseMgr,
		nodeLabelManager: nodeLabelManager,
		electionMgr:      electionMgr,
		TunnelMgr:        wireguard.NewTunnelManager(),
	}
}

func (p *Processor) AddOrModify(ctx context.Context, event watch.Event, serviceFunc *Callback, wg *sync.WaitGroup) error {
	svc, ok := event.Object.(*v1.Service)
	if !ok {
		return fmt.Errorf("unable to parse Kubernetes services from API watcher")
	}

	// We only care about LoadBalancer services
	if svc.Spec.Type != v1.ServiceTypeLoadBalancer {
		return nil
	}

	// Check if we ignore this service
	if svc.Annotations[kubevip.LoadbalancerIgnore] == "true" {
		log.Info("ignore annotation for kube-vip", "service name", svc.Name)
		return nil
	}

	// Check the loadBalancer class
	if p.lbClassFilter(svc, p.config) {
		return nil
	}

	svcAddresses, svcHostnames := instance.FetchServiceAddresses(svc)

	// We only care about LoadBalancer services that have been allocated an address
	if len(svcAddresses) <= 0 && len(svcHostnames) <= 0 {
		s, err := p.waitForAddress(ctx, svc)
		if err != nil {
			return fmt.Errorf("failed to get updated LB addresses for service %s/%s: %w", svc.Namespace, svc.Name, err)
		}
		svc = s
	}

	svcInstance := instance.FindServiceInstance(svc, p.ServiceInstances)
	var err error
	if svcInstance == nil {
		svcInstance, err = instance.NewInstance(ctx, svc, p.config, p.intfMgr, p.arpMgr, wg)
		if err != nil {
			return fmt.Errorf("unalbe to create instance for service %s/%s", svc.Namespace, svc.Name)
		}
		p.ServiceInstances = append(p.ServiceInstances, svcInstance)
	}

	_, usesCommonLease := svc.Annotations[kubevip.ServiceLease]
	if usesCommonLease && svc.Spec.ExternalTrafficPolicy != v1.ServiceExternalTrafficPolicyTypeCluster {
		return fmt.Errorf("annotation %q cannot be used with service traffic policy other than %q",
			kubevip.ServiceLease, v1.ServiceExternalTrafficPolicyTypeCluster)
	}

	svcCtx, err := p.getServiceContext(svc.UID)
	if err != nil {
		return fmt.Errorf("failed to get service context: %w", err)
	}

	// The modified event should only be triggered if the service has been modified (i.e. moved somewhere else)
	if event.Type == watch.Modified {
		shouldGarbageCollect := false
		if svcInstance != nil {
			shouldGarbageCollect = serviceChanged(svcInstance, svc)
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
			if svcCtx != nil {
				log.Warn("(svcs) The load balancer has changed, cancelling original load balancer")
				svcCtx.Cancel()

				if err := p.deleteService(ctx, svc.UID); err != nil {
					log.Error("(svc) unable to remove", "service", svc.UID)
				}
				// in theory this should never fail
				p.svcMap.Delete(svc.UID)
				// Reset the the svcCtx when it was garbage collected
				// As the next function will create a new context when nil
				svcCtx = nil
			}
		}
	}

	ips, hostnames := instance.FetchServiceAddresses(svc)
	log.Debug("(svcs) has been added/modified with addresses", "service name", svc.Name, "ips", ips, "hostnames", hostnames)

	if svcCtx == nil {
		ns, name := lease.ServiceName(svc)
		leaseID := lease.NewID(p.config.LeaderElectionType, ns, name)
		lease := p.leaseMgr.Add(ctx, leaseID)
		svcCtx = servicecontext.New(lease.Ctx)
		p.svcMap.Store(svc.UID, svcCtx)
	}

	// this goroutine starts service handling function (with or without leaderelection)
	if !svcCtx.IsWatched {
		wg.Go(func() {
			watchWg := sync.WaitGroup{}
			defer func() {
				// wait for the sub-goroutines and tag service as not watched
				watchWg.Wait()
				svcCtx.IsWatched = false
			}()

			watchWg.Go(func() {
				// start if service is not already watched/handled
				// signal endpoints goroutine we are ready to start and run service handling function
				log.Info("(svcs) service function starting", "uid", svc.UID)
				err = serviceFunc.Run(svcCtx, svc, wg)
				if err != nil {
					log.Error(err.Error())
					if errors.Is(err, &utils.PanicError{}) {
						// cancel service context on panic error
						// TODO:  should we quit kube-vip altogether here?
						svcCtx.Cancel()
					}
				}
				log.Info("(svcs) service function done", "uid", svc.UID)
			})

			// this goroutine will watch endpoints for the service
			watchWg.Go(func() {
				// create provider and start watching the endpoints
				var provider providers.Provider
				if p.config.EnableEndpoints {
					provider = providers.NewEndpoints()
				} else {
					provider = providers.NewEndpointslices()
				}
				if err = p.watchEndpoint(svcCtx, p.config.NodeName, svc, provider); err != nil {
					log.Error(err.Error())
				}
			})

		})

		// tag service as watched
		svcCtx.IsWatched = true
	}

	if !p.config.EnableServicesElection {
		log.Debug("Service now active", "name", svc.Name, "uid", svc.UID)
	}

	return nil
}

func (p *Processor) waitForAddress(ctx context.Context, svc *v1.Service) (*v1.Service, error) {
	addressCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ticker := time.NewTicker(time.Second)

	for {
		select {
		case <-addressCtx.Done():
			return nil, fmt.Errorf("failed to wait for the service LB address: %w", ctx.Err())
		case <-ticker.C:
			s, err := p.clientSet.CoreV1().Services(svc.Namespace).Get(addressCtx, svc.Name, metav1.GetOptions{})
			if err != nil {
				return nil, fmt.Errorf("failed to get updated service data: %w", err)
			}
			addrs, hostnames := instance.FetchServiceAddresses(s)
			if len(addrs) > 0 || len(hostnames) > 0 {
				return s, nil
			}
		}
	}
}

func (p *Processor) Delete(event watch.Event) error {
	svc, ok := event.Object.(*v1.Service)
	if !ok {
		return fmt.Errorf("(svcs) unable to parse Kubernetes services from API watcher")
	}
	svcCtx, err := p.getServiceContext(svc.UID)
	if err != nil {
		return fmt.Errorf("(svcs) unable to get context: %w", err)
	}

	if svcCtx != nil {
		// We only care about LoadBalancer services
		if svc.Spec.Type != v1.ServiceTypeLoadBalancer {
			return nil
		}

		// We can ignore this service
		if svc.Annotations[kubevip.LoadbalancerIgnore] == "true" {
			log.Info("(svcs) ignore annotation for kube-vip", "service name", svc.Name)
			return nil
		}

		// If no leader election is enabled, delete routes here
		if !p.config.EnableLeaderElection && !p.config.EnableServicesElection &&
			p.config.EnableRoutingTable && svcCtx.HasConfiguredNetworks() {
			if errs := endpoints.ClearRoutes(svc, &p.ServiceInstances); len(errs) == 0 {
				svcCtx.ConfiguredNetworks.Clear()
			}
		}

		if !p.config.EnableServicesElection {
			// If this is an active service then and additional leaderElection will handle stopping
			err = p.deleteService(svcCtx.Ctx, svc.UID)
			if err != nil {
				log.Error(err.Error())
			}
		}

		// Calls the cancel function of the context
		log.Warn("(svcs) The load balancer was deleted, cancelling context", "namespace", svc.Namespace, "name", svc.Name, "uid", svc.UID)
		svcCtx.Cancel()
		p.svcMap.Delete(svc.UID)
	}

	log.Info("(svcs) deleted", "service name", svc.Name, "namespace", svc.Namespace)

	return nil
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

func serviceChanged(i *instance.Instance, svc *v1.Service) bool {
	svcAddresses, svcHostnames := instance.FetchServiceAddresses(svc)
	originalServiceAddresses, originalServiceHostnames := instance.FetchServiceAddresses(i.ServiceSnapshot)

	// Service addresses changed
	return !reflect.DeepEqual(originalServiceAddresses, svcAddresses) ||
		// Service hostnames changed
		!reflect.DeepEqual(originalServiceHostnames, svcHostnames) ||
		// ExternalTrafficPolicy changed
		svc.Spec.ExternalTrafficPolicy != i.ServiceSnapshot.Spec.ExternalTrafficPolicy ||
		// IP stack configuration changed
		!reflect.DeepEqual(svc.Spec.IPFamilies, i.ServiceSnapshot.Spec.IPFamilies) ||
		*svc.Spec.IPFamilyPolicy != *i.ServiceSnapshot.Spec.IPFamilyPolicy ||
		// DDNS was disabled/enabled
		svc.Annotations[kubevip.ServiceDDNS] != i.ServiceSnapshot.Annotations[kubevip.ServiceDDNS]
}
