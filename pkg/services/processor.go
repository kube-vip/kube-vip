package services

import (
	"context"
	"fmt"
	log "log/slog"
	"net"
	"reflect"
	"slices"
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
	"github.com/kube-vip/kube-vip/pkg/metrics"
	"github.com/kube-vip/kube-vip/pkg/networkinterface"
	"github.com/kube-vip/kube-vip/pkg/node"
	"github.com/kube-vip/kube-vip/pkg/route"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	"github.com/kube-vip/kube-vip/pkg/utils"
	"github.com/kube-vip/kube-vip/pkg/vip"
	"github.com/kube-vip/kube-vip/pkg/wireguard"
	"github.com/prometheus/client_golang/prometheus"
	coordinationv1 "k8s.io/api/coordination/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	servicesMu    sync.Mutex
	services      map[types.NamespacedName]types.UID
	recoveryMu    sync.Mutex
	recovered     bool

	// Keeps track of all running instances
	ServiceInstances []*instance.Instance

	mutex            sync.Mutex
	serviceLocks     keymutex.KeyMutex
	serviceLocksOnce sync.Once
	bgpServer        *bgp.Server

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
		services:         make(map[types.NamespacedName]types.UID),
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
	defer unlockService()

	svcInstance := instance.FindServiceInstance(svc, p.ServiceInstances)

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
			}
		}
	}

	ips, hostnames := instance.FetchServiceAddresses(svc)
	log.Debug("(svcs) has been added/modified with addresses", "service name", svc.Name, "ips", ips, "hostnames", hostnames)

	if svcCtx == nil {
		ns, name := lease.ServiceName(svc)
		leaseID := lease.NewID(p.config.LeaderElectionType, ns, name)
		p.leaseMgr.Add(ctx, leaseID)
		// The service context is parented to the watcher, not to the lease: losing a
		// lease must not tear the service down, it has to let the election restart.
		svcCtx = servicecontext.New(ctx)
		p.svcMap.Store(svc.UID, svcCtx)
	}

	if svcInstance == nil {
		svcInstance, err = instance.NewInstance(ctx, svc, p.config, p.intfMgr, p.arpMgr, p.routeMgr, p.nodeLabelManager, wg)
		if err != nil {
			metrics.ServiceReconcileErrorsTotal.WithLabelValues(svc.Namespace, svc.Name, "new_instance").Inc()
			return fmt.Errorf("unable to create instance for service %s/%s", svc.Namespace, svc.Name)
		}
		p.ServiceInstances = append(p.ServiceInstances, svcInstance)
	}
	p.trackService(svc)

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

// RecoverAddresses removes tagged addresses that are no longer owned by this node.
func (p *Processor) RecoverAddresses(ctx context.Context) error {
	p.recoveryMu.Lock()
	defer p.recoveryMu.Unlock()
	if p.recovered || p.clientSet == nil || p.config.RoutingProtocol < 4 {
		return nil
	}

	services, err := p.clientSet.CoreV1().Services(p.config.ServiceNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list Services for address recovery: %w", err)
	}
	holders := make(map[string]string)
	retainedVIPs := make(map[string]struct{})
	if p.config.LeaderElectionType != "etcd" {
		if err := p.retainAnnotatedLeaseVIPs(ctx, holders, retainedVIPs); err != nil {
			return err
		}
	}
	for index := range services.Items {
		service := &services.Items[index]
		if !p.serviceOwnsRecoverableVIP(service) {
			continue
		}
		retain, err := p.serviceAddressRetained(ctx, service, holders)
		if err != nil {
			return err
		}
		if retain {
			for _, address := range serviceVIPAddresses(service) {
				retainedVIPs[address] = struct{}{}
			}
		}
	}
	canClean, err := p.retainControlPlaneVIPs(ctx, holders, retainedVIPs)
	if err != nil {
		return err
	}
	if !canClean {
		return nil
	}
	retained, err := vip.RetainedKubeVIPAddressKeys(p.config.RoutingProtocol, retainedVIPs)
	if err != nil {
		return fmt.Errorf("find retained kube-vip addresses: %w", err)
	}
	if _, err := vip.CleanupKubeVIPAddresses(p.config.RoutingProtocol, retained); err != nil {
		return fmt.Errorf("remove orphaned kube-vip addresses: %w", err)
	}
	p.recovered = true
	return nil
}

