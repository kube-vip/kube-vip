package services

import (
	"context"
	"errors"
	"fmt"
	log "log/slog"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kube-vip/kube-vip/pkg/arp"
	"github.com/kube-vip/kube-vip/pkg/bgp"
	"github.com/kube-vip/kube-vip/pkg/election"
	"github.com/kube-vip/kube-vip/pkg/endpoints/providers"
	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/metrics"
	"github.com/kube-vip/kube-vip/pkg/networkinterface"
	"github.com/kube-vip/kube-vip/pkg/node"
	"github.com/kube-vip/kube-vip/pkg/route"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	"github.com/kube-vip/kube-vip/pkg/utils"
	"github.com/kube-vip/kube-vip/pkg/vip"
	"github.com/kube-vip/kube-vip/pkg/wireguard"
	"github.com/prometheus/client_golang/prometheus"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/keymutex"
)

const concurrentServiceLocks = 128

type Processor struct {
	config        *kubevip.Config
	lbClassFilter func(svc *v1.Service, config *kubevip.Config) bool
	svcMap        sync.Map

	// Keeps track of all running instances
	ServiceInstances []*instance.Instance
	instancesMutex   sync.RWMutex
	serviceLocks     keymutex.KeyMutex
	serviceLocksOnce sync.Once
	resourceLocks    keyedMutexes
	networkLifecycle sync.RWMutex
	metricsMu        sync.Mutex

	electionsMu        sync.Mutex
	elections          map[string]*serviceElection
	claimSeq           atomic.Uint64
	electionRun        func(context.Context, *election.RunConfig, *kubevip.Config) error
	newInstance        func(context.Context, *v1.Service, *sync.WaitGroup) (*instance.Instance, error)
	garbageCollect     func(string, string, *networkinterface.Manager) (bool, error)
	desiredMu          sync.Mutex
	desiredEvents      map[types.UID]desiredEvent
	desiredDeletes     []desiredDelete
	pendingMu          sync.Mutex
	pending            map[types.UID]*pendingReconcile
	cleanupMu          sync.Mutex
	cleanup            map[<-chan struct{}]*cleanupGroup
	privateCallbacksMu sync.Mutex
	privateCallbacks   map[*Callback]int

	bgpServer *bgp.Server

	clientSet   *kubernetes.Clientset
	rwClientSet *kubernetes.Clientset

	intfMgr *networkinterface.Manager
	arpMgr  *arp.Manager

	leaseMgr *lease.Manager

	// nodeLabelManager is the manager for the node labels
	nodeLabelManager node.Labeler

	electionMgr *election.Manager

	// TunnelMgr manages multiple WireGuard tunnels (one per service VIP)
	TunnelMgr *wireguard.TunnelManager

	routeMgr *route.Manager
}

// labelManager is the interface for the node label manager to add/remove labels

func NewServicesProcessor(config *kubevip.Config, bgpServer *bgp.Server,
	clientSet *kubernetes.Clientset, rwClientSet *kubernetes.Clientset,
	intfMgr *networkinterface.Manager, arpMgr *arp.Manager, nodeLabelManager node.Labeler,
	electionMgr *election.Manager, leaseMgr *lease.Manager, routeMgr *route.Manager) *Processor {
	lbClassFilterFunc := lbClassFilter
	if config.LoadBalancerClassLegacyHandling {
		lbClassFilterFunc = lbClassFilterLegacy
	}

	return &Processor{
		config:           config,
		lbClassFilter:    lbClassFilterFunc,
		ServiceInstances: []*instance.Instance{},
		serviceLocks:     keymutex.NewHashed(concurrentServiceLocks),
		elections:        make(map[string]*serviceElection),
		bgpServer:        bgpServer,
		clientSet:        clientSet,
		rwClientSet:      rwClientSet,
		intfMgr:          intfMgr,
		arpMgr:           arpMgr,
		leaseMgr:         leaseMgr,
		nodeLabelManager: nodeLabelManager,
		electionMgr:      electionMgr,
		TunnelMgr:        wireguard.NewTunnelManager(),
		routeMgr:         routeMgr,
	}
}

func (p *Processor) Reconcile(ctx context.Context, event watch.Event, serviceFunc *Callback, forcedOnly bool,
	wg *sync.WaitGroup, cancelWatcher context.CancelCauseFunc) error {
	svc, ok := event.Object.(*v1.Service)
	if !ok || svc == nil {
		return fmt.Errorf("unable to parse Kubernetes services from API watcher")
	}
	if !watcherOwnsService(svc, forcedOnly) {
		return nil
	}
	version := p.recordDesiredEvent(event.Type, svc)
	if version == 0 {
		return nil
	}
	return p.reconcileDesired(ctx, event, serviceFunc, forcedOnly, wg, cancelWatcher, version)
}

