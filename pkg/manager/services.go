package manager

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	log "log/slog"

	"github.com/google/go-cmp/cmp"
	"github.com/vishvananda/netlink"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"

	"github.com/kube-vip/kube-vip/pkg/cluster"
	"github.com/kube-vip/kube-vip/pkg/upnp"
	"github.com/kube-vip/kube-vip/pkg/vip"
)

type ServiceInstanceAction string

const (
	ActionDelete ServiceInstanceAction = "delete"
	ActionAdd    ServiceInstanceAction = "add"
	ActionNone   ServiceInstanceAction = "none"
)

func (sm *Manager) syncServices(ctx context.Context, svc *v1.Service) error {
	log.Debug("[STARTING] Service Sync", "namespace", svc.Namespace, "name", svc.Name)

	// Iterate through the synchronising services

	action := sm.getServiceInstanceAction(svc)
	switch action {
	case ActionDelete:
		log.Debug("[service] delete", "namespace", svc.Namespace, "name", svc.Name)
		if err := sm.deleteService(svc.UID); err != nil {
			return fmt.Errorf("error deleting service %s/%s: %w", svc.Namespace, svc.Name, err)
		}
	case ActionAdd:
		log.Debug("[service] add", "namespace", svc.Namespace, "name", svc.Name)
		if err := sm.addService(ctx, svc); err != nil {
			return fmt.Errorf("error adding service %s/%s: %w", svc.Namespace, svc.Name, err)
		}
	case ActionNone:
		log.Debug("[service] no action", "namespace", svc.Namespace, "name", svc.Name)
	}
	log.Debug("[FINISHED] Service Sync", "namespace", svc.Namespace, "name", svc.Name)
	return nil
}