func (p *Processor) retainAnnotatedLeaseVIPs(ctx context.Context, holders map[string]string,
	retainedVIPs map[string]struct{}) error {
	namespace := v1.NamespaceAll
	if p.config.ServiceNamespace != "" {
		namespace = p.config.ServiceNamespace
	}
	leaseList, err := p.clientSet.CoordinationV1().Leases(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list Leases for address recovery: %w", err)
	}
	for index := range leaseList.Items {
		resource := &leaseList.Items[index]
		holder := ""
		if resource.Spec.HolderIdentity != nil {
			holder = *resource.Spec.HolderIdentity
		}
		holders[resource.Namespace+"/"+resource.Name] = holder
		encoded := resource.Annotations[kubevip.LeaseVIPs]
		if encoded == "" || holder != p.config.NodeName || !leaseOwnershipCurrent(resource, time.Now()) {
			continue
		}
		metadata, err := kubevip.ParseLeaseVIPs(encoded)
		if err != nil {
			return fmt.Errorf("parse Lease %s/%s VIP ownership: %w", resource.Namespace, resource.Name, err)
		}
		if metadata.IFAProto != p.config.RoutingProtocol {
			continue
		}
		for _, claimedVIP := range metadata.VIPs {
			retainedVIPs[claimedVIP.Value] = struct{}{}
		}
	}
	return nil
}

func leaseOwnershipCurrent(resource *coordinationv1.Lease, now time.Time) bool {
	if resource.Spec.RenewTime == nil || resource.Spec.LeaseDurationSeconds == nil {
		return true
	}
	return now.Before(resource.Spec.RenewTime.Add(time.Duration(*resource.Spec.LeaseDurationSeconds) * time.Second))
}

func (p *Processor) serviceOwnsRecoverableVIP(service *v1.Service) bool {
	classFilter := p.lbClassFilter
	if classFilter == nil {
		classFilter = lbClassFilter
	}
	return service != nil && service.Spec.Type == v1.ServiceTypeLoadBalancer &&
		service.Annotations[kubevip.LoadbalancerIgnore] != "true" &&
		!classFilter(service, p.config)
}

func (p *Processor) serviceAddressRetained(ctx context.Context, service *v1.Service, holders map[string]string) (bool, error) {
	if p.config.LeaderElectionType == "etcd" {
		return true, nil
	}
	forced := p.config.PerServiceElectionOnDemand && service.Annotations[kubevip.ForcePerServiceElection] == "true"
	usesGlobal := p.config.EnableARP || p.config.EnableWireguard ||
		((p.config.EnableBGP || p.config.EnableRoutingTable) && p.config.EnableLeaderElection)
	if !p.config.EnableServicesElection && !forced && !usesGlobal {
		return true, nil
	}
	namespace, name := p.serviceRecoveryLease(service)
	id := lease.NewID(p.config.LeaderElectionType, namespace, name)
	local, err := p.isLocalLeaseHolder(ctx, id, holders)
	if err != nil {
		return false, fmt.Errorf("get Service lease %q for address recovery: %w", id.NamespacedName(), err)
	}
	return local, nil
}

func (p *Processor) serviceRecoveryLease(service *v1.Service) (string, string) {
	if p.config.EnableServicesElection ||
		p.config.PerServiceElectionOnDemand && service.Annotations[kubevip.ForcePerServiceElection] == "true" {
		return lease.ServiceName(service)
	}
	return lease.NamespaceName(p.config.ServicesLeaseName, p.config)
}

