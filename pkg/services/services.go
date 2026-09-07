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
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"

	"github.com/kube-vip/kube-vip/pkg/egress"
	"github.com/kube-vip/kube-vip/pkg/endpoints"
	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	"github.com/kube-vip/kube-vip/pkg/upnp"
	"github.com/kube-vip/kube-vip/pkg/utils"
	"github.com/kube-vip/kube-vip/pkg/vip"
)

type ServiceInstanceAction string

const (
	ActionDelete ServiceInstanceAction = "delete"
	ActionAdd    ServiceInstanceAction = "add"
	ActionNone   ServiceInstanceAction = "none"

	// Default UPNP lease requested is 3600 seconds.
	defaultUPNPLeaseDuration = 1 * time.Hour
)

func (p *Processor) SyncServices(ctx *servicecontext.Context, svc *v1.Service, wg *sync.WaitGroup, usesLeaderElection bool) error {
	return p.syncServicesWithContext(ctx.Ctx, ctx, svc, wg, usesLeaderElection)
}

func (p *Processor) syncServicesWithContext(operationCtx context.Context, svcCtx *servicecontext.Context,
	svc *v1.Service, wg *sync.WaitGroup, usesLeaderElection bool) error {
	log.Debug("[STARTING] Service Sync", "namespace", svc.Namespace, "name", svc.Name, "uid", svc.UID)

	// Iterate through the synchronising services
	action := p.getServiceInstanceAction(svc)
	switch action {
	case ActionDelete:
		log.Debug("[service] delete", "namespace", svc.Namespace, "name", svc.Name, "uid", svc.UID)
		if err := p.deleteService(operationCtx, svc.UID); err != nil {
			return fmt.Errorf("error deleting service %s/%s: %w", svc.Namespace, svc.Name, err)
		}
	case ActionAdd:
		log.Debug("[service] add", "namespace", svc.Namespace, "name", svc.Name, "uid", svc.UID)
		if !usesLeaderElection {
			releaseReadiness, ready := svcCtx.WaitForReadiness()
			if !ready {
				return nil
			}
			defer releaseReadiness()
		}

		if err := p.addService(operationCtx, svc, wg); err != nil {
			return fmt.Errorf("error adding service %s/%s: %w", svc.Namespace, svc.Name, err)
		}

	case ActionNone:
		log.Debug("[service] no action", "namespace", svc.Namespace, "name", svc.Name, "uid", svc.UID)
		// Egress: when the service reaches ActionNone it means AddCalled is already true.
		// If ActiveEndpoint is now set (by the endpoint watcher) and the service has a
		// LB IP, the initial addService call may have missed the SNAT configuration because
		// ActiveEndpoint was not yet present. Re-run it here.
		if svc.Annotations[kubevip.Egress] == "true" && svc.Annotations[kubevip.ActiveEndpoint] != "" {
			if err := p.updateEgressConfiguration(operationCtx, svc); err != nil {
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

func (p *Processor) addService(ctx context.Context, svc *v1.Service, wg *sync.WaitGroup) error {
	startTime := time.Now()

	inst, err := p.prepareServiceInstance(ctx, svc, wg)
	if err != nil {
		return err
	}
	if inst == nil {
		return nil
	}

	if err := p.configureService(ctx, inst, svc, wg); err != nil {
		cleanupErr := p.deleteServiceInstance(context.WithoutCancel(ctx), inst)
		if cleanupErr != nil {
			return fmt.Errorf("configure service %s/%s: %w; cleanup: %w", svc.Namespace, svc.Name, err, cleanupErr)
		}
		return err
	}

	finishTime := time.Since(startTime)
	log.Info("[service]", "service", svc.Name, "namespace", svc.Namespace, "synchronised in", fmt.Sprintf("%dms", finishTime.Milliseconds()))

	return nil
}

// prepareServiceInstance finds or constructs the instance and marks it added. It
// acquires the Service lock for svc.UID; callers must not already hold it.
func (p *Processor) prepareServiceInstance(ctx context.Context, svc *v1.Service, wg *sync.WaitGroup) (*instance.Instance, error) {
	unlockService := p.lockService(svc.UID)
	defer unlockService()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	current := p.findServiceInstance(svc)
	if current != nil {
		if current.AddCalled {
			return nil, nil
		}
		current.AddCalled = true
		return current, nil
	}

	inst, err := p.createServiceInstance(ctx, svc, wg)
	if err != nil {
		return nil, err
	}
	inst.AddCalled = true
	p.appendServiceInstance(inst)

	return inst, nil
}

// configureService configures a tracked instance. It acquires the Service lock
// for svc.UID and verifies inst is still current; callers must not hold the lock.
func (p *Processor) configureService(ctx context.Context, inst *instance.Instance, svc *v1.Service, wg *sync.WaitGroup) error {
	unlockService := p.lockService(svc.UID)
	defer unlockService()
	if err := ctx.Err(); err != nil {
		return err
	}
	current := p.findServiceInstance(svc)
	if current != inst {
		return fmt.Errorf("service instance no longer active for %s/%s", svc.Namespace, svc.Name)
	}

	// is not a global leader election mode
	if p.config.EnableServicesElection || (!p.config.EnableARP && !p.config.EnableLeaderElection) || (!p.config.EnableARP && !p.config.EnableRoutingTable) {
		if err := endpoints.StartService(ctx, svc, inst, p.bgpServer, wg); err != nil {
			return fmt.Errorf("start service datapath: %w", err)
		}
	}

	p.upnpMap(ctx, inst)

	if inst.IsDHCPv4 {
		index := dhcpConfigIndex(inst.VIPConfigs, false)
		if index == -1 {
			log.Error("unable to find proper VIPConfig for the DHCPv4")
		} else {
			wg.Go(func() {
				for ip := range inst.DHCPv4Client.IPChannel() {
					if !p.updateDHCPAddress(ctx, svc, inst, index, ip, false) {
						return
					}
				}
				log.Debug("IPv4 update channel closed, stopping")
			})
		}
	}

	if inst.IsDHCPv6 {
		index := dhcpConfigIndex(inst.VIPConfigs, true)
		if index == -1 {
			log.Error("unable to find proper VIPConfig for the DHCPv6")
		} else {
			wg.Go(func() {
				for ip := range inst.DHCPv6Client.IPChannel() {
					if !p.updateDHCPAddress(ctx, svc, inst, index, ip, true) {
						return
					}
				}
				log.Debug("IPv6 update channel closed, stopping")
			})
		}
	}

	if !p.config.DisableServiceUpdates {
		log.Debug("[service] update", "namespace", inst.ServiceSnapshot.Namespace, "name", inst.ServiceSnapshot.Name)
		if err := p.updateStatus(ctx, inst); err != nil {
			log.Error("[service] updating status", "namespace", inst.ServiceSnapshot.Namespace, "name", inst.ServiceSnapshot.Name, "err", err)
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
	}

	labels := generateLabelsFromService(svc, kubevip.ServiceProvided)
	if err := p.nodeLabelManager.AddLabel(labels); err != nil {
		return fmt.Errorf("error adding label to node: %w", err)
	}
	inst.LabelAdded = true

	return nil
}

// dhcpConfigIndex reads VIP configuration state. The caller must hold the
// Service lock when configs belong to a tracked instance.
func dhcpConfigIndex(configs []*kubevip.Config, ipv6 bool) int {
	for index, config := range configs {
		ip := net.ParseIP(config.VIP)
		if ip != nil && (ip.To4() == nil) == ipv6 {
			return index
		}
	}
	return -1
}

// updateDHCPAddress applies one lease update. It acquires the Service lock for
// svc.UID and returns false if inst is no longer current; callers must not
// already hold the lock.
func (p *Processor) updateDHCPAddress(ctx context.Context, svc *v1.Service, inst *instance.Instance, index int, ip string, ipv6 bool) bool {
	unlockService := p.lockService(svc.UID)
	defer unlockService()

	if p.findServiceInstance(svc) != inst {
		return false
	}

	log.Debug("IP changed", "ip", ip)
	inst.VIPConfigs[index].VIP = ip
	if ipv6 {
		inst.DHCPInterfaceIPv6 = ip
	} else {
		inst.DHCPInterfaceIPv4 = ip
	}
	if !p.config.DisableServiceUpdates {
		if err := p.updateStatus(ctx, inst); err != nil {
			log.Warn("updating svc", "err", err)
		}
	}
	return true
}

// serviceSnapshotForEgress reads the tracked instance snapshot. The caller must
// hold the Service lock for inst.UID().
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

// deleteService removes the tracked instance for uid. It acquires the Service
// lock; callers must not already hold it.
func (p *Processor) deleteService(ctx context.Context, uid types.UID, expectedCtx ...*servicecontext.Context) error {
	unlockService := p.lockService(uid)
	defer unlockService()
	var expected *servicecontext.Context
	if len(expectedCtx) > 0 {
		expected = expectedCtx[0]
	}
	if expected != nil {
		currentCtx, err := p.getServiceContext(uid)
		if err != nil {
			return err
		}
		if currentCtx != nil && currentCtx != expected {
			return nil
		}
	}

	return p.deleteCurrentServiceByUID(ctx, uid)
}

// deleteCurrentServiceByUID removes a tracked instance. The caller must hold the
// Service lock for uid. A missing instance means cleanup already completed, so
// deletion is idempotent.
func (p *Processor) deleteCurrentServiceByUID(ctx context.Context, uid types.UID) error {
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{UID: uid}}
	serviceInstance := p.findServiceInstance(service)
	if serviceInstance == nil {
		log.Debug("service instance already absent", "uid", uid)
		return nil
	}
	return p.deleteCurrentService(ctx, serviceInstance)
}

// deleteServiceInstance removes expected only if it is still current. It
// acquires the Service lock for expected.UID; callers must not already hold it.
func (p *Processor) deleteServiceInstance(ctx context.Context, expected *instance.Instance) error {
	if expected == nil {
		return nil
	}

	unlockService := p.lockService(expected.UID())
	defer unlockService()
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{UID: expected.UID()}}
	if p.findServiceInstance(service) != expected {
		return nil
	}
	return p.deleteCurrentService(ctx, expected)
}

// deleteCurrentService removes the supplied tracked instance. The caller must
// hold the Service lock for serviceInstance.UID().
func (p *Processor) deleteCurrentService(ctx context.Context, serviceInstance *instance.Instance) error {
	if serviceInstance.LabelAdded {
		labels := generateLabelsFromService(serviceInstance.ServiceSnapshot, kubevip.ServiceProvided)
		if err := p.nodeLabelManager.RemoveLabel(labels); err != nil {
			return fmt.Errorf("error removing label from node: %w", err)
		}
	}

	p.serviceCleanupMu.Lock()
	defer p.serviceCleanupMu.Unlock()
	removed, updatedInstances := p.detachServiceInstance(serviceInstance.UID())
	if removed != serviceInstance {
		return nil
	}
	if err := endpoints.CleanupService(ctx, p.config, p.bgpServer, p.routeMgr, p.TunnelMgr, serviceInstance, updatedInstances); err != nil {
		p.appendServiceInstance(serviceInstance)
		return fmt.Errorf("cleanup service datapath: %w", err)
	}

	log.Info("Removed instance from manager", "uid", serviceInstance.UID(), "name", serviceInstance.ServiceSnapshot.Name, "remaining advertised services", len(updatedInstances))

	return nil
}

// updateEgressConfiguration updates egress state for the current instance. It
// acquires the Service lock for svc.UID; callers must not already hold it.
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
		if current.UID != i.UID() {
			return nil
		}
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
				string(i.UID()),
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
// The caller must hold the Service lock when s is a tracked instance.
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
// The caller must hold the Service lock when s is a tracked instance.
func upnpLeaseDurationForServiceSec(s *instance.Instance) uint32 {
	duration := upnpLeaseDurationForService(s)
	seconds := duration.Seconds()
	// Check if within range.
	if seconds >= 0 && seconds <= float64(10*365*24*60*60) {
		return uint32(seconds)
	}
	return uint32(defaultUPNPLeaseDuration.Seconds())
}

// upnpMap sets up UPNP forwards for a service. The caller must hold the Service
// lock for s.UID(). It first tries the Pinhole API introduced in UPNPv2 and falls
// back to UPNPv2 port forwarding if no forward was successful.
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

// updateStatus reads and updates tracked instance state. The caller must hold the
// Service lock for i.UID().
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
				p.refreshUPNPForward(ctx, serviceInstance)
			}
		}
	}
}

// refreshUPNPForward refreshes one instance if it is still current. It acquires
// the Service lock for serviceInstance.UID; callers must not already hold it.
func (p *Processor) refreshUPNPForward(ctx context.Context, serviceInstance *instance.Instance) {
	uid := serviceInstance.UID()
	unlockService := p.lockService(uid)
	defer unlockService()

	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{UID: uid}}
	if p.findServiceInstance(service) != serviceInstance {
		return
	}
	p.upnpMap(ctx, serviceInstance)
	if err := p.updateStatus(ctx, serviceInstance); err != nil {
		log.Warn("[UPNP] Error updating service", "ip", serviceInstance.ServiceSnapshot.Name, "err", err)
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