func (sm *Manager) getServiceInstanceAction(svc *v1.Service) ServiceInstanceAction {
	// protect against multiple calls
	addresses := cluster.FetchServiceAddresses(svc)
	ingressIPs := cluster.FetchLoadBalancerIngressAddresses(svc)
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	for _, instance := range sm.serviceInstances {
		if instance != nil && instance.ServiceSnapshot.UID == svc.UID {
			for _, address := range addresses {
				// handle the case where the service instance needs to be deleted
				if instance.IsDHCP {
					if address != "0.0.0.0" {
						return ActionDelete
					}
					if len(svc.Status.LoadBalancer.Ingress) > 0 && !slices.Contains(ingressIPs, instance.DHCPInterfaceIP) {
						return ActionDelete
					}
				}
				if !instance.IsDHCP {
					if address == "0.0.0.0" {
						return ActionDelete
					}
					if len(svc.Status.LoadBalancer.Ingress) > 0 && !slices.Contains(ingressIPs, address) {
						return ActionDelete
					}
				}
				if !comparePortsAndPortStatuses(svc) {
					return ActionDelete
				}
			}
			// If we reach here, it means the service instance matches the service UID and is not a DHCP service, so we can return "no action"
			return ActionNone
		}
	}
	if len(addresses) > 0 {
		log.Debug("No matching service instance found", "service", svc.Name, "namespace", svc.Namespace, "addresses", addresses)
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

func (sm *Manager) addService(ctx context.Context, svc *v1.Service) error {
	// protect against addService whil reading
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	startTime := time.Now()

	newService, err := cluster.NewInstance(svc, sm.config, sm.intfMgr, sm.arpMgr)
	if err != nil {
		return err
	}

	for x := range newService.VIPConfigs {
		log.Debug("starting loadbalancer for service", "name", svc.Name, "namespace", svc.Namespace)
		newService.Clusters[x].StartLoadBalancerService(newService.VIPConfigs[x], sm.bgpServer, svc.Name, &sm.serviceInstances)
	}

	sm.upnpMap(ctx, newService)

	if newService.IsDHCP && len(newService.VIPConfigs) == 1 {
		go func() {
			for ip := range newService.DHCPClient.IPChannel() {
				log.Debug("IP changed", "ip", ip)
				newService.VIPConfigs[0].VIP = ip
				newService.DHCPInterfaceIP = ip
				if !sm.config.DisableServiceUpdates {
					if err := sm.updateStatus(newService); err != nil {
						log.Warn("updating svc", "err", err)
					}
				}
			}
			log.Debug("IP update channel closed, stopping")
		}()
	}

	sm.serviceInstances = append(sm.serviceInstances, newService)

	if !sm.config.DisableServiceUpdates {
		log.Debug("[service] update", "namespace", newService.ServiceSnapshot.Namespace, "name", newService.ServiceSnapshot.Name)
		if err := sm.updateStatus(newService); err != nil {
			log.Error("[service] updating status", "namespace", newService.ServiceSnapshot.Namespace, "name", newService.ServiceSnapshot.Name, "err", err)
		}
	}

	serviceIPs := cluster.FetchServiceAddresses(svc)
	// Check if we need to flush any conntrack connections (due to some dangling conntrack connections)
	if svc.Annotations[flushContrack] == "true" {

		log.Debug("[service] Flushing conntrack rules", "service", svc.Name, "namespace", svc.Namespace)
		for _, serviceIP := range serviceIPs {
			err = vip.DeleteExistingSessions(serviceIP, false, svc.Annotations[egressDestinationPorts], svc.Annotations[egressSourcePorts])
			if err != nil {
				log.Error("[service] flushing any remaining egress connections", "service", svc.Name, "namespace", svc.Namespace, "err", err)
			}
			err = vip.DeleteExistingSessions(serviceIP, true, svc.Annotations[egressDestinationPorts], svc.Annotations[egressSourcePorts])
			if err != nil {
				log.Error("[service] flushing any remaining ingress connections", "service", svc.Name, "namespace", svc.Namespace, "err", err)
			}
		}
	}

	// Check if egress is enabled on the service, if so we'll need to configure some rules
	if svc.Annotations[egress] == "true" && len(serviceIPs) > 0 {
		log.Debug("[service] enabling egress", "service", svc.Name, "namespace", svc.Namespace)
		// We will need to modify the iptables rules
		err = sm.iptablesCheck()
		if err != nil {
			log.Error("[service] configuring egress", "service", svc.Name, "namespace", svc.Namespace, "err", err)
		}
		var podIP string
		errList := []error{}

		// Should egress be IPv6
		if svc.Annotations[egressIPv6] == "true" {
			// Does the service have an active IPv6 endpoint
			if svc.Annotations[activeEndpointIPv6] != "" {
				for _, serviceIP := range serviceIPs {
					if sm.config.EnableEndpointSlices && vip.IsIPv6(serviceIP) {

						podIP = svc.Annotations[activeEndpointIPv6]

						err = sm.configureEgress(serviceIP, podIP, svc.Namespace, svc.Annotations)
						if err != nil {
							errList = append(errList, err)
							log.Error("[service] configuring egress IPv6", "service", svc.Name, "namespace", svc.Namespace, "err", err)
						}
					}
				}
			}
		} else if svc.Annotations[activeEndpoint] != "" { // Not expected to be IPv6, so should be an IPv4 address
			for _, serviceIP := range serviceIPs {
				podIPs := svc.Annotations[activeEndpoint]
				if sm.config.EnableEndpointSlices && vip.IsIPv6(serviceIP) {
					podIPs = svc.Annotations[activeEndpointIPv6]
				}
				err = sm.configureEgress(serviceIP, podIPs, svc.Namespace, svc.Annotations)
				if err != nil {
					errList = append(errList, err)
					log.Error("[service] configuring egress IPv4", "service", svc.Name, "namespace", svc.Namespace, "err", err)
				}
			}
		}
		if len(errList) == 0 {
			var provider epProvider
			if !sm.config.EnableEndpointSlices {
				provider = &endpointsProvider{label: "endpoints"}
			} else {
				provider = &endpointslicesProvider{label: "endpointslices"}
			}
			err = provider.updateServiceAnnotation(svc.Annotations[activeEndpoint], svc.Annotations[activeEndpointIPv6], svc, sm)
			if err != nil {
				log.Error("[service] configuring egress", "service", svc.Name, "namespace", svc.Namespace, "err", err)
			}
		}
	}

	finishTime := time.Since(startTime)
	log.Info("[service]", "service", svc.Name, "namespace", svc.Namespace, "synchronised in", fmt.Sprintf("%dms", finishTime.Milliseconds()))

	return nil
}

func (sm *Manager) deleteService(uid types.UID) error {
	// protect multiple calls
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	var updatedInstances []*cluster.Instance
	var serviceInstance *cluster.Instance
	found := false
	for x := range sm.serviceInstances {
		log.Debug("service lookup", "target UID", uid, "found UID ", sm.serviceInstances[x].ServiceSnapshot.UID, "name", sm.serviceInstances[x].ServiceSnapshot.Name, "namespace", sm.serviceInstances[x].ServiceSnapshot.Namespace)
		// Add the running services to the new array
		if sm.serviceInstances[x].ServiceSnapshot.UID != uid {
			updatedInstances = append(updatedInstances, sm.serviceInstances[x])
		} else {
			// Flip the found when we match
			found = true
			serviceInstance = sm.serviceInstances[x]
		}
	}
	// If we've been through all services and not found the correct one then error
	if !found {
		// TODO: - fix UX
		// return fmt.Errorf("unable to find/stop service [%s]", uid)
		return nil
	}

	for _, c := range serviceInstance.Clusters {
		for n := range c.Network {
			c.Network[n].SetHasEndpoints(false)
		}
	}

	// Determine if this this VIP is shared with other loadbalancers
	shared := false
	vipSet := make(map[string]interface{})
	for x := range updatedInstances {
		for _, vip := range cluster.FetchServiceAddresses(updatedInstances[x].ServiceSnapshot) { //updatedInstances[x].ServiceSnapshot.Spec.LoadBalancerIP {
			vipSet[vip] = nil
		}
	}
	for _, vip := range cluster.FetchServiceAddresses(serviceInstance.ServiceSnapshot) {
		if _, found := vipSet[vip]; found {
			shared = true
		}
	}
	for x := range serviceInstance.Clusters {
		serviceInstance.Clusters[x].Stop()
	}
	if !shared {
		if serviceInstance.IsDHCP {
			serviceInstance.DHCPClient.Stop()
			macvlan, err := netlink.LinkByName(serviceInstance.DHCPInterface)
			if err != nil {
				return fmt.Errorf("error finding VIP Interface: %v", err)
			}

			err = netlink.LinkDel(macvlan)
			if err != nil {
				return fmt.Errorf("error deleting DHCP Link : %v", err)
			}
		}
		for i := range serviceInstance.VIPConfigs {
			if serviceInstance.VIPConfigs[i].EnableBGP {
				sm.clearBGPHosts(serviceInstance.ServiceSnapshot)
			}
		}

		// We will need to tear down the egress
		if serviceInstance.ServiceSnapshot.Annotations[egress] == "true" {
			if serviceInstance.ServiceSnapshot.Annotations[activeEndpoint] != "" {
				log.Info("egress re-write enabled", "service", serviceInstance.ServiceSnapshot.Name)
				err := sm.TeardownEgress(serviceInstance.ServiceSnapshot.Annotations[activeEndpoint], serviceInstance.ServiceSnapshot.Spec.LoadBalancerIP, serviceInstance.ServiceSnapshot.Namespace, serviceInstance.ServiceSnapshot.Annotations)
				if err != nil {
					log.Error("egress teardown", "err", err)
				}
			}
		}
	}

	// Update the service array
	sm.serviceInstances = updatedInstances

	log.Info("Removed instance from manager", "uid", uid, "remaining advertised services", len(sm.serviceInstances))

	return nil
}

// Set up UPNP forwards for a service
// We first try to use the more modern Pinhole API introduced in UPNPv2 and fall back to UPNPv2 Port Forwarding if no forward was successful
func (sm *Manager) upnpMap(ctx context.Context, s *cluster.Instance) {
	if !isUPNPEnabled(s.ServiceSnapshot) {
		// Skip services missing the annotation
		return
	}
	if !sm.upnp {
		log.Warn("[UPNP] Found kube-vip.io/forwardUPNP on service while UPNP forwarding is disabled in the kube-vip config. Not forwarding", "service", s.ServiceSnapshot.Name)
	}
	// If upnp is enabled then update the gateway/router with the address
	// TODO - check if this implementation for dualstack is correct

	gateways := upnp.GetGatewayClients(ctx)

	// Reset Gateway IPs to remove stale addresses
	s.UPNPGatewayIPs = make([]string, 0)

	for _, vip := range cluster.FetchServiceAddresses(s.ServiceSnapshot) {
		for _, port := range s.ServiceSnapshot.Spec.Ports {
			for _, gw := range gateways {
				log.Info("[UPNP] Adding map", "vip", vip, "port", port.Port, "service", s.ServiceSnapshot.Name, "gateway", gw.ConnectionClient.GetServiceClient().Location)

				forwardSucessful := false
				if gw.WANIPv6FirewallControlClient != nil {
					pinholeID, pinholeErr := gw.WANIPv6FirewallControlClient.AddPinholeCtx(ctx, "0.0.0.0", uint16(port.Port), vip, uint16(port.Port), upnp.MapProtocolToIANA(string(port.Protocol)), 3600) //nolint  TODO
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
					portMappingErr := gw.ConnectionClient.AddPortMapping("0.0.0.0", uint16(port.Port), strings.ToUpper(string(port.Protocol)), uint16(port.Port), vip, true, s.ServiceSnapshot.Name, 3600) //nolint  TODO
					if portMappingErr == nil {
						log.Info("[UPNP] Service should be accessible externally", "port", port.Port)
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

func (sm *Manager) updateStatus(i *cluster.Instance) error {
	// let's retry status update every 10ms for 30s
	retryConfig := wait.Backoff{
		Steps:    3000,
		Duration: 10 * time.Millisecond,
		Factor:   0,
		Jitter:   0.1,
	}
	// will retry for every error encountered, TODO: should a list of errors that will trigger retry be specified?
	err := retry.OnError(retryConfig, func(error) bool { return true }, func() error {
		// Retrieve the latest version of Deployment before attempting update
		// RetryOnConflict uses exponential backoff to avoid exhausting the apiserver
		currentService, err := sm.clientSet.CoreV1().Services(i.ServiceSnapshot.Namespace).Get(context.TODO(), i.ServiceSnapshot.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}

		currentServiceCopy := currentService.DeepCopy()
		if currentServiceCopy.Annotations == nil {
			currentServiceCopy.Annotations = make(map[string]string)
		}

		// If we're using ARP then we can only broadcast the VIP from one place, add an annotation to the service
		if sm.config.EnableARP {
			// Add the current host
			currentServiceCopy.Annotations[vipHost] = sm.config.NodeName
		}
		if i.DHCPInterfaceHwaddr != "" || i.DHCPInterfaceIP != "" {
			currentServiceCopy.Annotations[cluster.HWAddrKey] = i.DHCPInterfaceHwaddr
			currentServiceCopy.Annotations[cluster.RequestedIP] = i.DHCPInterfaceIP
		}

		if currentService.Annotations["development.kube-vip.io/synthetic-api-server-error-on-update"] == "true" {
			log.Error("(Synthetic error ) updating Spec", "service", i.ServiceSnapshot.Name, "err", err)
			return fmt.Errorf("(Synthetic) simulating api server errors")
		}

		if !cmp.Equal(currentService, currentServiceCopy) {
			currentService, err = sm.clientSet.CoreV1().Services(currentServiceCopy.Namespace).Update(context.TODO(), currentServiceCopy, metav1.UpdateOptions{})
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
			if !vip.IsIP(c.VIP) {
				ips, err := vip.LookupHost(c.VIP, sm.config.DNSMode)
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
		if !cmp.Equal(currentService.Status.LoadBalancer.Ingress, ingresses) {
			currentService.Status.LoadBalancer.Ingress = ingresses
			_, err = sm.clientSet.CoreV1().Services(currentService.Namespace).UpdateStatus(context.TODO(), currentService, metav1.UpdateOptions{})
			if err != nil {
				log.Error("updating Service", "namespace", i.ServiceSnapshot.Namespace, "name", i.ServiceSnapshot.Name, "err", err)
				return err
			}
		}
		return nil
	})

	return err
}

func isUPNPEnabled(s *v1.Service) bool {
	return metav1.HasAnnotation(s.ObjectMeta, upnpEnabled) && s.Annotations[upnpEnabled] == "true"
}
