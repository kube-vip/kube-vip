package services

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	log "log/slog"

	"github.com/google/go-cmp/cmp"
	"github.com/vishvananda/netlink"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"

	"github.com/kube-vip/kube-vip/pkg/cluster"
	"github.com/kube-vip/kube-vip/pkg/egress"
	"github.com/kube-vip/kube-vip/pkg/endpoints"
	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/nftables"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	"github.com/kube-vip/kube-vip/pkg/upnp"
	"github.com/kube-vip/kube-vip/pkg/utils"
	"github.com/kube-vip/kube-vip/pkg/vip"
)

type ServiceInstanceAction string

type serviceExpectation struct {
	svcCtx    *servicecontext.Context
	lost      <-chan any
	version   uint64
	lifecycle serviceLifecycle
	valid     func() bool
	track     func(*instance.Instance)
}

const (
	ActionDelete ServiceInstanceAction = "delete"
	ActionAdd    ServiceInstanceAction = "add"
	ActionNone   ServiceInstanceAction = "none"

	// Default UPNP lease requested is 3600 seconds.
	defaultUPNPLeaseDuration = 1 * time.Hour
)

var errServiceActivationStale = errors.New("service activation is no longer current")

func (p *Processor) SyncServices(ctx *servicecontext.Context, svc *v1.Service, wg *sync.WaitGroup, usesLeaderElection bool) error {
	if !p.serviceIsLatestDesired(svc) {
		return nil
	}
	return p.syncServices(ctx.Ctx, ctx, svc, wg, usesLeaderElection, nil)
}

func (p *Processor) syncServices(activationCtx context.Context, svcCtx *servicecontext.Context, svc *v1.Service, wg *sync.WaitGroup, usesLeaderElection bool, expected *serviceExpectation) error {
	log.Debug("[STARTING] Service Sync", "namespace", svc.Namespace, "name", svc.Name, "uid", svc.UID)
	if !p.serviceExpectationCurrent(svc, expected) {
		return nil
	}
	if expected != nil && expected.track != nil {
		unlock := p.lockService(svc.UID)
		if p.serviceExpectationCurrentLocked(svc, expected) {
			expected.track(p.findServiceInstance(svc))
		}
		unlock()
	}

	// Iterate through the synchronising services
	action := p.getServiceInstanceAction(svc)
	switch action {
	case ActionDelete:
		log.Debug("[service] delete", "namespace", svc.Namespace, "name", svc.Name, "uid", svc.UID)
		if err := p.deleteExpectedService(activationCtx, svc, expected); err != nil {
			return fmt.Errorf("error deleting service %s/%s: %w", svc.Namespace, svc.Name, err)
		}
	case ActionAdd:
		log.Debug("[service] add", "namespace", svc.Namespace, "name", svc.Name, "uid", svc.UID)
		if !usesLeaderElection {
			select {
			case <-activationCtx.Done():
				return nil
			case <-svcCtx.Readiness():
			}
		}

		if err := p.addService(activationCtx, svc, wg, expected); err != nil {
			return fmt.Errorf("error adding service %s/%s: %w", svc.Namespace, svc.Name, err)
		}

	case ActionNone:
		log.Debug("[service] no action", "namespace", svc.Namespace, "name", svc.Name, "uid", svc.UID)
		// Egress: when the service reaches ActionNone it means AddCalled is already true.
		// If ActiveEndpoint is now set (by the endpoint watcher) and the service has a
		// LB IP, the initial addService call may have missed the SNAT configuration because
		// ActiveEndpoint was not yet present. Re-run it here.
		if svc.Annotations[kubevip.Egress] == "true" && svc.Annotations[kubevip.ActiveEndpoint] != "" {
			if err := p.updateEgressConfiguration(activationCtx, svc); err != nil {
				log.Warn("[service] egress reconfigure on ActionNone", "service", svc.Name, "namespace", svc.Namespace, "err", err)
			}
		}
	}
	log.Debug("[FINISHED] Service Sync", "namespace", svc.Namespace, "name", svc.Name, "uid", svc.UID)
	return nil
}

func (p *Processor) getServiceInstanceAction(svc *v1.Service) ServiceInstanceAction {
	unlockService := p.lockService(svc.UID)
	defer unlockService()

	// protect against multiple calls
	// get the annotations or legacy values from manual configuration
	addresses, hostnames := instance.FetchServiceAddresses(svc)
	// get the status information of the LB Service
	statusAddresses, _ := instance.FetchLoadBalancerIngress(svc)
	inst := p.findServiceInstance(svc)
	if inst != nil {
		if !inst.AddCalled {
			return ActionAdd
		}
		for _, address := range addresses {
			// handle the case where the service instance needs to be deleted
			if inst.IsDHCPv4 {
				if address != "0.0.0.0" {
					return ActionDelete
				}
				if len(svc.Status.LoadBalancer.Ingress) > 0 && !slices.Contains(statusAddresses, inst.DHCPInterfaceIPv4) {
					return ActionDelete
				}
			} else {
				if address == "0.0.0.0" {
					return ActionDelete
				}
				if len(svc.Status.LoadBalancer.Ingress) > 0 && !slices.Contains(statusAddresses, address) {
					return ActionDelete
				}
			}
			if inst.IsDHCPv6 {
				if address != "::" {
					return ActionDelete
				}
				if len(svc.Status.LoadBalancer.Ingress) > 0 && !slices.Contains(statusAddresses, inst.DHCPInterfaceIPv6) {
					return ActionDelete
				}
			} else {
				if address == "::" {
					return ActionDelete
				}
				if len(svc.Status.LoadBalancer.Ingress) > 0 && !slices.Contains(statusAddresses, address) {
					return ActionDelete
				}
			}
			if len(svc.Status.LoadBalancer.Ingress) > 0 && !comparePortsAndPortStatuses(svc) {
				return ActionDelete
			}
		}
		// If we reach here, it means the service instance matches the service UID and is not a DHCP service, so we can return "no action"
		return ActionNone
	}
	if len(addresses) > 0 || len(hostnames) > 0 {
		log.Debug("no matching service instance found", "service", svc.Name, "namespace", svc.Namespace, "uid", svc.UID, "addresses", addresses, "hostnames", hostnames)
		return ActionAdd // If no matching instance is found, we need to add a new service instance
	}
	return ActionNone
}

func comparePortsAndPortStatuses(svc *v1.Service) bool {
	if len(svc.Status.LoadBalancer.Ingress) == 0 {
		return false
	}
	portsStatus := svc.Status.LoadBalancer.Ingress[0].Ports
	if len(portsStatus) != len(svc.Spec.Ports) {
		return false
	}
	for i, portSpec := range svc.Spec.Ports {
		if portsStatus[i].Port != portSpec.Port || portsStatus[i].Protocol != portSpec.Protocol {
			return false
		}
	}
	return true
}