func (p *Processor) retainControlPlaneVIPs(ctx context.Context, holders map[string]string, retainedVIPs map[string]struct{}) (bool, error) {
	if !p.config.EnableControlPlane {
		return true, nil
	}
	addresses, known := configuredVIPAddresses(p.config)
	if !known {
		log.Warn("skipping address recovery for hostname-backed control-plane VIP")
		return false, nil
	}
	if p.config.LeaderElectionType == "etcd" || !p.config.EnableLeaderElection {
		for _, address := range addresses {
			retainedVIPs[address] = struct{}{}
		}
		return true, nil
	}
	namespace, name := lease.NamespaceName(p.config.LeaseName, p.config)
	id := lease.NewID(p.config.LeaderElectionType, namespace, name)
	local, err := p.isLocalLeaseHolder(ctx, id, holders)
	if err != nil {
		return false, fmt.Errorf("get control-plane lease for address recovery: %w", err)
	}
	if local {
		for _, address := range addresses {
			retainedVIPs[address] = struct{}{}
		}
	}
	return true, nil
}

func configuredVIPAddresses(config *kubevip.Config) ([]string, bool) {
	configured := config.VIP
	if config.Address != "" {
		configured = config.Address
	}
	addresses := make([]string, 0)
	for _, value := range vip.Split(configured) {
		address := net.ParseIP(utils.StripCIDR(value))
		if address == nil {
			return nil, false
		}
		addresses = append(addresses, address.String())
	}
	return addresses, true
}

func (p *Processor) isLocalLeaseHolder(ctx context.Context, id lease.ID, holders map[string]string) (bool, error) {
	holder, err := p.kubernetesLeaseHolder(ctx, id, holders)
	if err != nil {
		return false, err
	}
	return holder == p.config.NodeName, nil
}

func (p *Processor) kubernetesLeaseHolder(ctx context.Context, id lease.ID, holders map[string]string) (string, error) {
	key := id.NamespacedName()
	if holder, found := holders[key]; found {
		return holder, nil
	}
	resource, err := p.clientSet.CoordinationV1().Leases(id.Namespace()).Get(ctx, id.Name(), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		holders[key] = ""
		return "", nil
	}
	if err != nil {
		return "", err
	}
	holder := ""
	if resource.Spec.HolderIdentity != nil {
		holder = *resource.Spec.HolderIdentity
	}
	holders[key] = holder
	return holder, nil
}

func serviceVIPAddresses(service *v1.Service) []string {
	addresses, _ := instance.FetchServiceAddresses(service)
	ingress, _ := instance.FetchLoadBalancerIngress(service)
	return append(addresses, ingress...)
}

