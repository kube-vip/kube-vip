package services

import (
	"context"
	"errors"
	"fmt"
	log "log/slog"
	"net"
	"reflect"
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

var errServiceAddressPending = errors.New("service load-balancer address pending")

type Processor struct {
	config        *kubevip.Config
	lbClassFilter func(svc *v1.Service, config *kubevip.Config) bool
	svcMap        sync.Map

	// instancesMutex protects membership of ServiceInstances. Mutable fields on each
	// instance are protected separately by serviceLocks, keyed by Instance.UID().
	ServiceInstances []*instance.Instance
	instancesMutex   sync.RWMutex
	serviceCleanupMu sync.Mutex
	recoveryMu       sync.Mutex
	recovered        bool
	serviceLocks     keymutex.KeyMutex
	serviceLocksOnce sync.Once
	electionsMutex   sync.Mutex
	elections        map[string]*serviceElection
	nextMemberToken  atomic.Uint64
	electionLoops    sync.Map

	bgpServer *bgp.Server

	clientSet   *kubernetes.Clientset
	rwClientSet *kubernetes.Clientset

	intfMgr *networkinterface.Manager
	arpMgr  *arp.Manager

	leaseMgr *lease.Manager

	// nodeLabelManager is the manager for the node labels
	nodeLabelManager node.Labeler

	electionMgr             *election.Manager
	electionRun             func(context.Context, *election.RunConfig, *kubevip.Config) error
	serviceSync             func(context.Context, *servicecontext.Context, *v1.Service, *sync.WaitGroup, bool) error
	scheduleElectionRestart func(func())
	instanceFactory         func(context.Context, *v1.Service, *sync.WaitGroup) (*instance.Instance, error)

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

	timer := prometheus.NewTimer(metrics.ServiceReconcileDuration.WithLabelValues(svc.Namespace))
	defer timer.ObserveDuration()

	if !serviceMatchesWatcher(svc, forcedOnly) {
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

	var svcInstance *instance.Instance
	var svcCtx *servicecontext.Context
	shouldGarbageCollect := false
	var err error
	if err := func() error {
		unlockService := p.lockService(svc.UID)
		defer unlockService()

		svcInstance = p.findServiceInstance(svc)
		_, usesCommonLease := svc.Annotations[kubevip.ServiceLease]
		if usesCommonLease && svc.Spec.ExternalTrafficPolicy != v1.ServiceExternalTrafficPolicyTypeCluster {
			metrics.ServiceReconcileErrorsTotal.WithLabelValues(svc.Namespace, svc.Name, "invalid_config").Inc()
			return fmt.Errorf("annotation %q cannot be used with service traffic policy other than %q, service %s/%s",
				kubevip.ServiceLease, v1.ServiceExternalTrafficPolicyTypeCluster, svc.Namespace, svc.Name)
		}

		svcCtx, err = p.getServiceContext(svc.UID)
		if err != nil {
			metrics.ServiceReconcileErrorsTotal.WithLabelValues(svc.Namespace, svc.Name, "service_context").Inc()
			return fmt.Errorf("failed to get service context: %w", err)
		}
		if event.Type == watch.Modified && svcInstance != nil {
			shouldGarbageCollect = serviceChanged(svcInstance, svc)
		}
		return nil
	}(); err != nil {
		return err
	}
	if svcCtx != nil && svcCtx.Ctx.Err() != nil {
		svcCtx, err = p.ensureServiceContext(ctx, svc)
		if err != nil {
			return fmt.Errorf("replace cancelled service context: %w", err)
		}
	}

	// The modified event should only be triggered if the service has been modified (i.e. moved somewhere else)
	if event.Type == watch.Modified {
		if shouldGarbageCollect {
			// This service has been modified, but it was also active.
			if svcCtx != nil {
				log.Warn("(svcs) The load balancer has changed, cancelling original load balancer")
				oldService := svc
				if svcInstance != nil && svcInstance.ServiceSnapshot != nil {
					oldService = svcInstance.ServiceSnapshot
				}
				//Set it to inactive
				svcCtx.Cancel()

				if err := p.deleteService(ctx, svc.UID); err != nil {
					metrics.ServiceReconcileErrorsTotal.WithLabelValues(svc.Namespace, svc.Name, "delete_service").Inc()
					log.Error("(svc) unable to remove", "service", svc.UID)
				}
				p.leaveServiceElectionForContext(svcCtx, oldService)
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

	if svcCtx == nil || svcCtx.Ctx.Err() != nil {
		svcCtx, err = p.ensureServiceContext(ctx, svc)
		if err != nil {
			return fmt.Errorf("failed to get service context: %w", err)
		}
	}

	if svcInstance == nil {
		var instanceAdded bool
		svcInstance, instanceAdded, err = p.admitServiceInstance(ctx, svc, wg)
		if err != nil {
			return err
		}
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

// admitServiceInstance constructs and tracks a Service instance under its
// Service lock. Callers must not already hold that lock.
func (p *Processor) admitServiceInstance(ctx context.Context, svc *v1.Service, wg *sync.WaitGroup) (*instance.Instance, bool, error) {
	unlockService := p.lockService(svc.UID)
	defer unlockService()

	serviceInstance := p.findServiceInstance(svc)
	if serviceInstance != nil {
		return serviceInstance, false, nil
	}

	serviceInstance, err := p.createServiceInstance(ctx, svc, wg)
	if err != nil {
		metrics.ServiceReconcileErrorsTotal.WithLabelValues(svc.Namespace, svc.Name, "new_instance").Inc()
		return nil, false, fmt.Errorf("unable to create instance for service %s/%s: %w", svc.Namespace, svc.Name, err)
	}
	p.appendServiceInstance(serviceInstance)
	return serviceInstance, true, nil
}

func (p *Processor) createServiceInstance(ctx context.Context, svc *v1.Service, wg *sync.WaitGroup) (*instance.Instance, error) {
	if p.instanceFactory != nil {
		return p.instanceFactory(ctx, svc, wg)
	}
	return instance.NewInstance(ctx, svc, p.config, p.intfMgr, p.arpMgr, p.routeMgr, p.nodeLabelManager, wg)
}

func (p *Processor) waitForAddress(ctx context.Context, svc *v1.Service) (*v1.Service, error) {
	s, err := p.clientSet.CoreV1().Services(svc.Namespace).Get(ctx, svc.Name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get updated service data: %w", err)
	}
	addrs, hostnames := instance.FetchServiceAddresses(s)
	if len(addrs) == 0 && len(hostnames) == 0 {
		return nil, errServiceAddressPending
	}
	return s, nil
}

// RecoverAddresses removes tagged addresses that no longer belong to
// this node. Kubernetes lease holder identity is authoritative for per-Service
// election; modes without a per-Service lease retain Service VIPs conservatively.
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
		if !retain {
			continue
		}
		for _, address := range serviceVIPAddresses(service) {
			retainedVIPs[address] = struct{}{}
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
	removed, err := vip.CleanupKubeVIPAddresses(p.config.RoutingProtocol, retained)
	if err != nil {
		return fmt.Errorf("remove orphaned kube-vip addresses: %w", err)
	}
	p.recovered = true
	if removed != 0 {
		log.Info("removed orphaned kube-vip addresses", "count", removed)
	}
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
	expires := resource.Spec.RenewTime.Add(time.Duration(*resource.Spec.LeaseDurationSeconds) * time.Second)
	return now.Before(expires)
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
	if !p.config.EnableServicesElection && !p.usesGlobalServiceElection() {
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

func (p *Processor) usesGlobalServiceElection() bool {
	return p.config.EnableARP || p.config.EnableWireguard ||
		((p.config.EnableBGP || p.config.EnableRoutingTable) && p.config.EnableLeaderElection)
}

func (p *Processor) serviceRecoveryLease(service *v1.Service) (string, string) {
	if p.config.EnableServicesElection {
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
		// A hostname-backed control-plane VIP may currently resolve to an
		// address that is not present in the static config. Do not sweep any
		// tagged address until that ownership can be determined safely.
		log.Warn("skipping address recovery for hostname-backed control-plane VIP")
		return false, nil
	}
	if p.config.LeaderElectionType == "etcd" {
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
	if !local {
		return true, nil
	}
	for _, address := range addresses {
		retainedVIPs[address] = struct{}{}
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
	addresses = append(addresses, ingress...)
	return addresses
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
	return orderedServiceVIPs(services), nil
}

func (p *Processor) Delete(event watch.Event, forcedOnly bool) error {
	svc, ok := event.Object.(*v1.Service)
	if !ok || svc == nil {
		return fmt.Errorf("(svcs) unable to parse Kubernetes services from API watcher")
	}

	if !serviceMatchesWatcher(svc, forcedOnly) {
		return nil
	}

	return p.deleteTrackedService(svc)
}

func serviceMatchesWatcher(svc *v1.Service, forcedOnly bool) bool {
	forced := svc.Annotations[kubevip.ForcePerServiceElection] == "true"
	return forcedOnly == forced
}

func (p *Processor) deleteTrackedService(svc *v1.Service) error {
	svcCtx, cleanupCtx, err := p.retireServiceContext(svc)
	if err != nil {
		return err
	}

	if err := p.deleteService(cleanupCtx, svc.UID, svcCtx); err != nil {
		return fmt.Errorf("delete service %s/%s: %w", svc.Namespace, svc.Name, err)
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

// retireServiceContext preemptively cancels the published Service context before
// acquiring the Service lock, then rechecks ownership under the lock.
func (p *Processor) retireServiceContext(svc *v1.Service) (*servicecontext.Context, context.Context, error) {
	// Cancel before locking so in-flight Service work can release the lock.
	contextBeforeLock, err := p.cancelPublishedServiceContext(svc.UID)
	if err != nil {
		return nil, nil, fmt.Errorf("(svcs) unable to get context: %w", err)
	}

	unlockService := p.lockService(svc.UID)
	defer unlockService()

	// A replacement context may have been published while waiting for the lock.
	currentContext, err := p.getServiceContext(svc.UID)
	if err != nil {
		return nil, nil, fmt.Errorf("(svcs) unable to get context: %w", err)
	}

	cleanupCtx := context.Background()
	if currentContext != nil {
		log.Warn("(svcs) The load balancer was deleted, cancelling context", "namespace", svc.Namespace, "name", svc.Name, "uid", svc.UID)
		if currentContext != contextBeforeLock {
			currentContext.Cancel()
		}
		p.leaveServiceElectionForContext(currentContext, svc)
		cleanupCtx = context.WithoutCancel(currentContext.Ctx)
	}
	return currentContext, cleanupCtx, nil
}

func (p *Processor) cancelPublishedServiceContext(uid types.UID) (*servicecontext.Context, error) {
	svcCtx, err := p.getServiceContext(uid)
	if err != nil {
		return nil, err
	}
	if svcCtx != nil {
		svcCtx.Cancel()
	}
	return svcCtx, nil
}

// Stop acquires each instance's Service lock while stopping its workers and
// marking it for reconfiguration.
func (p *Processor) Stop() {
	p.svcMap.Range(func(_, value any) bool {
		if svcCtx, ok := value.(*servicecontext.Context); ok {
			svcCtx.Cancel()
		}
		return true
	})
	for _, instance := range p.serviceInstances() {
		unlockService := p.lockService(instance.UID())
		for _, cluster := range instance.Clusters {
			cluster.StopAndWait()
		}
		instance.AddCalled = false
		unlockService()
	}
}

// getServiceContext performs one concurrency-safe svcMap lookup. Callers that
// combine the result with other state changes must hold the Service lock for uid.
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

// ensureServiceContext returns the current usable context or creates one. It
// acquires the Service lock for svc.UID; callers must not already hold it.
func (p *Processor) ensureServiceContext(ctx context.Context, svc *v1.Service) (*servicecontext.Context, error) {
	for {
		observed, err := p.getServiceContext(svc.UID)
		if err != nil {
			return nil, err
		}
		if observed != nil && observed.Ctx.Err() != nil {
			if err := observed.WaitForWatchingStopped(ctx); err != nil {
				return nil, err
			}
		}

		current, retry, err := p.ensureServiceContextLocked(ctx, svc, observed)
		if err != nil {
			return nil, err
		}
		if retry {
			continue
		}
		return current, nil
	}
}

func (p *Processor) ensureServiceContextLocked(ctx context.Context, svc *v1.Service,
	observed *servicecontext.Context) (*servicecontext.Context, bool, error) {
	unlockService := p.lockService(svc.UID)
	defer unlockService()

	current, err := p.getServiceContext(svc.UID)
	if err != nil {
		return nil, false, err
	}
	if current != observed {
		return nil, true, nil
	}
	current = p.dropCancelledServiceContext(svc, current)
	if current == nil {
		current = servicecontext.New(ctx)
		p.svcMap.Store(svc.UID, current)
	}
	return current, false, nil
}

// dropCancelledServiceContext discards a service context whose context has already been
// cancelled, removing it from svcMap and returning nil so that callers create a fresh one.
//
// Callers wait for the old watcher lifecycle before invoking this function.
// The caller must hold the Service lock for svc.UID.
func (p *Processor) dropCancelledServiceContext(svc *v1.Service, svcCtx *servicecontext.Context) *servicecontext.Context {
	if svcCtx == nil || svcCtx.Ctx.Err() == nil {
		return svcCtx
	}
	if serviceInstance := p.findServiceInstance(svc); serviceInstance != nil {
		serviceInstance.AddCalled = false
	}
	p.svcMap.CompareAndDelete(svc.UID, svcCtx)
	return nil
}

// serviceChanged reads the tracked instance snapshot. The caller must hold the
// Service lock for i.UID().
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
		!ipFamilyPolicyEqual(svc.Spec.IPFamilyPolicy, i.ServiceSnapshot.Spec.IPFamilyPolicy) ||
		// DDNS was disabled/enabled
		svc.Annotations[kubevip.ServiceDDNS] != i.ServiceSnapshot.Annotations[kubevip.ServiceDDNS] ||
		// lease name was changed
		svc.Annotations[kubevip.ServiceLease] != i.ServiceSnapshot.Annotations[kubevip.ServiceLease]
}

func ipFamilyPolicyEqual(first, second *v1.IPFamilyPolicy) bool {
	if first == nil || second == nil {
		return first == second
	}
	return *first == *second
}

// updateActiveServicesMetric acquires each instance's Service lock before
// reading its snapshot.
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

// findServiceInstance protects the collection lookup only. The caller must hold
// the Service lock before accessing mutable fields on the returned instance.
func (p *Processor) findServiceInstance(service *v1.Service) *instance.Instance {
	p.instancesMutex.RLock()
	defer p.instancesMutex.RUnlock()
	return instance.FindServiceInstance(service, p.ServiceInstances)
}

// serviceInstances returns a stable copy of the collection. The caller must hold
// each instance's Service lock before accessing its mutable fields.
func (p *Processor) serviceInstances() []*instance.Instance {
	p.instancesMutex.RLock()
	defer p.instancesMutex.RUnlock()
	return append([]*instance.Instance(nil), p.ServiceInstances...)
}

// ServiceSnapshots returns stable copies for external observers such as
// diagnostics. It acquires each instance's Service lock while copying.
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

// appendServiceInstance adds inst to the tracked collection. The caller must
// hold the Service lock for inst.UID() to serialize logical membership changes;
// instancesMutex protects only the slice mutation.
func (p *Processor) appendServiceInstance(inst *instance.Instance) {
	p.instancesMutex.Lock()
	defer p.instancesMutex.Unlock()
	p.ServiceInstances = append(p.ServiceInstances, inst)
}

// detachServiceInstance removes the tracked instance for uid. The caller must
// hold the Service lock for uid to serialize logical membership changes;
// instancesMutex protects only the slice mutation.
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

// lockService serializes mutable state for one Service UID. The returned unlock
// function must be called exactly once; the lock is not reentrant.
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