func (p *Processor) addService(ctx context.Context, svc *v1.Service, wg *sync.WaitGroup, expectation ...*serviceExpectation) error {
	var expected *serviceExpectation
	if len(expectation) > 0 {
		expected = expectation[0]
	}
	unlockService := p.lockService(svc.UID)
	unlockVIPs := func() {}
	updateMetric := false
	defer func() {
		unlockVIPs()
		unlockService()
		if updateMetric {
			p.updateActiveServicesMetric()
		}
	}()

	startTime := time.Now()
	if !p.serviceExpectationCurrentLocked(svc, expected) {
		return nil
	}

	current := p.findServiceInstance(svc)
	var inst *instance.Instance
	if current != nil {
		inst = current
		if expected != nil && expected.track != nil {
			expected.track(inst)
		}
		if inst.AddCalled {
			return nil
		}
		unlockVIPs = p.lockInstanceResources(svc, inst)
		inst.AddCalled = true
	} else {
		unlockVIPs = p.lockServiceResources(svc)
		var err error
		inst, err = p.makeServiceInstance(ctx, svc, wg)
		if err != nil {
			return err
		}
		if ctx.Err() != nil || !p.serviceExpectationCurrentLocked(svc, expected) {
			if cleanupErr := p.deleteCurrentServiceLocked(context.WithoutCancel(ctx), inst); cleanupErr != nil {
				return fmt.Errorf("discard stale service instance: %w", cleanupErr)
			}
			return errServiceActivationStale
		}
		inst.AddCalled = true

		p.appendServiceInstance(inst)
	}
	if current == nil && expected != nil && expected.track != nil {
		expected.track(inst)
	}
	if err := p.configureService(ctx, inst, svc, wg, expected); err != nil {
		cleanupErr := p.deleteCurrentServiceLocked(context.WithoutCancel(ctx), inst)
		if cleanupErr != nil {
			return fmt.Errorf("configure service %s/%s: %w; cleanup: %w", svc.Namespace, svc.Name, err, cleanupErr)
		}
		updateMetric = true
		return err
	}

	finishTime := time.Since(startTime)
	updateMetric = true
	log.Info("[service]", "service", svc.Name, "namespace", svc.Namespace, "synchronised in", fmt.Sprintf("%dms", finishTime.Milliseconds()))

	return nil
}

func (p *Processor) configureService(ctx context.Context, inst *instance.Instance, svc *v1.Service, wg *sync.WaitGroup, expectation ...*serviceExpectation) error {
	var expected *serviceExpectation
	if len(expectation) > 0 {
		expected = expectation[0]
	}
	if !p.serviceExpectationCurrentLocked(svc, expected) {
		return errServiceActivationStale
	}
	checkCurrent := func() error {
		if ctx.Err() != nil || !p.serviceExpectationCurrentLocked(svc, expected) {
			return errServiceActivationStale
		}
		return nil
	}
	current := p.findServiceInstance(svc)
	if current != inst {
		return fmt.Errorf("service instance no longer active for %s/%s", svc.Namespace, svc.Name)
	}

	// is not a global leader election mode
	if p.config.EnableServicesElection || (!p.config.EnableARP && !p.config.EnableLeaderElection) || (!p.config.EnableARP && !p.config.EnableRoutingTable) {
		for x := range inst.VIPConfigs {
			if err := checkCurrent(); err != nil {
				return err
			}
			log.Debug("[service] starting loadbalancer for service", "name", svc.Name, "namespace", svc.Namespace, "uid", svc.UID)
			if err := inst.Clusters[x].StartLoadBalancerService(ctx, inst.VIPConfigs[x], p.bgpServer, lease.ServiceNamespacedName(svc), wg); err != nil {
				return fmt.Errorf("failed to start lb: %w", err)
			}
			if err := checkCurrent(); err != nil {
				return err
			}
		}
	}

	p.upnpMap(ctx, inst)

	if inst.IsDHCPv4 {
		wg.Go(func() {
			index := -1
			for i := range inst.VIPConfigs {
				ip := net.ParseIP(inst.VIPConfigs[i].VIP)
				if ip.To4() != nil {
					index = i
					break
				}
			}
			if index == -1 {
				log.Error("unable to find proper VIPConfig for the DHCPv4")
			} else {
				for ip := range inst.DHCPv4Client.IPChannel() {
					unlockService := p.lockService(svc.UID)
					current := p.findServiceInstance(svc)
					if current != inst {
						unlockService()
						return
					}
					log.Debug("IP changed", "ip", ip)
					inst.VIPConfigs[index].VIP = ip
					inst.DHCPInterfaceIPv4 = ip
					if !p.config.DisableServiceUpdates {
						if err := p.updateStatus(ctx, inst); err != nil {
							log.Warn("updating svc", "err", err)
						}
					}
					unlockService()
				}
				log.Debug("IPv4 update channel closed, stopping")
			}

		})
	}

	if inst.IsDHCPv6 {
		wg.Go(func() {
			index := -1
			for i := range inst.VIPConfigs {
				ip := net.ParseIP(inst.VIPConfigs[i].VIP)
				if ip.To4() == nil {
					index = i
					break
				}
			}
			if index == -1 {
				log.Error("unable to find proper VIPConfig for the DHCPv6")
			} else {
				for ip := range inst.DHCPv4Client.IPChannel() {
					unlockService := p.lockService(svc.UID)
					current := p.findServiceInstance(svc)
					if current != inst {
						unlockService()
						return
					}
					log.Debug("IP changed", "ip", ip)
					inst.VIPConfigs[index].VIP = ip
					inst.DHCPInterfaceIPv6 = ip
					if !p.config.DisableServiceUpdates {
						if err := p.updateStatus(ctx, inst); err != nil {
							log.Warn("updating svc", "err", err)
						}
					}
					unlockService()
				}
				log.Debug("IPv6 update channel closed, stopping")
			}
		})
	}

	if !p.config.DisableServiceUpdates {
		if err := checkCurrent(); err != nil {
			return err
		}
		log.Debug("[service] update", "namespace", inst.ServiceSnapshot.Namespace, "name", inst.ServiceSnapshot.Name)
		if err := p.updateStatus(ctx, inst); err != nil {
			log.Error("[service] updating status", "namespace", inst.ServiceSnapshot.Namespace, "name", inst.ServiceSnapshot.Name, "err", err)
		}
		if err := checkCurrent(); err != nil {
			return err
		}
	}

	egressService := serviceSnapshotForEgress(inst, svc)
	serviceIPs, _ := instance.FetchServiceAddresses(egressService)
	// Check if we need to flush any conntrack connections (due to some dangling conntrack connections)
	if egressService.Annotations[kubevip.FlushContrack] == "true" {

		log.Debug("[service] Flushing conntrack rules", "service", egressService.Name, "namespace", egressService.Namespace)
		for _, serviceIP := range serviceIPs {
			err := vip.DeleteExistingSessions(serviceIP, false, egressService.Annotations[kubevip.EgressDestinationPorts], egressService.Annotations[kubevip.EgressSourcePorts])
			if err != nil {
				log.Error("[service] flushing any remaining egress connections", "service", egressService.Name, "namespace", egressService.Namespace, "err", err)
			}
			err = vip.DeleteExistingSessions(serviceIP, true, egressService.Annotations[kubevip.EgressDestinationPorts], egressService.Annotations[kubevip.EgressSourcePorts])
			if err != nil {
				log.Error("[service] flushing any remaining ingress connections", "service", egressService.Name, "namespace", egressService.Namespace, "err", err)
			}
		}
	}

	// Check if egress is enabled on the service, if so we'll need to configure some rules
	if egressService.Annotations[kubevip.Egress] == "true" && len(serviceIPs) > 0 {
		if err := checkCurrent(); err != nil {
			return err
		}
		log.Debug("[service] enabling egress", "service", egressService.Name, "namespace", egressService.Namespace)
		// If we'er not using NFtables, then ensure that the correct iptables modules are loaded
		if p.config.EgressWithNftables {
			// Ensure that kernel modules are loaded and report back missing modules.
			err := p.nftablesCheck()
			if err != nil {
				log.Warn("[service] configuring nft egress", "service", svc.Name, "namespace", svc.Namespace, "err", err)
			}
		} else {
			// Ensure that kernel modules are loaded and report back missing modules.
			err := p.iptablesCheck()
			if err != nil {
				log.Warn("[service] configuring egress", "service", svc.Name, "namespace", svc.Namespace, "err", err)
			}
		}
		var podIP string
		errList := []error{}
		configuredRules := 0
		useInternalNftables := egressService.Annotations[kubevip.EgressInternal] != "" || p.config.EgressWithNftables
		preparedFamilies := map[bool]bool{}

		// Should egress be IPv6
		if egressService.Annotations[kubevip.EgressIPv6] == "true" {
			// Does the service have an active IPv6 endpoint
			if egressService.Annotations[kubevip.ActiveEndpointIPv6] != "" {
				for _, serviceIP := range serviceIPs {
					if !p.config.EnableEndpoints && utils.IsIPv6(serviceIP) {

						podIP = egressService.Annotations[kubevip.ActiveEndpointIPv6]
						if useInternalNftables && !preparedFamilies[true] {
							if err := p.prepareEgressNftablesTable(string(egressService.UID), true); err != nil {
								errList = append(errList, err)
								continue
							}
							preparedFamilies[true] = true
						}

						applied := false
						err := p.configureEgress(ctx, serviceIP, podIP, egressService.Namespace, string(egressService.UID), egressService.Annotations, &applied)
						if err != nil {
							errList = append(errList, err)
							log.Warn("[service] configuring egress IPv6", "service", egressService.Name, "namespace", egressService.Namespace, "err", err)
						} else if applied {
							configuredRules++
						}
					}
				}
			}
		} else if egressService.Annotations[kubevip.ActiveEndpoint] != "" { // Not expected to be IPv6, so should be an IPv4 address
			for _, serviceIP := range serviceIPs {
				podIPs := egressService.Annotations[kubevip.ActiveEndpoint]
				if !p.config.EnableEndpoints && utils.IsIPv6(serviceIP) {
					podIPs = egressService.Annotations[kubevip.ActiveEndpointIPv6]
				}
				ipv6 := utils.IsIPv6(serviceIP)
				if useInternalNftables && !preparedFamilies[ipv6] {
					if err := p.prepareEgressNftablesTable(string(egressService.UID), ipv6); err != nil {
						errList = append(errList, err)
						continue
					}
					preparedFamilies[ipv6] = true
				}
				applied := false
				err := p.configureEgress(ctx, serviceIP, podIPs, egressService.Namespace, string(egressService.UID), egressService.Annotations, &applied)
				if err != nil {
					errList = append(errList, err)
					log.Warn("[service] configuring egress IPv4", "service", egressService.Name, "namespace", egressService.Namespace, "err", err)
				} else if applied {
					configuredRules++
				}
			}
		}
		if len(errList) == 0 {
			if configuredRules > 0 && useInternalNftables {
				if err := p.updateEgressNftablesTableAnnotation(ctx, egressService); err != nil {
					return err
				}
			}
		}
		if err := checkCurrent(); err != nil {
			return err
		}
	}

	// Configure WireGuard DNAT rules if WireGuard is enabled
	if p.config.EnableWireguard {
		if err := checkCurrent(); err != nil {
			return err
		}
		log.Debug("[service] configuring WireGuard DNAT rules", "service", svc.Name, "namespace", svc.Namespace)
		if err := p.addServiceWireguard(ctx, svc); err != nil {
			log.Warn("[service] failed to configure WireGuard DNAT", "service", svc.Name, "namespace", svc.Namespace, "err", err)
			// Don't fail the entire service if WireGuard config fails
		}
		if err := checkCurrent(); err != nil {
			return err
		}
	}

	if err := checkCurrent(); err != nil {
		return err
	}
	labels := generateLabelsFromService(svc, kubevip.ServiceProvided)
	if err := p.nodeLabelManager.AddLabel(labels); err != nil {
		return fmt.Errorf("error adding label to node: %w", err)
	}
	inst.LabelAdded = true
	if err := checkCurrent(); err != nil {
		return err
	}

	return nil
}