// ElectionVIPs returns configured Service VIPs in stable Service creation order.
func (p *Processor) ElectionVIPs(ctx context.Context) ([]string, error) {
	if p == nil || p.clientSet == nil {
		return nil, nil
	}
	serviceList, err := p.clientSet.CoreV1().Services(p.config.ServiceNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list Services for election VIP metadata: %w", err)
	}
	services := make([]*v1.Service, 0, len(serviceList.Items))
	for index := range serviceList.Items {
		service := &serviceList.Items[index]
		if p.serviceOwnsRecoverableVIP(service) {
			services = append(services, service)
		}
	}
	slices.SortFunc(services, func(first, second *v1.Service) int {
		return first.CreationTimestamp.Time.Compare(second.CreationTimestamp.Time)
	})
	vips := make([]string, 0)
	for _, service := range services {
		vips = append(vips, serviceVIPAddresses(service)...)
	}
	return vips, nil
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
	var svcCtx *servicecontext.Context
	for {
		var err error
		svcCtx, err = p.getServiceContext(svc.UID)
		if err != nil {
			return fmt.Errorf("(svcs) unable to get context: %w", err)
		}
		if svcCtx != nil {
			svcCtx.Cancel()
			svcCtx.CallLeaderCancel()
			if err := svcCtx.WaitForWatchingStopped(context.Background()); err != nil {
				return fmt.Errorf("wait for service watcher: %w", err)
			}
		}

		unlockService := p.lockService(svc.UID)
		currentCtx, err := p.getServiceContext(svc.UID)
		if err != nil {
			unlockService()
			return fmt.Errorf("(svcs) unable to get context: %w", err)
		}
		if currentCtx == svcCtx {
			defer unlockService()
			break
		}
		unlockService()
	}

	if svcCtx != nil {
		// If no leader election is enabled, delete routes here
		if !p.config.EnableLeaderElection && !p.config.EnableServicesElection &&
			p.config.EnableRoutingTable && svcCtx.HasConfiguredNetworks() {
			if errs := endpoints.ClearRoutes(svc, &p.ServiceInstances, p.routeMgr); len(errs) == 0 {
				svcCtx.ConfiguredNetworks.Clear()
			}
		}

		// Delete synchronously even with per-Service election. Waiting for the
		// election callback leaves a window in which a queued callback can add the
		// deleted Service again.
		if err := p.deleteService(context.WithoutCancel(svcCtx.Ctx), svc.UID); err != nil {
			log.Error(err.Error())
		}

		log.Warn("(svcs) The load balancer was deleted, cancelling context", "namespace", svc.Namespace, "name", svc.Name, "uid", svc.UID)
		ns, name := lease.ServiceName(svc)
		leaseID := lease.NewID(p.config.LeaderElectionType, ns, name)
		p.leaseMgr.Delete(leaseID, lease.ServiceNamespacedName(svc), nil)
		p.svcMap.CompareAndDelete(svc.UID, svcCtx)
		log.Info("(svcs) deleted", "service name", svc.Name, "namespace", svc.Namespace)
	}
	p.untrackService(svc)

	return nil
}

func (p *Processor) withActiveService(uid types.UID, svcCtx *servicecontext.Context, reconcile func()) bool {
	unlockService := p.lockService(uid)
	defer unlockService()
	if svcCtx.Ctx.Err() != nil {
		return false
	}
	current, err := p.getServiceContext(uid)
	if err != nil || current != svcCtx {
		return false
	}
	reconcile()
	return true
}

func (p *Processor) serviceContextCurrent(uid types.UID, svcCtx *servicecontext.Context) bool {
	unlockService := p.lockService(uid)
	defer unlockService()
	if svcCtx == nil || svcCtx.Ctx.Err() != nil {
		return false
	}
	current, err := p.getServiceContext(uid)
	return err == nil && current == svcCtx
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
			log.Error("failed to unlock service lifecycle", "uid", uid, "err", err)
		}
	}
}

func (p *Processor) Stop() {
	p.mutex.Lock()
	defer p.mutex.Unlock()

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
	p.svcMap.Delete(uid)
	p.untrackServiceUID(uid)
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

func (p *Processor) trackService(svc *v1.Service) {
	p.servicesMu.Lock()
	defer p.servicesMu.Unlock()
	if p.services == nil {
		p.services = make(map[types.NamespacedName]types.UID)
	}
	p.services[types.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}] = svc.UID
	p.updateActiveServicesMetricLocked()
}

func (p *Processor) untrackService(svc *v1.Service) {
	p.servicesMu.Lock()
	defer p.servicesMu.Unlock()
	key := types.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}
	if p.services[key] == svc.UID {
		delete(p.services, key)
	}
	p.updateActiveServicesMetricLocked()
}

func (p *Processor) untrackServiceUID(uid types.UID) {
	p.servicesMu.Lock()
	defer p.servicesMu.Unlock()
	for key, currentUID := range p.services {
		if currentUID == uid {
			delete(p.services, key)
		}
	}
	p.updateActiveServicesMetricLocked()
}

func (p *Processor) updateActiveServicesMetricLocked() {
	counts := map[string]int{}
	for service := range p.services {
		counts[service.Namespace]++
	}
	metrics.ActiveServices.Reset()
	for ns, count := range counts {
		metrics.ActiveServices.WithLabelValues(ns).Set(float64(count))
	}
}
