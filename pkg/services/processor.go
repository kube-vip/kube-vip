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
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/keymutex"
)

const concurrentServiceLocks = 128

var serviceDeletionRetryBackoff = wait.Backoff{
	Steps:    3,
	Duration: 10 * time.Millisecond,
	Factor:   1.0,
	Jitter:   0.1,
}

type Processor struct {
	config        *kubevip.Config
	lbClassFilter func(svc *v1.Service, config *kubevip.Config) bool
	svcMap        sync.Map

	// Keeps track of all running instances
	ServiceInstances []*instance.Instance
	instancesMutex   sync.RWMutex
	serviceLocks     keymutex.KeyMutex
	serviceLocksOnce sync.Once

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

func (p *Processor) AddOrModify(ctx context.Context, event watch.Event, serviceFunc *Callback, forcedOnly bool,
	wg *sync.WaitGroup, cancelWatcher context.CancelCauseFunc) error {
	svc, ok := event.Object.(*v1.Service)
	if !ok {
		return fmt.Errorf("unable to parse Kubernetes services from API watcher")
	}

	timer := prometheus.NewTimer(metrics.ServiceReconcileDuration.WithLabelValues(svc.Namespace))
	defer timer.ObserveDuration()

	if forcedOnly && svc.Annotations[kubevip.ForcePerServiceElection] != "true" ||
		!forcedOnly && svc.Annotations[kubevip.ForcePerServiceElection] == "true" {
		return nil
	}

	// A tracked LoadBalancer must be torn down when its type changes.
	if svc.Spec.Type != v1.ServiceTypeLoadBalancer {
		return p.deleteTrackedService(svc)
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
	}

	unlockService := p.lockService(svc.UID)
	serviceLocked := true
	defer func() {
		if serviceLocked {
			unlockService()
		}
	}()
	svcInstance := p.findServiceInstance(svc)

	_, usesCommonLease := svc.Annotations[kubevip.ServiceLease]
	if usesCommonLease && svc.Spec.ExternalTrafficPolicy != v1.ServiceExternalTrafficPolicyTypeCluster {
		metrics.ServiceReconcileErrorsTotal.WithLabelValues(svc.Namespace, svc.Name, "invalid_config").Inc()
		return fmt.Errorf("annotation %q cannot be used with service traffic policy other than %q, service %s/%s",
			kubevip.ServiceLease, v1.ServiceExternalTrafficPolicyTypeCluster, svc.Namespace, svc.Name)
	}

	svcCtx, err := p.getServiceContext(svc.UID)
	if err != nil {
		metrics.ServiceReconcileErrorsTotal.WithLabelValues(svc.Namespace, svc.Name, "service_context").Inc()
		return fmt.Errorf("failed to get service context: %w", err)
	}
	svcCtx = p.dropCancelledServiceContext(svc.UID, svcCtx)

	// The modified event should only be triggered if the service has been modified (i.e. moved somewhere else)
	if event.Type == watch.Modified {
		shouldGarbageCollect := false
		if svcInstance != nil {
			shouldGarbageCollect = serviceChanged(svcInstance, svc)
		}
		unlockService()
		serviceLocked = false
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
				//Set it to inactive
				svcCtx.Cancel()

				if err := p.deleteService(ctx, svc.UID); err != nil {
					metrics.ServiceReconcileErrorsTotal.WithLabelValues(svc.Namespace, svc.Name, "delete_service").Inc()
					log.Error("(svc) unable to remove", "service", svc.UID)
				}
				// Retire the lease before the replacement context is built, so Add below
				// cannot hand back an instance the pending cleanup is about to cancel.
				// A lease shared with other services keeps their references and survives.
				ns, name := lease.ServiceName(svc)
				leaseID := lease.NewID(p.config.LeaderElectionType, ns, name)
				p.leaseMgr.Delete(leaseID, lease.ServiceNamespacedName(svc), nil)
				// Reset the the svcCtx when it was garbage collected
				// As the next function will create a new context when nil
				svcCtx = nil
				svcInstance = nil
				p.updateActiveServicesMetric()
			}
		}
	} else {
		unlockService()
		serviceLocked = false
	}
	ips, hostnames := instance.FetchServiceAddresses(svc)
	log.Debug("(svcs) has been added/modified with addresses", "service name", svc.Name, "ips", ips, "hostnames", hostnames)

	if svcCtx == nil {
		unlockService := p.lockService(svc.UID)
		svcCtx, err = p.getServiceContext(svc.UID)
		if err != nil {
			unlockService()
			return fmt.Errorf("failed to get service context: %w", err)
		}
		svcCtx = p.dropCancelledServiceContext(svc.UID, svcCtx)
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

	if svcInstance == nil {
		unlockService := p.lockService(svc.UID)
		instanceAdded := false
		svcInstance = p.findServiceInstance(svc)
		if svcInstance == nil {
			svcInstance, err = instance.NewInstance(ctx, svc, p.config, p.intfMgr, p.arpMgr, p.routeMgr, p.nodeLabelManager, wg)
			if err != nil {
				unlockService()
				metrics.ServiceReconcileErrorsTotal.WithLabelValues(svc.Namespace, svc.Name, "new_instance").Inc()
				return fmt.Errorf("unable to create instance for service %s/%s", svc.Namespace, svc.Name)
			}
			p.appendServiceInstance(svcInstance)
			instanceAdded = true
		}
		unlockService()
		if instanceAdded {
			p.updateActiveServicesMetric()
		}
	}

	// this goroutine starts service handling function (with or without leaderelection)
	if svcCtx.StartWatching() {
		wg.Go(func() {
			watchWg := sync.WaitGroup{}
			defer func() {
				// wait for the sub-goroutines and tag service as not watched
				watchWg.Wait()
				svcCtx.StopWatching()
			}()

			watchWg.Go(func() {
				// start if service is not already watched/handled
				// signal endpoints goroutine we are ready to start and run service handling function
				log.Info("(svcs) service function starting", "uid", svc.UID)
				err = serviceFunc.Run(svcCtx, svc, wg)
				if err != nil {
					log.Error(err.Error())
					if utils.IsPanicError(err) {
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
				if err := p.watchEndpoint(svcCtx, p.config.NodeName, svc, provider, cancelWatcher); err != nil {
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

func (p *Processor) Delete(event watch.Event, forcedOnly bool) error {
	svc, ok := event.Object.(*v1.Service)
	if !ok {
		return fmt.Errorf("(svcs) unable to parse Kubernetes services from API watcher")
	}

	if forcedOnly && svc.Annotations[kubevip.ForcePerServiceElection] != "true" ||
		!forcedOnly && svc.Annotations[kubevip.ForcePerServiceElection] == "true" {
		return nil
	}

	return p.deleteTrackedService(svc)
}

func (p *Processor) deleteTrackedService(svc *v1.Service) error {
	unlockService := p.lockService(svc.UID)
	svcCtx, err := p.getServiceContext(svc.UID)
	if err != nil {
		unlockService()
		return fmt.Errorf("(svcs) unable to get context: %w", err)
	}

	if svcCtx == nil {
		unlockService()
		return nil
	}

	// Calls the cancel function of the context.
	log.Warn("(svcs) The load balancer was deleted, cancelling context", "namespace", svc.Namespace, "name", svc.Name, "uid", svc.UID)
	svcCtx.Cancel()
	p.svcMap.CompareAndDelete(svc.UID, svcCtx)
	// Drop the per-service election series so a recreated service starts clean.
	metrics.ServiceElectionLoops.DeleteLabelValues(svc.Namespace, svc.Name)
	deleteWithoutElection := !p.config.EnableServicesElection
	unlockService()

	if deleteWithoutElection {
		cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), time.Second)
		deletionErr := retry.OnError(serviceDeletionRetryBackoff, func(err error) bool {
			return cleanupCtx.Err() == nil && !errors.Is(err, context.Canceled)
		}, func() error {
			return p.deleteService(cleanupCtx, svc.UID)
		})
		cancelCleanup()
		if deletionErr != nil {
			return fmt.Errorf("delete service %s/%s: %w", svc.Namespace, svc.Name, deletionErr)
		}
	}
	p.updateActiveServicesMetric()
	log.Info("(svcs) deleted", "service name", svc.Name, "namespace", svc.Namespace)

	return nil
}

func (p *Processor) Stop() {
	for _, instance := range p.serviceInstances() {
		unlockService := p.lockService(instance.UID())
		for _, cluster := range instance.Clusters {
			cluster.Stop()
		}
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
// This matters because the in-memory lease and the service context are removed independently.
// The cleanup goroutine started by StartServicesLeaderElection calls leaseMgr.Delete once
// svcCtx.Ctx is done, and Manager.Delete drops the lease entirely when its last object goes
// away. Several paths cancel the service context without also removing it from svcMap - for
// example the deferred close(stopChan) in watchEndpoint, and the utils.PanicError branch in
// AddOrModify.
//
// If such a cancelled context were reused, AddOrModify would skip its `if svcCtx == nil`
// branch and therefore never call leaseMgr.Add again, so StartServicesLeaderElection would
// fail with "no existing lease found" on every subsequent event and the VIP would never be
// advertised again.
func (p *Processor) dropCancelledServiceContext(uid types.UID, svcCtx *servicecontext.Context) *servicecontext.Context {
	if svcCtx == nil || svcCtx.Ctx.Err() == nil {
		return svcCtx
	}
	p.svcMap.CompareAndDelete(uid, svcCtx)
	return nil
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
		svc.Annotations[kubevip.ServiceDDNS] != i.ServiceSnapshot.Annotations[kubevip.ServiceDDNS] ||
		// lease name was changed
		svc.Annotations[kubevip.ServiceLease] != i.ServiceSnapshot.Annotations[kubevip.ServiceLease]
}

func (p *Processor) updateActiveServicesMetric() {
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