func serviceSnapshotForEgress(inst *instance.Instance, service *v1.Service) *v1.Service {
	if inst == nil || inst.ServiceSnapshot == nil || service == nil {
		return service
	}

	merged := service.DeepCopy()
	if merged.Annotations == nil {
		merged.Annotations = make(map[string]string)
	}
	merged.Annotations[kubevip.ActiveEndpoint] = inst.ServiceSnapshot.Annotations[kubevip.ActiveEndpoint]
	merged.Annotations[kubevip.ActiveEndpointIPv6] = inst.ServiceSnapshot.Annotations[kubevip.ActiveEndpointIPv6]
	return merged
}

func (p *Processor) deleteService(ctx context.Context, uid types.UID, expectedCtx ...*servicecontext.Context) error {
	unlockService := p.lockService(uid)
	var expected *servicecontext.Context
	if len(expectedCtx) > 0 {
		expected = expectedCtx[0]
	}
	if expected != nil {
		currentCtx, err := p.getServiceContext(uid)
		if err != nil {
			unlockService()
			return err
		}
		if currentCtx != nil && currentCtx != expected {
			unlockService()
			return nil
		}
	}

	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{UID: uid}}
	err := p.deleteCurrentService(ctx, p.findServiceInstance(service))
	unlockService()
	if err == nil {
		p.updateActiveServicesMetric()
	}
	return err
}

func (p *Processor) deleteServiceInstance(ctx context.Context, expected *instance.Instance) error {
	return p.deleteServiceInstanceExcluding(ctx, expected, nil)
}

func (p *Processor) deleteServiceInstanceExcluding(ctx context.Context, expected *instance.Instance, stopping map[*instance.Instance]struct{}, forceDelete ...map[string]struct{}) error {
	return p.deleteServiceInstanceWithMode(ctx, expected, stopping, false, forceDelete...)
}

func (p *Processor) deleteServiceInstanceWithMode(ctx context.Context, expected *instance.Instance, stopping map[*instance.Instance]struct{}, leadershipLost bool, forceDelete ...map[string]struct{}) error {
	if expected == nil {
		return nil
	}

	unlockService := p.lockService(expected.UID())
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{UID: expected.UID()}}
	if p.findServiceInstance(service) != expected {
		unlockService()
		return nil
	}
	err := p.deleteCurrentServiceExcludingMode(ctx, expected, stopping, leadershipLost, forceDelete...)
	unlockService()
	if err == nil {
		p.updateActiveServicesMetric()
	}
	return err
}

// deleteCurrentService removes the supplied tracked instance while its Service key is held.
func (p *Processor) deleteCurrentService(ctx context.Context, serviceInstance *instance.Instance) error {
	return p.deleteCurrentServiceExcluding(ctx, serviceInstance, nil)
}