func (p *Processor) reconcileDesired(ctx context.Context, event watch.Event, serviceFunc *Callback, forcedOnly bool,
	wg *sync.WaitGroup, cancelWatcher context.CancelCauseFunc, version uint64) error {
	svc := event.Object.(*v1.Service)
	if latest := p.desiredServiceForVersion(svc.UID, version); latest != nil {
		svc = latest
		event.Object = latest
	} else if !p.desiredTerminalEventCurrent(svc.UID, version) {
		return nil
	}

	timer := prometheus.NewTimer(metrics.ServiceReconcileDuration.WithLabelValues(svc.Namespace))
	defer timer.ObserveDuration()

	if !watcherOwnsService(svc, forcedOnly) {
		return nil
	}

	// A tracked LoadBalancer must be torn down when its type changes.
	if svc.Spec.Type != v1.ServiceTypeLoadBalancer {
		p.discardPendingReconcile(svc.UID)
		return p.deleteTrackedService(svc, version)
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

	// The Service annotation is cluster-wide while nftables state is local to
	// each node. Reconcile stale per-Service chains on every node after a table
	// migration, even when this kube-vip pod is not the Service leader.
	if svc.Annotations[kubevip.EgressNftablesTable] != "" {
		if err := p.cleanupStaleEgressNftablesChains(svc); err != nil {
			log.Warn("failed to clean stale nftables egress chains", "service", svc.Name, "namespace", svc.Namespace, "err", err)
		}
	}

	svcAddresses, svcHostnames := instance.FetchServiceAddresses(svc)

	// We only care about LoadBalancer services that have been allocated an address
	if len(svcAddresses) <= 0 && len(svcHostnames) <= 0 {
		s, err := p.waitForAddress(ctx, svc)
		if err != nil {
			return fmt.Errorf("failed to get updated LB addresses for service %s/%s: %w", svc.Namespace, svc.Name, err)
		}
		svc = s
		p.refreshDesiredService(svc.UID, version, svc)
	}

	var svcInstance *instance.Instance
	var svcCtx *servicecontext.Context
	var previousService *v1.Service
	serviceModified := false
	shouldGarbageCollect := false
	var err error
	svcCtx, err = p.getServiceContext(svc.UID)
	if err != nil {
		return fmt.Errorf("failed to get service context: %w", err)
	}
	if svcCtx != nil && svcCtx.Ctx.Err() != nil {
		if c, member := p.serviceElectionMemberForContext(svcCtx); member != nil {
			p.queuePendingReconcile(c, member, ctx, event, serviceFunc, forcedOnly, wg, cancelWatcher, version)
			if cleanupErr := p.waitAndRetryServiceElectionMember(svcCtx); cleanupErr != nil {
				return fmt.Errorf("service %s/%s cleanup is still pending: %w", svc.Namespace, svc.Name, cleanupErr)
			}
			return nil
		}
		svcCtx = p.dropCancelledServiceContext(svc, svcCtx)
	}
	if svcCtx == nil {
		svcInstance = nil
	} else if svcCtx.Ctx.Err() != nil {
		return fmt.Errorf("service %s/%s cleanup is still pending", svc.Namespace, svc.Name)
	}
	if err := func() error {
		unlockService := p.lockService(svc.UID)
		defer unlockService()
		if !p.desiredEventCurrent(svc.UID, version) {
			return errServiceReconcileStale
		}

		svcInstance = p.findServiceInstance(svc)
		if svcCtx != nil {
			if c, member := p.serviceElectionMemberForContext(svcCtx); member != nil {
				previousService = c.memberServiceSnapshot(member)
			}
		}
		if previousService == nil && svcInstance != nil {
			previousService = svcInstance.ServiceSnapshot
		}
		_, usesCommonLease := svc.Annotations[kubevip.ServiceLease]
		if usesCommonLease && svc.Spec.ExternalTrafficPolicy != v1.ServiceExternalTrafficPolicyTypeCluster {
			metrics.ServiceReconcileErrorsTotal.WithLabelValues(svc.Namespace, svc.Name, "invalid_config").Inc()
			return fmt.Errorf("annotation %q cannot be used with service traffic policy other than %q, service %s/%s",
				kubevip.ServiceLease, v1.ServiceExternalTrafficPolicyTypeCluster, svc.Namespace, svc.Name)
		}

		if event.Type == watch.Modified {
			serviceModified = p.desiredLifecycleChanged(svc.UID, version) || serviceSnapshotChanged(previousService, svc) || serviceChanged(svcInstance, svc)
			shouldGarbageCollect = svcInstance != nil && serviceChanged(svcInstance, svc)
		}
		return nil
	}(); err != nil {
		if errors.Is(err, errServiceReconcileStale) {
			return nil
		}
		return err
	}

	// The modified event should only be triggered if the service has been modified (i.e. moved somewhere else)
	if event.Type == watch.Modified {
		if shouldGarbageCollect {
			unlockService := p.lockService(svc.UID)
			if p.findServiceInstance(svc) != svcInstance {
				unlockService()
				shouldGarbageCollect = false
			} else {
				oldService := svcInstance.ServiceSnapshot
				if oldService == nil {
					oldService = svc
				}
				unlockResources := p.lockInstanceResources(oldService, svcInstance)
				activeSiblingAddresses := make(map[string]struct{})
				for _, sibling := range p.serviceInstances() {
					if sibling == svcInstance {
						continue
					}
					for _, address := range p.activeInstanceAddresses(sibling) {
						activeSiblingAddresses[address] = struct{}{}
					}
				}
				garbageCollect := p.garbageCollect
				if garbageCollect == nil {
					garbageCollect = vip.GarbageCollect
				}
				for _, addr := range ownedInstanceAddresses(svcInstance) {
					if _, shared := activeSiblingAddresses[addr]; shared {
						continue
					}
					// log.Debugf("(svcs) Retrieving local addresses, to ensure that this modified address doesn't exist: %s", addr)
					f, err := garbageCollect(p.config.Interface, addr, p.intfMgr)
					if err != nil {
						log.Error("(svcs) cleaning existing address error", "err", err)
					}
					if f {
						log.Warn("(svcs) already found existing config", "address", addr, "adapter", p.config.Interface)
					}
				}
				unlockResources()
				unlockService()
			}
		}
		if serviceModified {
			// This service has been modified, but it was also active.
			if svcCtx != nil {
				log.Warn("(svcs) The load balancer has changed, cancelling original load balancer")
				oldSvcCtx := svcCtx
				oldService := previousService
				if oldService == nil {
					oldService = p.previousLifecycleService(svc.UID, version, svc)
				}
				if c, member := p.serviceElectionMemberForContext(oldSvcCtx); member != nil {
					p.queuePendingReconcile(c, member, ctx, event, serviceFunc, forcedOnly, wg, cancelWatcher, version)
					oldSvcCtx.ResetReadiness()
					oldSvcCtx.Cancel()
					if err := p.waitAndRetryServiceElectionMember(oldSvcCtx); err != nil {
						return fmt.Errorf("cleanup replaced service %s/%s: %w", svc.Namespace, svc.Name, err)
					}
					return nil
				} else if err := p.deleteService(ctx, svc.UID, oldSvcCtx); err != nil {
					metrics.ServiceReconcileErrorsTotal.WithLabelValues(svc.Namespace, svc.Name, "delete_service").Inc()
					return fmt.Errorf("cleanup replaced service %s/%s: %w", svc.Namespace, svc.Name, err)
				}
				oldSvcCtx.Cancel()
				// Retire the lease before the replacement context is built, so Add below
				// cannot hand back an instance the pending cleanup is about to cancel.
				// A lease shared with other services keeps their references and survives.
				ns, name := lease.ServiceName(oldService)
				leaseID := lease.NewID(p.config.LeaderElectionType, ns, name)
				if !p.serviceElectionMemberExists(oldService, oldSvcCtx) {
					p.releaseServiceLease(oldSvcCtx, oldService, leaseID, lease.ServiceClaimID(oldService), p.leaseMgr.Get(leaseID))
				}
				p.svcMap.CompareAndDelete(svc.UID, oldSvcCtx)
				// Reset the the svcCtx when it was garbage collected
				// As the next function will create a new context when nil
				svcCtx = nil
				svcInstance = nil
				p.updateActiveServicesMetric()
			}
		}
	}
	ips, hostnames := instance.FetchServiceAddresses(svc)
	log.Debug("(svcs) has been added/modified with addresses", "service name", svc.Name, "ips", ips, "hostnames", hostnames)

	if svcCtx == nil {
		for svcCtx == nil {
			unlockService := p.lockService(svc.UID)
			if !p.desiredEventCurrent(svc.UID, version) {
				unlockService()
				return nil
			}
			svcCtx, err = p.getServiceContext(svc.UID)
			if err != nil {
				unlockService()
				return fmt.Errorf("failed to get service context: %w", err)
			}
			if svcCtx != nil && svcCtx.Ctx.Err() != nil {
				unlockService()
				svcCtx = p.dropCancelledServiceContext(svc, svcCtx)
				if svcCtx != nil {
					return fmt.Errorf("service %s/%s cleanup is still pending", svc.Namespace, svc.Name)
				}
				continue
			}
			if svcCtx == nil {
				ns, name := lease.ServiceName(svc)
				leaseID := lease.NewID(p.config.LeaderElectionType, ns, name)
				p.leaseMgr.Add(ctx, leaseID)
				// The service context is parented to the watcher, not to the lease: losing a
				// lease must not tear the service down, it has to let the election restart.
				svcCtx = servicecontext.New(ctx)
				p.svcMap.Store(svc.UID, svcCtx)
			}
			unlockService()
		}
	}

	if svcInstance == nil && !p.config.EnableServicesElection {
		unlockService := p.lockService(svc.UID)
		if !p.desiredEventCurrent(svc.UID, version) {
			unlockService()
			return nil
		}
		instanceAdded := false
		svcInstance = p.findServiceInstance(svc)
		if svcInstance == nil {
			unlockResources := p.lockServiceResources(svc)
			svcInstance, err = p.makeServiceInstance(ctx, svc, wg)
			if err != nil {
				unlockResources()
				unlockService()
				metrics.ServiceReconcileErrorsTotal.WithLabelValues(svc.Namespace, svc.Name, "new_instance").Inc()
				return fmt.Errorf("unable to create instance for service %s/%s", svc.Namespace, svc.Name)
			}
			if !p.desiredEventCurrent(svc.UID, version) {
				cleanupErr := p.deleteCurrentServiceLocked(context.WithoutCancel(ctx), svcInstance)
				unlockResources()
				unlockService()
				if cleanupErr != nil {
					return fmt.Errorf("discard stale service instance: %w", cleanupErr)
				}
				return nil
			}
			p.appendServiceInstance(svcInstance)
			unlockResources()
			instanceAdded = true
		}
		unlockService()
		if instanceAdded {
			p.updateActiveServicesMetric()
		}
	}

	// this goroutine starts service handling function (with or without leaderelection)
	if svcCtx.StartWatching() {
		var endpointServiceFunc func(*servicecontext.Context, *v1.Service, *sync.WaitGroup, bool) error
		if serviceFunc != nil && serviceFunc.Function != nil && !p.isPrivateServiceElectionCallback(serviceFunc) {
			endpointServiceFunc = serviceFunc.Function
		}
		wg.Go(func() {
			watchWg := sync.WaitGroup{}
			defer func() {
				// wait for the sub-goroutines and tag service as not watched
				watchWg.Wait()
				svcCtx.StopWatching()
			}()

			watchWg.Go(func() {
				// signal endpoints goroutine we are ready to start and run service handling function
				log.Info("(svcs) service function starting", "uid", svc.UID)
				runErr := serviceFunc.Run(svcCtx, svc, wg)
				if runErr != nil {
					log.Error(runErr.Error())
					if utils.IsPanicError(runErr) {
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
				if err := p.watchEndpoint(svcCtx, p.config.NodeName, svc, provider, endpointServiceFunc, cancelWatcher); err != nil {
					log.Error("endpoint watcher failed", "service", svc.Name, "namespace", svc.Namespace, "err", err)
					if utils.IsPanicError(err) {
						cancelWatcher(err)
					}
				}
			})

		})
	}

	if !p.config.EnableServicesElection {
		log.Debug("Service now active", "name", svc.Name, "uid", svc.UID)
	}
	p.markDesiredLifecycleApplied(svc.UID, version)

	return nil
}

func (p *Processor) registerPrivateCallback(callback *Callback) func() {
	p.privateCallbacksMu.Lock()
	if p.privateCallbacks == nil {
		p.privateCallbacks = make(map[*Callback]int)
	}
	p.privateCallbacks[callback]++
	p.privateCallbacksMu.Unlock()
	return func() {
		p.privateCallbacksMu.Lock()
		if p.privateCallbacks[callback]--; p.privateCallbacks[callback] == 0 {
			delete(p.privateCallbacks, callback)
		}
		p.privateCallbacksMu.Unlock()
	}
}

func (p *Processor) isPrivateServiceElectionCallback(callback *Callback) bool {
	p.privateCallbacksMu.Lock()
	defer p.privateCallbacksMu.Unlock()
	return p.privateCallbacks[callback] != 0
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

func (p *Processor) Delete(event watch.Event, forcedOnly bool) error {
	svc, ok := event.Object.(*v1.Service)
	if !ok || svc == nil {
		return fmt.Errorf("(svcs) unable to parse Kubernetes services from API watcher")
	}

	if !watcherOwnsService(svc, forcedOnly) {
		return nil
	}
	version := p.recordDesiredEvent(event.Type, svc)
	if version == 0 {
		return nil
	}
	p.discardPendingReconcile(svc.UID)
	return p.deleteTrackedService(svc, version)
}

var errServiceReconcileStale = errors.New("service reconcile is no longer desired")

type desiredEvent struct {
	version              uint64
	type_                watch.EventType
	resourceVersion      string
	service              *v1.Service
	lifecycle            serviceLifecycle
	previousLifecycle    serviceLifecycle
	hasPreviousLifecycle bool
}

type desiredDelete struct {
	uid     types.UID
	version uint64
}

type serviceLifecycle struct {
	uid                           types.UID
	type_                         v1.ServiceType
	externalTrafficPolicy         v1.ServiceExternalTrafficPolicy
	internalTrafficPolicy         *v1.ServiceInternalTrafficPolicy
	ipFamilies                    []v1.IPFamily
	ipFamilyPolicy                *v1.IPFamilyPolicy
	ports                         []v1.ServicePort
	loadBalancerClass             *string
	allocateLoadBalancerNodePorts *bool
	loadBalancerSourceRanges      []string
	trafficDistribution           *string
	addresses                     []string
	hostnames                     []string
	annotations                   map[string]string
}

const maxDesiredDeleteTombstones = 1024

var lifecycleAnnotationKeys = []string{
	kubevip.RequestedIP,
	kubevip.Egress,
	kubevip.EgressInternal,
	kubevip.EgressIPv6,
	kubevip.EgressDestinationPorts,
	kubevip.EgressSourcePorts,
	kubevip.EgressAllowedNetworks,
	kubevip.EgressDeniedNetworks,
	kubevip.EgressNoInternalTraffic,
	kubevip.EgressDetectAPIServer,
	kubevip.FlushContrack,
	kubevip.LoadbalancerIPAnnotation,
	kubevip.LoadbalancerIgnore,
	kubevip.LoadbalancerHostname,
	kubevip.ServiceInterface,
	kubevip.ServiceVlan,
	kubevip.ServiceSecurityIgnore,
	kubevip.UpnpEnabled,
	kubevip.UpnpLeaseDuration,
	kubevip.RPFilter,
	kubevip.ServiceLease,
	kubevip.ForcePerServiceElection,
	kubevip.AllowReconcileWithoutEndpoints,
	kubevip.ServiceDDNS,
	kubevip.MacvlanName,
	kubevip.DHCPBroadcast,
}

func watcherOwnsService(service *v1.Service, forcedOnly bool) bool {
	forced := service.Annotations[kubevip.ForcePerServiceElection] == "true"
	return forcedOnly == forced
}

func serviceLifecycleFor(service *v1.Service) serviceLifecycle {
	addresses, hostnames := instance.FetchServiceAddresses(service)
	spec := service.Spec.DeepCopy()
	annotations := make(map[string]string)
	for _, key := range lifecycleAnnotationKeys {
		if value, ok := service.Annotations[key]; ok {
			annotations[key] = value
		}
	}
	return serviceLifecycle{
		uid: service.UID, type_: spec.Type, externalTrafficPolicy: spec.ExternalTrafficPolicy,
		internalTrafficPolicy: spec.InternalTrafficPolicy, ipFamilies: spec.IPFamilies,
		ipFamilyPolicy: spec.IPFamilyPolicy, ports: spec.Ports,
		loadBalancerClass:             spec.LoadBalancerClass,
		allocateLoadBalancerNodePorts: spec.AllocateLoadBalancerNodePorts,
		loadBalancerSourceRanges:      spec.LoadBalancerSourceRanges,
		trafficDistribution:           spec.TrafficDistribution,
		addresses:                     slices.Clone(addresses), hostnames: slices.Clone(hostnames), annotations: annotations,
	}
}

func serviceLifecycleEqual(first, second serviceLifecycle) bool {
	return reflect.DeepEqual(first, second)
}

func (p *Processor) recordDesiredEvent(eventType watch.EventType, service *v1.Service) uint64 {
	p.desiredMu.Lock()
	defer p.desiredMu.Unlock()
	if p.desiredEvents == nil {
		p.desiredEvents = make(map[types.UID]desiredEvent)
	}
	current, exists := p.desiredEvents[service.UID]
	// Forced and non-forced watchers can observe the same Kubernetes event.
	// They share one desired state, so retain its version for an exact duplicate.
	if exists && current.type_ == eventType && current.resourceVersion == service.ResourceVersion &&
		(current.service == nil || reflect.DeepEqual(current.service, service)) {
		return current.version
	}
	if exists && resourceVersionOlder(service.ResourceVersion, current.resourceVersion) {
		return 0
	}

	terminal := eventType == watch.Deleted || service.Spec.Type != v1.ServiceTypeLoadBalancer
	next := desiredEvent{version: current.version, type_: eventType, resourceVersion: service.ResourceVersion}
	if terminal {
		next.version++
		p.desiredEvents[service.UID] = next
		p.desiredDeletes = append(p.desiredDeletes, desiredDelete{uid: service.UID, version: next.version})
		p.pruneDesiredDeletesLocked()
		p.updateDesiredStateMetricsLocked()
		return next.version
	}

	next.service = service.DeepCopy()
	next.lifecycle = serviceLifecycleFor(service)
	if !exists || current.service == nil || !serviceLifecycleEqual(current.lifecycle, next.lifecycle) {
		next.version++
		if current.service != nil {
			next.previousLifecycle = current.lifecycle
			next.hasPreviousLifecycle = true
		}
	} else {
		next.previousLifecycle = current.previousLifecycle
		next.hasPreviousLifecycle = current.hasPreviousLifecycle
	}
	p.desiredEvents[service.UID] = next
	p.updateDesiredStateMetricsLocked()
	return next.version
}

func resourceVersionOlder(incoming, current string) bool {
	if incoming == "" || current == "" {
		return false
	}
	incomingNumber, incomingErr := strconv.ParseUint(incoming, 10, 64)
	currentNumber, currentErr := strconv.ParseUint(current, 10, 64)
	return incomingErr == nil && currentErr == nil && incomingNumber < currentNumber
}

func (p *Processor) pruneDesiredDeletesLocked() {
	for len(p.desiredDeletes) > maxDesiredDeleteTombstones {
		oldest := p.desiredDeletes[0]
		p.desiredDeletes = p.desiredDeletes[1:]
		if current, ok := p.desiredEvents[oldest.uid]; ok && current.service == nil && current.version == oldest.version {
			delete(p.desiredEvents, oldest.uid)
		}
	}
}

func (p *Processor) updateDesiredStateMetricsLocked() {
	active, terminal := 0, 0
	for _, desired := range p.desiredEvents {
		if desired.service == nil {
			terminal++
		} else {
			active++
		}
	}
	metrics.ServiceDesiredStateEntries.WithLabelValues("active").Set(float64(active))
	metrics.ServiceDesiredStateEntries.WithLabelValues("terminal").Set(float64(terminal))
}

func (p *Processor) desiredEventCurrent(uid types.UID, version uint64) bool {
	p.desiredMu.Lock()
	defer p.desiredMu.Unlock()
	desired, ok := p.desiredEvents[uid]
	if !ok || desired.version != version || desired.type_ == watch.Deleted || desired.service == nil {
		return false
	}
	return desired.service.Spec.Type == v1.ServiceTypeLoadBalancer
}

func (p *Processor) desiredLifecycleCurrent(uid types.UID, version uint64, lifecycle serviceLifecycle) bool {
	p.desiredMu.Lock()
	defer p.desiredMu.Unlock()
	desired, ok := p.desiredEvents[uid]
	return ok && desired.version == version && desired.type_ != watch.Deleted && desired.service != nil &&
		desired.service.Spec.Type == v1.ServiceTypeLoadBalancer && serviceLifecycleEqual(desired.lifecycle, lifecycle)
}

func (p *Processor) desiredService(uid types.UID) (*v1.Service, uint64) {
	p.desiredMu.Lock()
	defer p.desiredMu.Unlock()
	desired, ok := p.desiredEvents[uid]
	if !ok || desired.type_ == watch.Deleted || desired.service == nil || desired.service.Spec.Type != v1.ServiceTypeLoadBalancer {
		return nil, desired.version
	}
	return desired.service.DeepCopy(), desired.version
}

func (p *Processor) desiredServiceForVersion(uid types.UID, version uint64) *v1.Service {
	p.desiredMu.Lock()
	defer p.desiredMu.Unlock()
	desired, ok := p.desiredEvents[uid]
	if !ok || desired.version != version || desired.type_ == watch.Deleted || desired.service == nil {
		return nil
	}
	return desired.service.DeepCopy()
}

func (p *Processor) desiredLifecycleChanged(uid types.UID, version uint64) bool {
	p.desiredMu.Lock()
	defer p.desiredMu.Unlock()
	desired, ok := p.desiredEvents[uid]
	return ok && desired.version == version && desired.hasPreviousLifecycle &&
		!serviceLifecycleEqual(desired.previousLifecycle, desired.lifecycle)
}

func (p *Processor) markDesiredLifecycleApplied(uid types.UID, version uint64) {
	p.desiredMu.Lock()
	desired, ok := p.desiredEvents[uid]
	if ok && desired.version == version {
		desired.previousLifecycle = serviceLifecycle{}
		desired.hasPreviousLifecycle = false
		p.desiredEvents[uid] = desired
	}
	p.desiredMu.Unlock()
}

func (p *Processor) previousLifecycleService(uid types.UID, version uint64, current *v1.Service) *v1.Service {
	p.desiredMu.Lock()
	defer p.desiredMu.Unlock()
	desired, ok := p.desiredEvents[uid]
	if !ok || desired.version != version || !desired.hasPreviousLifecycle {
		return current
	}
	previous := current.DeepCopy()
	if previous.Annotations == nil {
		previous.Annotations = make(map[string]string)
	}
	if leaseName, ok := desired.previousLifecycle.annotations[kubevip.ServiceLease]; ok {
		previous.Annotations[kubevip.ServiceLease] = leaseName
	} else {
		delete(previous.Annotations, kubevip.ServiceLease)
	}
	return previous
}

func (p *Processor) refreshDesiredService(uid types.UID, version uint64, service *v1.Service) {
	p.desiredMu.Lock()
	defer p.desiredMu.Unlock()
	desired, ok := p.desiredEvents[uid]
	if !ok || desired.version != version || desired.type_ == watch.Deleted {
		return
	}
	desired.service = service.DeepCopy()
	desired.resourceVersion = service.ResourceVersion
	desired.lifecycle = serviceLifecycleFor(service)
	p.desiredEvents[uid] = desired
}

func (p *Processor) serviceIsLatestDesired(service *v1.Service) bool {
	p.desiredMu.Lock()
	defer p.desiredMu.Unlock()
	desired, ok := p.desiredEvents[service.UID]
	if !ok {
		return true
	}
	return desired.type_ != watch.Deleted && desired.service != nil && desired.service.Spec.Type == v1.ServiceTypeLoadBalancer &&
		serviceLifecycleEqual(desired.lifecycle, serviceLifecycleFor(service))
}

func (p *Processor) deleteTrackedService(svc *v1.Service, expectedVersion ...uint64) error {
	if svc == nil {
		return fmt.Errorf("(svcs) unable to delete nil service")
	}
	unlockService := p.lockService(svc.UID)
	if len(expectedVersion) != 0 && !p.desiredTerminalEventCurrent(svc.UID, expectedVersion[0]) {
		unlockService()
		return nil
	}
	svcCtx, err := p.getServiceContext(svc.UID)
	if err != nil {
		unlockService()
		return fmt.Errorf("(svcs) unable to get context: %w", err)
	}

	cleanupCtx := context.Background()
	var member *serviceElectionMember
	var releaseID lease.ID
	releaseClaim := false
	if svcCtx != nil {
		// Stop the old watchers and retire their lease before a replacement can attach.
		log.Warn("(svcs) The load balancer was deleted, cancelling context", "namespace", svc.Namespace, "name", svc.Name, "uid", svc.UID)
		if p.leaseMgr != nil {
			namespace, name := lease.ServiceName(svc)
			leaseID := lease.NewID(p.config.LeaderElectionType, namespace, name)
			_, member = p.serviceElectionMemberForContext(svcCtx)
			if member == nil {
				releaseID, releaseClaim = leaseID, true
			}
		}
		cleanupCtx = context.WithoutCancel(svcCtx.Ctx)
	}
	unlockService()
	if releaseClaim {
		p.releaseServiceLease(svcCtx, svc, releaseID, lease.ServiceClaimID(svc), p.leaseMgr.Get(releaseID))
	}

	if member != nil {
		finalize := func() {
			svcCtx.Cancel()
			p.svcMap.CompareAndDelete(svc.UID, svcCtx)
			metrics.ServiceElectionLoops.DeleteLabelValues(svc.Namespace, svc.Name)
			p.updateActiveServicesMetric()
		}
		if c, current := p.serviceElectionMemberForContext(svcCtx); current == member {
			p.finalizeServiceElectionMember(c, member, finalize)
		} else {
			finalize()
		}
		svcCtx.ResetReadiness()
		svcCtx.Cancel()
		if err := p.waitAndRetryServiceElectionMember(svcCtx); err != nil {
			log.Error("service cleanup continues asynchronously", "service", svc.Name, "namespace", svc.Namespace, "err", err)
			return fmt.Errorf("delete service %s/%s: %w", svc.Namespace, svc.Name, err)
		}
	} else if err := p.deleteService(cleanupCtx, svc.UID, svcCtx); err != nil {
		metrics.ServiceReconcileErrorsTotal.WithLabelValues(svc.Namespace, svc.Name, "delete_service").Inc()
		return fmt.Errorf("delete service %s/%s: %w", svc.Namespace, svc.Name, err)
	}
	if svcCtx != nil {
		svcCtx.Cancel()
	}
	if svcCtx != nil {
		p.svcMap.CompareAndDelete(svc.UID, svcCtx)
	}
	// Drop the per-service election series so a recreated service starts clean.
	metrics.ServiceElectionLoops.DeleteLabelValues(svc.Namespace, svc.Name)
	p.updateActiveServicesMetric()
	log.Info("(svcs) deleted", "service name", svc.Name, "namespace", svc.Namespace)

	return nil
}

func (p *Processor) desiredTerminalEventCurrent(uid types.UID, version uint64) bool {
	p.desiredMu.Lock()
	defer p.desiredMu.Unlock()
	desired, ok := p.desiredEvents[uid]
	return ok && desired.version == version && (desired.type_ == watch.Deleted || desired.service == nil || desired.service.Spec.Type != v1.ServiceTypeLoadBalancer)
}

func (p *Processor) Stop() {
	for _, instance := range p.serviceInstances() {
		unlockService := p.lockService(instance.UID())
		for _, cluster := range instance.Clusters {
			cluster.Stop()
		}
		instance.AddCalled = false
		unlockService()
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

// dropCancelledServiceContext discards a service context whose context has already been
// cancelled, removing it from svcMap and returning nil so that callers create a fresh one.
//
// This retires the cancelled context's lease membership before removing it from
// svcMap. That prevents a replacement context from reusing a lease which the old
// context's deferred cleanup is about to retire. Several paths cancel the service
// context without also removing it from svcMap - for example the deferred
// close(stopChan) in watchEndpoint and the utils.PanicError branch in AddOrModify.
//
// If such a cancelled context were reused, AddOrModify would skip its `if svcCtx == nil`
// branch and therefore never call leaseMgr.Add again, so StartServicesLeaderElection would
// fail with "no existing lease found" on every subsequent event and the VIP would never be
// advertised again.
func (p *Processor) dropCancelledServiceContext(svc *v1.Service, svcCtx *servicecontext.Context) *servicecontext.Context {
	if svcCtx == nil || svcCtx.Ctx.Err() == nil {
		return svcCtx
	}
	if c, member := p.serviceElectionMemberForContext(svcCtx); member != nil {
		p.finalizeServiceElectionMember(c, member, func() {
			p.svcMap.CompareAndDelete(svc.UID, svcCtx)
		})
	}
	if err := p.waitAndRetryServiceElectionMember(svcCtx); err != nil {
		return svcCtx
	}
	if err := p.deleteService(context.Background(), svc.UID, svcCtx); err != nil {
		return svcCtx
	}
	if p.leaseMgr != nil {
		namespace, name := lease.ServiceName(svc)
		leaseID := lease.NewID(p.config.LeaderElectionType, namespace, name)
		p.releaseServiceLease(svcCtx, svc, leaseID, lease.ServiceClaimID(svc), p.leaseMgr.Get(leaseID))
	}
	p.svcMap.CompareAndDelete(svc.UID, svcCtx)
	return nil
}

func serviceChanged(i *instance.Instance, svc *v1.Service) bool {
	if i == nil {
		return false
	}
	return serviceSnapshotChanged(i.ServiceSnapshot, svc)
}

func serviceSnapshotChanged(old, svc *v1.Service) bool {
	if old == nil || svc == nil {
		return false
	}
	return !serviceLifecycleEqual(serviceLifecycleFor(old), serviceLifecycleFor(svc))
}

func (p *Processor) updateActiveServicesMetric() {
	p.metricsMu.Lock()
	defer p.metricsMu.Unlock()
	counts := map[string]int{}
	for _, inst := range p.serviceInstances() {
		unlockService := p.lockService(inst.UID())
		if inst.ServiceSnapshot != nil {
			counts[inst.ServiceSnapshot.Namespace]++
		}
		unlockService()
	}
	metrics.ActiveServices.Reset()
	for ns, count := range counts {
		metrics.ActiveServices.WithLabelValues(ns).Set(float64(count))
	}
}

func (p *Processor) findServiceInstance(service *v1.Service) *instance.Instance {
	p.instancesMutex.RLock()
	defer p.instancesMutex.RUnlock()
	return instance.FindServiceInstance(service, p.ServiceInstances)
}

func (p *Processor) serviceInstances() []*instance.Instance {
	p.instancesMutex.RLock()
	defer p.instancesMutex.RUnlock()
	return append([]*instance.Instance(nil), p.ServiceInstances...)
}

// ServiceSnapshots returns stable copies for external observers such as diagnostics.
func (p *Processor) ServiceSnapshots() []*v1.Service {
	instances := p.serviceInstances()
	snapshots := make([]*v1.Service, 0, len(instances))
	for _, inst := range instances {
		if inst == nil {
			continue
		}
		unlockService := p.lockService(inst.UID())
		if inst.ServiceSnapshot != nil {
			snapshots = append(snapshots, inst.ServiceSnapshot.DeepCopy())
		}
		unlockService()
	}
	return snapshots
}

func (p *Processor) appendServiceInstance(inst *instance.Instance) {
	p.instancesMutex.Lock()
	defer p.instancesMutex.Unlock()
	p.ServiceInstances = append(p.ServiceInstances, inst)
}

func (p *Processor) detachServiceInstance(uid types.UID) (*instance.Instance, []*instance.Instance) {
	p.instancesMutex.Lock()
	defer p.instancesMutex.Unlock()

	remaining := make([]*instance.Instance, 0, len(p.ServiceInstances))
	var found *instance.Instance
	for _, inst := range p.ServiceInstances {
		if inst != nil && inst.UID() == uid {
			found = inst
			continue
		}
		remaining = append(remaining, inst)
	}
	if found != nil {
		p.ServiceInstances = remaining
		return found, append([]*instance.Instance(nil), remaining...)
	}
	return nil, append([]*instance.Instance(nil), remaining...)
}

func (p *Processor) lockService(uid types.UID) func() {
	p.serviceLocksOnce.Do(func() {
		if p.serviceLocks == nil {
			p.serviceLocks = keymutex.NewHashed(concurrentServiceLocks)
		}
	})
	key := string(uid)
	p.serviceLocks.LockKey(key)
	return func() {
		if err := p.serviceLocks.UnlockKey(key); err != nil {
			log.Error("failed to unlock service reconciliation", "uid", uid, "err", err)
		}
	}
}

type keyedMutex struct {
	mu   sync.Mutex
	refs int
}

type keyedMutexes struct {
	mu      sync.Mutex
	entries map[string]*keyedMutex
}

func (m *keyedMutexes) lock(keys []string) func() {
	keys = slices.DeleteFunc(keys, func(key string) bool { return key == "" })
	slices.Sort(keys)
	keys = slices.Compact(keys)

	m.mu.Lock()
	if m.entries == nil {
		m.entries = make(map[string]*keyedMutex)
	}
	locks := make([]*keyedMutex, 0, len(keys))
	for _, key := range keys {
		entry := m.entries[key]
		if entry == nil {
			entry = &keyedMutex{}
			m.entries[key] = entry
		}
		entry.refs++
		locks = append(locks, entry)
	}
	m.mu.Unlock()

	for _, entry := range locks {
		entry.mu.Lock()
	}

	return func() {
		for i := len(locks) - 1; i >= 0; i-- {
			locks[i].mu.Unlock()
		}
		m.mu.Lock()
		for i, key := range keys {
			locks[i].refs--
			if locks[i].refs == 0 {
				delete(m.entries, key)
			}
		}
		m.mu.Unlock()
	}
}

func (m *keyedMutexes) len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.entries)
}

func (p *Processor) lockServiceResources(svc *v1.Service) func() {
	return p.lockNetworkResources(serviceHasHostname(svc), serviceResourceKeys(svc))
}

func (p *Processor) lockInstanceResources(svc *v1.Service, inst *instance.Instance) func() {
	return p.lockNetworkResources(serviceHasHostname(svc) || instanceHasHostname(inst), instanceResourceKeys(svc, inst))
}

func (p *Processor) lockNetworkResources(exclusive bool, keys []string) func() {
	if exclusive {
		p.networkLifecycle.Lock()
	} else {
		p.networkLifecycle.RLock()
	}
	unlockResources := p.lockResources(keys)
	return func() {
		unlockResources()
		if exclusive {
			p.networkLifecycle.Unlock()
		} else {
			p.networkLifecycle.RUnlock()
		}
	}
}

func serviceHasHostname(svc *v1.Service) bool {
	if svc == nil {
		return false
	}
	_, hostnames := instance.FetchServiceAddresses(svc)
	return len(hostnames) != 0
}

func instanceHasHostname(inst *instance.Instance) bool {
	if inst == nil {
		return false
	}
	if serviceHasHostname(inst.ServiceSnapshot) {
		return true
	}
	for _, config := range inst.VIPConfigs {
		if config != nil && config.VIP != "" && !utils.IsIP(config.VIP) {
			return true
		}
	}
	return false
}

func (p *Processor) lockResources(keys []string) func() {
	return p.resourceLocks.lock(keys)
}

func serviceResourceKeys(svc *v1.Service) []string {
	if svc == nil {
		return nil
	}
	addresses, hostnames := instance.FetchServiceAddresses(svc)
	keys := []string{"service:" + svc.Namespace + "/" + svc.Name}
	dhcp := false
	for _, address := range addresses {
		if address == "0.0.0.0" || address == "::" {
			dhcp = true
			continue
		}
		if address != "" {
			keys = append(keys, "vip:"+address)
		}
	}
	for _, hostname := range hostnames {
		if hostname != "" {
			keys = append(keys, "hostname:"+hostname)
		}
	}
	if vlan := strings.TrimSpace(svc.Annotations[kubevip.ServiceVlan]); vlan != "" {
		keys = append(keys, "vlan:"+vlan)
	}
	if requested := svc.Annotations[kubevip.RequestedIP]; requested != "" {
		for _, address := range strings.Split(requested, ",") {
			address = strings.TrimSpace(address)
			if address != "" {
				keys = append(keys, "vip:"+address)
			}
		}
	}
	if dhcp {
		name := svc.Annotations[kubevip.MacvlanName]
		if name == "" {
			uid := string(svc.UID)
			if len(uid) >= 8 {
				name = "vip-" + uid[:8]
			}
		}
		if name != "" {
			keys = append(keys, "dhcp:"+name)
		}
	}
	return keys
}

func instanceResourceKeys(svc *v1.Service, inst *instance.Instance) []string {
	keys := serviceResourceKeys(svc)
	if inst == nil {
		return keys
	}
	for _, address := range ownedInstanceAddresses(inst) {
		keys = append(keys, "vip:"+address)
	}
	if inst.IsVLAN && inst.VLANInterface != "" {
		keys = append(keys, "vlan:"+inst.VLANInterface)
	}
	if (inst.IsDHCPv4 || inst.IsDHCPv6) && inst.DHCPInterface != "" {
		keys = append(keys, "dhcp:"+inst.DHCPInterface)
	}
	return keys
}

func (p *Processor) activeInstanceAddresses(inst *instance.Instance) []string {
	set := make(map[string]struct{})
	for _, c := range inst.Clusters {
		if c.WorkersRunning() {
			for _, network := range c.Network {
				address := network.IP()
				if address != "" && address != "0.0.0.0" && address != "::" {
					set[address] = struct{}{}
				}
			}
		}
	}
	addresses := make([]string, 0, len(set))
	for address := range set {
		addresses = append(addresses, address)
	}
	return addresses
}

func (p *Processor) makeServiceInstance(ctx context.Context, svc *v1.Service, wg *sync.WaitGroup) (*instance.Instance, error) {
	if p.newInstance != nil {
		return p.newInstance(ctx, svc, wg)
	}
	return instance.NewInstance(ctx, svc, p.config, p.intfMgr, p.arpMgr, p.routeMgr, p.nodeLabelManager, wg)
}