func (p *Processor) deleteCurrentServiceExcluding(ctx context.Context, serviceInstance *instance.Instance, stopping map[*instance.Instance]struct{}, forceDelete ...map[string]struct{}) error {
	return p.deleteCurrentServiceExcludingMode(ctx, serviceInstance, stopping, false, forceDelete...)
}

func (p *Processor) deleteCurrentServiceExcludingMode(ctx context.Context, serviceInstance *instance.Instance, stopping map[*instance.Instance]struct{}, leadershipLost bool, forceDelete ...map[string]struct{}) error {

	// If we've been through all services and not found the correct one then error
	if serviceInstance == nil {
		// TODO: - fix UX
		// return fmt.Errorf("unable to find/stop service [%s]", uid)
		log.Warn("unable to find/stop service")
		return nil
	}
	unlockVIPs := p.lockInstanceResources(serviceInstance.ServiceSnapshot, serviceInstance)
	defer unlockVIPs()
	return p.deleteCurrentServiceLockedWithOwnership(ctx, serviceInstance, stopping, firstAddressSet(forceDelete), leadershipLost)
}

func firstAddressSet(sets []map[string]struct{}) map[string]struct{} {
	if len(sets) == 0 {
		return nil
	}
	return sets[0]
}

func (p *Processor) deleteCurrentServiceLocked(ctx context.Context, serviceInstance *instance.Instance, stopping ...map[*instance.Instance]struct{}) error {
	var stopSet map[*instance.Instance]struct{}
	if len(stopping) != 0 {
		stopSet = stopping[0]
	}
	return p.deleteCurrentServiceLockedWithOwnership(ctx, serviceInstance, stopSet, nil, false)
}

func (p *Processor) deleteCurrentServiceLockedWithOwnership(ctx context.Context, serviceInstance *instance.Instance, stopSet map[*instance.Instance]struct{}, forceDelete map[string]struct{}, leadershipLost bool) error {

	updatedInstances := make([]*instance.Instance, 0)
	for _, inst := range p.serviceInstances() {
		if inst == serviceInstance {
			continue
		}
		updatedInstances = append(updatedInstances, inst)
	}

	if serviceInstance.LabelAdded {
		labels := generateLabelsFromService(serviceInstance.ServiceSnapshot, kubevip.ServiceProvided)
		if err := p.nodeLabelManager.RemoveLabel(labels); err != nil {
			return fmt.Errorf("error removing label from node: %w", err)
		}
		serviceInstance.LabelAdded = false
	}

	for _, c := range serviceInstance.Clusters {
		for n := range c.Network {
			c.Network[n].SetHasEndpoints(false)
		}
	}

	// Only active siblings own VIPs. Pre-created election instances have no
	// workers yet and must not prevent cleanup.
	vipSet := make(map[string]struct{})
	for x := range updatedInstances {
		if _, departing := stopSet[updatedInstances[x]]; departing {
			continue
		}
		for _, address := range p.activeInstanceAddresses(updatedInstances[x]) {
			vipSet[address] = struct{}{}
		}
	}

	if p.config.EnableBGP {
		endpoints.ClearBGPHostsByInstance(ctx, serviceInstance, p.bgpServer)
	}

	if p.config.EnableRoutingTable {
		if errs := endpoints.ClearRoutesByInstance(serviceInstance.ServiceSnapshot, serviceInstance, &updatedInstances, p.routeMgr); len(errs) > 0 {
			for _, err := range errs {
				log.Error("unable to clear routes", "err", err)
			}
		}
	}

	internalNftablesEgress := serviceInstance.ServiceSnapshot.Annotations[kubevip.EgressInternal] != "" || p.config.EgressWithNftables
	if serviceInstance.ServiceSnapshot.Annotations[kubevip.Egress] == "true" && internalNftablesEgress {
		if err := nftables.DeleteSNATFromAllTables(string(serviceInstance.ServiceSnapshot.UID)); err != nil {
			log.Error("[service] nftables egress teardown", "service", serviceInstance.ServiceSnapshot.Name, "err", err)
		}
	}

	for _, c := range serviceInstance.Clusters {
		preserve := make(map[string]struct{})
		for _, network := range c.Network {
			_, shared := vipSet[network.IP()]
			preserveLeadershipVIP := leadershipLost && p.config.PreserveVIPOnLeadershipLoss && !utils.IsIPv6(network.IP())
			if shared || preserveLeadershipVIP {
				preserve[network.IP()] = struct{}{}
			}
		}
		c.StopWorkersAndWaitPreserving(preserve)
	}

	finalDelete := make(map[string]struct{})
	if forceDelete == nil {
		for _, address := range ownedInstanceAddresses(serviceInstance) {
			finalDelete[address] = struct{}{}
		}
	} else {
		for address := range forceDelete {
			finalDelete[address] = struct{}{}
		}
	}
	if leadershipLost && p.config.PreserveVIPOnLeadershipLoss {
		for address := range finalDelete {
			if !utils.IsIPv6(address) {
				delete(finalDelete, address)
			}
		}
	}
	if len(finalDelete) != 0 {
		// Every designated address may have been preserved by an earlier stop
		// snapshot. Drain all departing generations that reference it before the
		// final ownership check and exact deletion.
		for sibling := range stopSet {
			if sibling == nil || sibling == serviceInstance {
				continue
			}
			for _, c := range sibling.Clusters {
				if clusterOwnsAnyAddress(c, finalDelete) {
					c.StopWorkersAndWaitPreserving(finalDelete)
				}
			}
		}
		active := make(map[string]struct{})
		for _, sibling := range p.serviceInstances() {
			if sibling == serviceInstance {
				continue
			}
			if _, departing := stopSet[sibling]; departing {
				continue
			}
			for _, address := range p.activeInstanceAddresses(sibling) {
				active[address] = struct{}{}
			}
		}
		for address := range active {
			delete(finalDelete, address)
		}
		for i, c := range serviceInstance.Clusters {
			if i >= len(serviceInstance.VIPConfigs) || serviceInstance.VIPConfigs[i] == nil {
				continue
			}
			if err := c.CleanupStoppedServiceNetworks(serviceInstance.VIPConfigs[i], finalDelete); err != nil {
				return err
			}
		}
	}

	if serviceInstance.IsVLAN && !instanceUsesVLAN(updatedInstances, serviceInstance.VLANInterface) {
		vlan, err := netlink.LinkByName(serviceInstance.VLANInterface)
		if err != nil && !isMissingLink(err) {
			return fmt.Errorf("[service] error finding VLAN Interface: %v", err)
		}
		if err == nil {
			if err := netlink.LinkDel(vlan); err != nil && !isMissingLink(err) {
				return fmt.Errorf("[service] error deleting VLAN interface: %v", err)
			}
		}
	}

	if serviceInstance.IsDHCPv4 || serviceInstance.IsDHCPv6 {
		if serviceInstance.IsDHCPv4 && serviceInstance.DHCPv4Client != nil {
			serviceInstance.DHCPv4Client.Stop()
		}

		if serviceInstance.IsDHCPv6 && serviceInstance.DHCPv6Client != nil {
			serviceInstance.DHCPv6Client.Stop()
		}

		if !instanceUsesDHCPInterface(updatedInstances, serviceInstance.DHCPInterface) {
			macvlan, err := netlink.LinkByName(serviceInstance.DHCPInterface)
			if err != nil && !isMissingLink(err) {
				return fmt.Errorf("[service] error finding VIP Interface: %v", err)
			}
			if err == nil {
				if err := netlink.LinkDel(macvlan); err != nil && !isMissingLink(err) {
					return fmt.Errorf("[service] error deleting DHCP Link: %v", err)
				}
			}
		}
	}

	// Legacy teardown removes common marking state, so retain the original
	// single-call behavior and address selection.
	if serviceInstance.ServiceSnapshot.Annotations[kubevip.Egress] == "true" && !internalNftablesEgress {
		address := serviceInstance.ServiceSnapshot.Spec.LoadBalancerIP
		_, shared := vipSet[address]
		if address != "" && !shared && serviceInstance.ServiceSnapshot.Annotations[kubevip.ActiveEndpoint] != "" {
			log.Info("[service] egress re-write enabled", "service", serviceInstance.ServiceSnapshot.Name)
			if err := egress.Teardown(serviceInstance.ServiceSnapshot.Annotations[kubevip.ActiveEndpoint], address, serviceInstance.ServiceSnapshot.Namespace, string(serviceInstance.ServiceSnapshot.UID), serviceInstance.ServiceSnapshot.Annotations, p.config.EgressWithNftables); err != nil {
				log.Error("[service] egress teardown", "err", err)
			}
		}
	}

	// Clean up WireGuard DNAT rules if WireGuard is enabled
	if p.config.EnableWireguard {
		log.Debug("[service] cleaning up WireGuard DNAT rules", "uid", serviceInstance.UID(), "name", serviceInstance.ServiceSnapshot.Name)
		p.deleteServiceWireguard(ctx, serviceInstance.ServiceSnapshot)
	}

	p.detachServiceInstance(serviceInstance.UID())

	log.Info("Removed instance from manager", "uid", serviceInstance.UID(), "name", serviceInstance.ServiceSnapshot.Name, "remaining advertised services", len(updatedInstances))

	return nil
}

// prepareServiceInstanceStop records shared addresses before anything can close
// the instance worker contexts. Cleanup repeats this calculation before stop,
// and Cluster merges both snapshots monotonically for the exact generation.
func (p *Processor) prepareServiceInstanceStop(serviceInstance *instance.Instance) {
	p.prepareServiceInstanceStopPreserving(serviceInstance, nil, nil)
}

func (p *Processor) prepareServiceInstanceStopPreserving(serviceInstance *instance.Instance, stopping map[*instance.Instance]struct{}, preserveAddresses map[string]struct{}) {
	if serviceInstance == nil {
		return
	}
	unlockService := p.lockService(serviceInstance.UID())
	defer unlockService()
	unlockResources := p.lockInstanceResources(serviceInstance.ServiceSnapshot, serviceInstance)
	defer unlockResources()

	vipSet := make(map[string]struct{})
	for _, sibling := range p.serviceInstances() {
		if sibling == serviceInstance {
			continue
		}
		if _, departing := stopping[sibling]; departing {
			continue
		}
		for _, address := range p.activeInstanceAddresses(sibling) {
			vipSet[address] = struct{}{}
		}
	}
	for address := range preserveAddresses {
		vipSet[address] = struct{}{}
	}
	for _, c := range serviceInstance.Clusters {
		preserve := make(map[string]struct{})
		for _, network := range c.Network {
			if _, found := vipSet[network.IP()]; found {
				preserve[network.IP()] = struct{}{}
			}
		}
		c.PrepareStopPreserving(preserve)
	}
}

func (p *Processor) prepareServiceInstancesStop(instances []*instance.Instance, leadershipLost ...bool) map[*instance.Instance]map[string]struct{} {
	lost := make(map[*instance.Instance]bool, len(instances))
	if len(leadershipLost) != 0 && leadershipLost[0] {
		for _, inst := range instances {
			lost[inst] = true
		}
	}
	return p.prepareServiceInstancesCampaignStop(instances, lost)
}

func (p *Processor) prepareServiceInstancesCampaignStop(instances []*instance.Instance, leadershipLost map[*instance.Instance]bool) map[*instance.Instance]map[string]struct{} {
	stopping := make(map[*instance.Instance]struct{}, len(instances))
	addressOwners := make(map[string][]*instance.Instance)
	preserveAddresses := make(map[string]struct{})
	for _, inst := range instances {
		if inst == nil {
			continue
		}
		stopping[inst] = struct{}{}
		for _, address := range ownedInstanceAddresses(inst) {
			addressOwners[address] = append(addressOwners[address], inst)
			preserveAddresses[address] = struct{}{}
		}
	}
	for _, inst := range instances {
		p.prepareServiceInstanceStopPreserving(inst, stopping, preserveAddresses)
	}
	forceDeletes := make(map[*instance.Instance]map[string]struct{})
	for inst := range stopping {
		forceDeletes[inst] = make(map[string]struct{})
	}
	for address, owners := range addressOwners {
		owner := campaignAddressDeleteOwner(address, owners, leadershipLost, p.config.PreserveVIPOnLeadershipLoss)
		if owner != nil {
			forceDeletes[owner][address] = struct{}{}
		}
	}
	return forceDeletes
}

func campaignAddressDeleteOwner(address string, owners []*instance.Instance, leadershipLost map[*instance.Instance]bool, preserveVIPOnLeadershipLoss bool) *instance.Instance {
	if preserveVIPOnLeadershipLoss && !utils.IsIPv6(address) {
		for _, owner := range owners {
			if leadershipLost[owner] {
				return nil
			}
		}
	}
	if len(owners) == 0 {
		return nil
	}
	return owners[0]
}

func clusterOwnsAnyAddress(c *cluster.Cluster, addresses map[string]struct{}) bool {
	for _, network := range c.Network {
		if _, ok := addresses[network.IP()]; ok {
			return true
		}
	}
	return false
}

func ownedInstanceAddresses(inst *instance.Instance) []string {
	set := make(map[string]struct{})
	for _, address := range inst.Addresses() {
		if address != "" && address != "0.0.0.0" && address != "::" {
			set[address] = struct{}{}
		}
	}
	for _, c := range inst.Clusters {
		for _, network := range c.Network {
			address := network.IP()
			if address != "" && address != "0.0.0.0" && address != "::" {
				set[address] = struct{}{}
			}
		}
	}
	addresses := make([]string, 0, len(set))
	for address := range set {
		addresses = append(addresses, address)
	}
	return addresses
}

func instanceUsesVLAN(instances []*instance.Instance, name string) bool {
	for _, inst := range instances {
		if inst.IsVLAN && inst.VLANInterface == name {
			return true
		}
	}
	return false
}

func instanceUsesDHCPInterface(instances []*instance.Instance, name string) bool {
	for _, inst := range instances {
		if (inst.IsDHCPv4 || inst.IsDHCPv6) && inst.DHCPInterface == name {
			return true
		}
	}
	return false
}

func isMissingLink(err error) bool {
	var notFound netlink.LinkNotFoundError
	return errors.As(err, &notFound)
}

func (p *Processor) deleteExpectedService(ctx context.Context, svc *v1.Service, expected *serviceExpectation) error {
	if expected == nil {
		return p.deleteService(ctx, svc.UID)
	}
	unlock := p.lockService(svc.UID)
	if !p.serviceExpectationCurrentLocked(svc, expected) {
		unlock()
		return nil
	}
	err := p.deleteCurrentService(ctx, p.findServiceInstance(svc))
	unlock()
	if err == nil {
		p.updateActiveServicesMetric()
	}
	return err
}

func (p *Processor) serviceExpectationCurrent(svc *v1.Service, expected *serviceExpectation) bool {
	if expected == nil {
		return true
	}
	unlock := p.lockService(svc.UID)
	defer unlock()
	return p.serviceExpectationCurrentLocked(svc, expected)
}

func (p *Processor) serviceExpectationCurrentLocked(svc *v1.Service, expected *serviceExpectation) bool {
	if expected == nil {
		return true
	}
	if expected.valid != nil && !expected.valid() {
		return false
	}
	if expected.lifecycle.uid != "" && svc.UID != expected.lifecycle.uid {
		return false
	}
	if expected.version != 0 && !p.desiredLifecycleCurrent(svc.UID, expected.version, expected.lifecycle) {
		return false
	}
	current, err := p.getServiceContext(svc.UID)
	if err != nil || current != expected.svcCtx || expected.svcCtx.Ctx.Err() != nil {
		return false
	}
	select {
	case <-expected.lost:
		return false
	default:
		return true
	}
}

func (p *Processor) updateEgressConfiguration(ctx context.Context, svc *v1.Service, expected ...*instance.Instance) error {
	unlockService := p.lockService(svc.UID)
	defer unlockService()

	i := p.findServiceInstance(svc)
	if i == nil {
		return fmt.Errorf("service instance not found for %s/%s", svc.Namespace, svc.Name)
	}
	if len(expected) > 0 && expected[0] != nil && i != expected[0] {
		return nil
	}

	oldIPv4 := i.ServiceSnapshot.Annotations[kubevip.ActiveEndpoint]
	newIPv4 := svc.Annotations[kubevip.ActiveEndpoint]
	oldIPv6 := i.ServiceSnapshot.Annotations[kubevip.ActiveEndpointIPv6]
	newIPv6 := svc.Annotations[kubevip.ActiveEndpointIPv6]
	oldEgressIPv6 := i.ServiceSnapshot.Annotations[kubevip.EgressIPv6] == "true"
	newEgressIPv6 := svc.Annotations[kubevip.EgressIPv6] == "true"

	// Skip update if neither endpoints nor the selected egress family changed.
	if oldIPv4 == newIPv4 && oldIPv6 == newIPv6 && oldEgressIPv6 == newEgressIPv6 {
		return nil
	}

	// The svc snapshot may have been captured before the LB IP was assigned.
	// Refresh from the API so FetchServiceAddresses sees the current ingress.
	if current, err := p.clientSet.CoreV1().Services(svc.Namespace).Get(ctx, svc.Name, metav1.GetOptions{}); err == nil {
		// Preserve the caller-supplied annotations (ActiveEndpoint etc.) that triggered this call.
		for k, v := range svc.Annotations {
			if current.Annotations == nil {
				current.Annotations = make(map[string]string)
			}
			current.Annotations[k] = v
		}
		svc = current
	}

	log.Info("[service] updating egress configuration",
		"service", svc.Name,
		"namespace", svc.Namespace,
		"old_ipv4", oldIPv4,
		"new_ipv4", newIPv4,
		"old_ipv6", oldIPv6,
		"new_ipv6", newIPv6)

	// Remove old egress rules if they exist
	if oldIPv4 != "" || oldIPv6 != "" {
		serviceIPs, _ := instance.FetchServiceAddresses(i.ServiceSnapshot)
		for _, serviceIP := range serviceIPs {
			if oldEgressIPv6 && !utils.IsIPv6(serviceIP) {
				continue
			}
			oldEndpoint := oldIPv4
			if utils.IsIPv6(serviceIP) {
				oldEndpoint = oldIPv6
			}
			if oldEndpoint == "" {
				continue
			}
			if err := egress.Teardown(
				oldEndpoint,
				serviceIP,
				i.ServiceSnapshot.Namespace,
				string(i.ServiceSnapshot.UID),
				i.ServiceSnapshot.Annotations,
				p.config.EgressWithNftables,
			); err != nil {
				log.Warn("[service] removing old egress rules", "service", svc.Name, "namespace", svc.Namespace, "err", err)
			}
		}
	}

	// Apply new egress rules with updated endpoint
	serviceIPs, _ := instance.FetchServiceAddresses(svc)
	errList := []error{}
	configuredRules := 0
	useInternalNftables := svc.Annotations[kubevip.EgressInternal] != "" || p.config.EgressWithNftables
	preparedFamilies := map[bool]bool{}

	// Check if egress should be IPv6
	if svc.Annotations[kubevip.EgressIPv6] == "true" {
		// Does the service have an active IPv6 endpoint
		if newIPv6 != "" {
			for _, serviceIP := range serviceIPs {
				if !p.config.EnableEndpoints && utils.IsIPv6(serviceIP) {
					podIP := newIPv6
					if useInternalNftables && !preparedFamilies[true] {
						if err := p.prepareEgressNftablesTable(string(svc.UID), true); err != nil {
							errList = append(errList, err)
							continue
						}
						preparedFamilies[true] = true
					}
					applied := false
					err := p.configureEgress(ctx, serviceIP, podIP, svc.Namespace, string(svc.UID), svc.Annotations, &applied)
					if err != nil {
						errList = append(errList, err)
						log.Warn("[service] configuring egress IPv6", "service", svc.Name, "namespace", svc.Namespace, "err", err)
					} else if applied {
						configuredRules++
					}
				}
			}
		}
	} else if newIPv4 != "" { // Not expected to be IPv6, so should be an IPv4 address
		for _, serviceIP := range serviceIPs {
			podIPs := newIPv4
			if !p.config.EnableEndpoints && utils.IsIPv6(serviceIP) {
				podIPs = newIPv6
			}
			ipv6 := utils.IsIPv6(serviceIP)
			if useInternalNftables && !preparedFamilies[ipv6] {
				if err := p.prepareEgressNftablesTable(string(svc.UID), ipv6); err != nil {
					errList = append(errList, err)
					continue
				}
				preparedFamilies[ipv6] = true
			}
			applied := false
			err := p.configureEgress(ctx, serviceIP, podIPs, svc.Namespace, string(svc.UID), svc.Annotations, &applied)
			if err != nil {
				errList = append(errList, err)
				log.Warn("[service] configuring egress IPv4", "service", svc.Name, "namespace", svc.Namespace, "err", err)
			} else if applied {
				configuredRules++
			}
		}
	}

	if len(errList) > 0 {
		return fmt.Errorf("errors configuring egress: %v", errList)
	}
	if configuredRules > 0 && useInternalNftables {
		if err := p.updateEgressNftablesTableAnnotation(ctx, svc); err != nil {
			return err
		}
	}

	// Update the service snapshot to reflect the new state
	// NOTE: Do NOT call UpdateServiceAnnotation here - the annotation was already updated
	// by the endpoint processor, which is why we're being called in the first place.
	// Calling it again would create an infinite loop of Modified events.
	// svc is already a DeepCopy from the endpoint processor, so no need to copy again.
	i.ServiceSnapshot = svc

	log.Info("[service] egress configuration updated successfully", "service", svc.Name, "namespace", svc.Namespace)
	return nil
}

// upnpLeaseDurationForService determines the UPNP lease duration for a given service, based on its annotations.
//
// The default lease duration is set to 1 hour, maintaining the default of 3600 seconds that was previously passed. If
// the service has an annotation of [kubevip.UpnpLeaseDuration], the function attempts to parse its value as a
// [time.Duration] using [time.ParseDuration].
//
// If parsing is successful, the lease duration is updated accordingly; otherwise, a warning is logged and the default
// duration is retained.
//
// Overriding the default lease duration can be useful for services that require longer or shorter UPNP port mappings,
// or for buggy UPNP implementations that may not handle renewals correctly. At least one router's implementation
// completely times out the mapping very shortly after creation if it is set to 3600 or 7200, but works fine if 0 is
// used.
//
// This function must therefore explicitly permit duration of 0, and callers and the underlying library must pass that
// value in XML correctly. A duration of 0 indicates to the UPNP gateway that the mapping should be permanent.
//
// It may be useful to update this function to read a global configuration option as well. This helper could also take
// in v1.Service instead of [instance.Instance], but the latter is more convenient for callers.
//
// Example where 0 was observed to stay on the problematic router: miniupnpc's test client, upnpc v2.2.4.
func upnpLeaseDurationForService(s *instance.Instance) time.Duration {
	if s == nil || s.ServiceSnapshot == nil || s.ServiceSnapshot.Annotations == nil {
		// No warning output. No annotation is unusual but perfectly ok.
		return defaultUPNPLeaseDuration
	}

	// Constant is named `UpnpLeaseDuration` for consistency with `UpnpEnabled`. According to Go naming conventions
	// regarding use of acronyms, `UPNPLeaseDuration` would be preferred. Cleanup of both of these is left for a future
	// refactor as these are public symbols and might be used elsewhere in the ecosystem.
	val, ok := s.ServiceSnapshot.Annotations[kubevip.UpnpLeaseDuration]
	if !ok {
		// No warning output. No annotation is common and perfectly ok.
		return defaultUPNPLeaseDuration
	}

	if val == "" {
		log.Warn("[UPNP] Lease duration annotation is empty, using default of 1 hour", "service", s.ServiceSnapshot.Name)
		return defaultUPNPLeaseDuration
	}

	parsed, err := time.ParseDuration(val)
	if err != nil {
		log.Warn("[UPNP] Unable to parse lease duration from annotation, using default of 1 hour", "service", s.ServiceSnapshot.Name, "err", err)
		return defaultUPNPLeaseDuration
	}

	if parsed < 0 {
		log.Warn("[UPNP] Lease duration from annotation is negative, using default of 1 hour", "service", s.ServiceSnapshot.Name)
		return defaultUPNPLeaseDuration
	}

	return parsed
}

// upnpLeaseDurationForServiceSec returns the UPNP lease duration for a service in uint32 seconds, as expected by the
// helper library. This is a convenience wrapper around [upnpLeaseDurationForService], and in case
// upnpLeaseDurationForService returns a duration that maps to a negative value of seconds or invalid float of seconds,
// it will return the default lease duration in seconds instead. (Technically, it will check for a reasonable range of
// seconds, e.g. ~10 years-ish.)
func upnpLeaseDurationForServiceSec(s *instance.Instance) uint32 {
	duration := upnpLeaseDurationForService(s)
	seconds := duration.Seconds()
	// Check if within range.
	if seconds >= 0 && seconds <= float64(10*365*24*60*60) {
		return uint32(seconds)
	}
	return uint32(defaultUPNPLeaseDuration.Seconds())
}

// Set up UPNP forwards for a service
// We first try to use the more modern Pinhole API introduced in UPNPv2 and fall back to UPNPv2 Port Forwarding if no forward was successful
func (p *Processor) upnpMap(ctx context.Context, s *instance.Instance) {
	if !isUPNPEnabled(s.ServiceSnapshot) {
		// Skip services missing the annotation
		return
	}
	if !p.config.EnableUPNP {
		log.Warn("[UPNP] Found kube-vip.io/forwardUPNP on service while UPNP forwarding is disabled in the kube-vip config. Not forwarding", "service", s.ServiceSnapshot.Name)
		return
	}
	// If upnp is enabled then update the gateway/router with the address
	// TODO - check if this implementation for dualstack is correct

	gateways := upnp.GetGatewayClients(ctx)

	// Determine desired UPNP TTL / "lease duration". Passed into the library as integer seconds from now, as the
	// underlying XML API wants integer seconds.
	leaseDurationSec := upnpLeaseDurationForServiceSec(s)

	// Reset Gateway IPs to remove stale addresses
	s.UPNPGatewayIPs = make([]string, 0)

	vips, _ := instance.FetchServiceAddresses(s.ServiceSnapshot)
	for _, vip := range vips {
		for _, port := range s.ServiceSnapshot.Spec.Ports {
			for _, gw := range gateways {

				forwardSucessful := false
				if gw.WANIPv6FirewallControlClient != nil {
					log.Info("[UPNP] Adding map", "vip", vip, "port", port.Port, "service", s.ServiceSnapshot.Name, "gateway", gw.WANIPv6FirewallControlClient.Location, "leaseDurationSec", leaseDurationSec)

					pinholeID, pinholeErr := gw.WANIPv6FirewallControlClient.AddPinholeCtx(ctx, "0.0.0.0", uint16(port.Port), vip, uint16(port.Port), upnp.MapProtocolToIANA(string(port.Protocol)), leaseDurationSec) //nolint  TODO
					if pinholeErr == nil {
						forwardSucessful = true
						log.Info("[UPNP] Service should be accessible externally", "port", port.Port, "pinhold ID", pinholeID)
					} else {
						//TODO: Cleanup
						log.Error("[UPNP] Unable to map port to gateway using Pinhole API", "err", pinholeErr.Error())
					}
				}
				// Fallback to PortForward
				if !forwardSucessful {
					log.Info("[UPNP] Adding map", "vip", vip, "port", port.Port, "service", s.ServiceSnapshot.Name, "leaseDurationSec", leaseDurationSec)

					portMappingErr := gw.ConnectionClient.AddPortMapping("0.0.0.0", uint16(port.Port), strings.ToUpper(string(port.Protocol)), uint16(port.Port), vip, true, s.ServiceSnapshot.Name, leaseDurationSec) //nolint  TODO
					if portMappingErr == nil {
						ip, err := gw.ConnectionClient.GetExternalIPAddress()
						if err != nil {
							// Log the error but continue on the off chance the mapping was successful
							log.Error("[UPNP] Unable to get external IP address from gateway", "service", s.ServiceSnapshot.Name, "port", port.Port, "err", err)
						} else {
							log.Info("[UPNP] Service should be accessible externally", "service", s.ServiceSnapshot.Name, "port", port.Port, "externalip", ip)
						}
						forwardSucessful = true
					} else {
						//TODO: Cleanup
						log.Error("[UPNP] Unable to map port to gateway using PortForward API", "err", portMappingErr.Error())
					}
				}

				if forwardSucessful {
					ip, err := gw.ConnectionClient.GetExternalIPAddress()
					if err == nil {
						s.UPNPGatewayIPs = append(s.UPNPGatewayIPs, ip)
					}
				}
			}
		}
	}

	// Remove duplicate IPs
	slices.Sort(s.UPNPGatewayIPs)
	s.UPNPGatewayIPs = slices.Compact(s.UPNPGatewayIPs)
}

func (p *Processor) updateStatus(ctx context.Context, i *instance.Instance) error {
	// let's retry status update every 10ms for 30s
	retryConfig := wait.Backoff{
		Steps:    3000,
		Duration: 10 * time.Millisecond,
		Factor:   0,
		Jitter:   0.1,
	}
	// will retry for every error encountered, TODO: should a list of errors that will trigger retry be specified?
	err := retry.OnError(retryConfig, func(err error) bool {
		return !errors.Is(err, context.Canceled)
	}, func() error {
		// Retrieve the latest version of Deployment before attempting update
		// RetryOnConflict uses exponential backoff to avoid exhausting the apiserver
		currentService, err := p.clientSet.CoreV1().Services(i.ServiceSnapshot.Namespace).Get(ctx, i.ServiceSnapshot.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}

		currentServiceCopy := currentService.DeepCopy()
		if currentServiceCopy.Annotations == nil {
			currentServiceCopy.Annotations = make(map[string]string)
		}

		// If we're using ARP then we can only broadcast the VIP from one place, also useful for other software when running BGP, add an annotation to the service
		if p.config.EnableARP || p.config.EnableBGP {
			// Add the current host
			currentServiceCopy.Annotations[kubevip.VipHost] = p.config.NodeName
		}
		if i.DHCPInterfaceHwaddr != "" || i.DHCPInterfaceIPv4 != "" || i.DHCPInterfaceIPv6 != "" {
			currentServiceCopy.Annotations[kubevip.HwAddrKey] = i.DHCPInterfaceHwaddr
			dhcpInterfaceIP := ""
			if i.DHCPInterfaceIPv4 != "" {
				dhcpInterfaceIP = i.DHCPInterfaceIPv4
				if i.DHCPInterfaceIPv6 != "" {
					dhcpInterfaceIP += ","
				}
			}
			if i.DHCPInterfaceIPv6 != "" {
				dhcpInterfaceIP += i.DHCPInterfaceIPv6
			}
			currentServiceCopy.Annotations[kubevip.RequestedIP] = dhcpInterfaceIP
		}

		if currentService.Annotations["development.kube-vip.io/synthetic-api-server-error-on-update"] == "true" {
			log.Error("(Synthetic error ) updating Spec", "service", i.ServiceSnapshot.Name, "err", err)
			return fmt.Errorf("(Synthetic) simulating api server errors")
		}

		if !cmp.Equal(currentService, currentServiceCopy) {
			currentService, err = p.clientSet.CoreV1().Services(currentServiceCopy.Namespace).Update(ctx, currentServiceCopy, metav1.UpdateOptions{})
			if err != nil {
				log.Error("updating Spec", "service", i.ServiceSnapshot.Name, "err", err)
				return err
			}
		}

		ports := make([]v1.PortStatus, 0, len(i.ServiceSnapshot.Spec.Ports))
		for _, port := range i.ServiceSnapshot.Spec.Ports {
			ports = append(ports, v1.PortStatus{
				Port:     port.Port,
				Protocol: port.Protocol,
			})
		}

		ingresses := []v1.LoadBalancerIngress{}

		for _, c := range i.VIPConfigs {
			if !utils.IsIP(c.VIP) {
				ips, err := utils.LookupHost(c.VIP, c.DNSMode, *i.ServiceSnapshot.Spec.IPFamilyPolicy == v1.IPFamilyPolicyRequireDualStack)
				if err != nil {
					return err
				}
				for _, ip := range ips {
					i := v1.LoadBalancerIngress{
						IP:    ip,
						Ports: ports,
					}
					ingresses = append(ingresses, i)
				}
			} else {
				i := v1.LoadBalancerIngress{
					IP:    c.VIP,
					Ports: ports,
				}
				ingresses = append(ingresses, i)
			}
			if isUPNPEnabled(currentService) {
				for _, ip := range i.UPNPGatewayIPs {
					i := v1.LoadBalancerIngress{
						IP:    ip,
						Ports: ports,
					}
					ingresses = append(ingresses, i)
				}
			}
		}
		log.Debug("LB status", "current", currentService.Status.LoadBalancer.Ingress, "new", ingresses)
		if !ingressEqual(currentService.Status.LoadBalancer.Ingress, ingresses) {
			currentService.Status.LoadBalancer.Ingress = ingresses
			log.Debug("updating service status", "namespace", currentService.Namespace, "name", currentService.Name, "uid", currentService.UID)
			_, err = p.clientSet.CoreV1().Services(currentService.Namespace).UpdateStatus(ctx, currentService, metav1.UpdateOptions{})
			if err != nil && !apierrors.IsInvalid(err) {
				log.Error("updating Service", "namespace", i.ServiceSnapshot.Namespace, "name", i.ServiceSnapshot.Name, "err", err)
				return err
			}
		}
		return nil
	})

	return err
}

func ingressEqual(a, b []v1.LoadBalancerIngress) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range len(a) {
		if a[i].IP != b[i].IP || a[i].Hostname != b[i].Hostname ||
			!cmp.Equal(a[i].Ports, b[i].Ports) {
			return false
		}
	}
	return true
}

func isUPNPEnabled(s *v1.Service) bool {
	return metav1.HasAnnotation(s.ObjectMeta, kubevip.UpnpEnabled) && s.Annotations[kubevip.UpnpEnabled] == "true"
}

// Refresh UPNP Port Forwards for all Service Instances registered in the processor
func (p *Processor) RefreshUPNPForwards(ctx context.Context) {
	log.Info("Starting UPNP Port Refresher")

	ticker := time.NewTicker(300 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			instances := p.serviceInstances()
			// Skip logging if no service instances
			if len(instances) == 0 {
				continue
			}

			log.Info("[UPNP] Refreshing Instances", "number of instances", len(instances))
			for _, serviceInstance := range instances {
				uid := serviceInstance.UID()
				unlockService := p.lockService(uid)
				service := &v1.Service{ObjectMeta: metav1.ObjectMeta{UID: uid}}
				current := p.findServiceInstance(service)
				if current != serviceInstance {
					unlockService()
					continue
				}
				p.upnpMap(ctx, serviceInstance)
				if err := p.updateStatus(ctx, serviceInstance); err != nil {
					log.Warn("[UPNP] Error updating service", "ip", serviceInstance.ServiceSnapshot.Name, "err", err)
				}
				unlockService()
			}
		}
	}
}

// GenerateLabelFromService generates a label key and value for the given service
func generateLabelsFromService(svc *v1.Service, labelKey string) map[string]string {
	addresses, _ := instance.FetchServiceAddresses(svc)

	sanitized := make([]string, len(addresses))
	for i, addr := range addresses {
		sanitized[i] = utils.SanitizeIPForLabel(addr)
	}

	return map[string]string{
		fmt.Sprintf("%s/%s.%s", labelKey, svc.Name, svc.Namespace): strings.Join(sanitized, ","),
	}
}
