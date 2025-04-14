package manager

import (
	"context"
	"fmt"
	"net"
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

	"github.com/kube-vip/kube-vip/pkg/upnp"
	"github.com/kube-vip/kube-vip/pkg/vip"
)

func (sm *Manager) syncServices(ctx context.Context, svc *v1.Service) error {
	log.Debug("[STARTING] Service Sync")

	// Iterate through the synchronising services
	foundInstance := false
	newServiceAddresses := fetchServiceAddresses(svc)
	newServiceUID := svc.UID

	ingressIPs := []string{}

	for _, ingress := range svc.Status.LoadBalancer.Ingress {
		ingressIPs = append(ingressIPs, ingress.IP)
	}

	shouldBreake := false

	for x := range sm.serviceInstances {
		if shouldBreake {
			break
		}
		for _, newServiceAddress := range newServiceAddresses {
			log.Debug("service", "isDHCP", sm.serviceInstances[x].isDHCP, "newServiceAddress", newServiceAddress)
			if sm.serviceInstances[x].serviceSnapshot.UID == newServiceUID {
				// If the found instance's DHCP configuration doesn't match the new service, delete it.
				if (sm.serviceInstances[x].isDHCP && newServiceAddress != "0.0.0.0") ||
					(!sm.serviceInstances[x].isDHCP && newServiceAddress == "0.0.0.0") ||
					(!sm.serviceInstances[x].isDHCP && len(svc.Status.LoadBalancer.Ingress) > 0 && !slices.Contains(ingressIPs, newServiceAddress)) ||
					(len(svc.Status.LoadBalancer.Ingress) > 0 && !comparePortsAndPortStatuses(svc)) ||
					(sm.serviceInstances[x].isDHCP && len(svc.Status.LoadBalancer.Ingress) > 0 && !slices.Contains(ingressIPs, sm.serviceInstances[x].dhcpInterfaceIP)) {
					if err := sm.deleteService(newServiceUID); err != nil {
						return err
					}
					shouldBreake = true
					break
				}
				foundInstance = true
			}
		}
	}

	// This instance wasn't found, we need to add it to the manager
	if !foundInstance && len(newServiceAddresses) > 0 {
		if err := sm.addService(ctx, svc); err != nil {
			return err
		}
	}

	return nil
}

func comparePortsAndPortStatuses(svc *v1.Service) bool {
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
	startTime := time.Now()

	newService, err := NewInstance(svc, sm.config, sm.intfMgr, sm.arpMgr)
	if err != nil {
		return err
	}

	for x := range newService.vipConfigs {
		log.Debug("starting loadbalancer for service", "name", svc.Name, "namespace", svc.Namespace)
		newService.clusters[x].StartLoadBalancerService(newService.vipConfigs[x], sm.bgpServer, svc.Name)
	}

	sm.upnpMap(ctx, newService)

	if newService.isDHCP && len(newService.vipConfigs) == 1 {
		go func() {
			for ip := range newService.dhcpClient.IPChannel() {
				log.Debug("IP changed", "ip", ip)
				newService.vipConfigs[0].VIP = ip
				newService.dhcpInterfaceIP = ip
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
		log.Debug("[service] update", "namespace", newService.serviceSnapshot.Namespace, "name", newService.serviceSnapshot.Name)
		if err := sm.updateStatus(newService); err != nil {
			log.Error("[service] updating status", "namespace", newService.serviceSnapshot.Namespace, "name", newService.serviceSnapshot.Name, "err", err)
		}
	}

	serviceIPs := fetchServiceAddresses(svc)
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

	var updatedInstances []*Instance
	var serviceInstance *Instance
	found := false
	for x := range sm.serviceInstances {
		log.Debug("service lookup", "target UID", uid, "found UID ", sm.serviceInstances[x].serviceSnapshot.UID)
		// Add the running services to the new array
		if sm.serviceInstances[x].serviceSnapshot.UID != uid {
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

	for _, c := range serviceInstance.clusters {
		for n := range c.Network {
			c.Network[n].SetHasEndpoints(false)
		}
	}

	// Determine if this this VIP is shared with other loadbalancers
	shared := false
	vipSet := make(map[string]interface{})
	for x := range updatedInstances {
		for _, vip := range fetchServiceAddresses(updatedInstances[x].serviceSnapshot) { //updatedInstances[x].serviceSnapshot.Spec.LoadBalancerIP {
			vipSet[vip] = nil
		}
	}
	for _, vip := range fetchServiceAddresses(serviceInstance.serviceSnapshot) {
		if _, found := vipSet[vip]; found {
			shared = true
		}
	}
	for x := range serviceInstance.clusters {
		serviceInstance.clusters[x].Stop()
	}
	if !shared {
		if serviceInstance.isDHCP {
			serviceInstance.dhcpClient.Stop()
			macvlan, err := netlink.LinkByName(serviceInstance.dhcpInterface)
			if err != nil {
				return fmt.Errorf("error finding VIP Interface: %v", err)
			}

			err = netlink.LinkDel(macvlan)
			if err != nil {
				return fmt.Errorf("error deleting DHCP Link : %v", err)
			}
		}
		for i := range serviceInstance.vipConfigs {
			if serviceInstance.vipConfigs[i].EnableBGP {
				sm.clearBGPHosts(serviceInstance.serviceSnapshot)
			}
		}

		// We will need to tear down the egress
		if serviceInstance.serviceSnapshot.Annotations[egress] == "true" {
			if serviceInstance.serviceSnapshot.Annotations[activeEndpoint] != "" {
				log.Info("egress re-write enabled", "service", serviceInstance.serviceSnapshot.Name)
				err := sm.TeardownEgress(serviceInstance.serviceSnapshot.Annotations[activeEndpoint], serviceInstance.serviceSnapshot.Spec.LoadBalancerIP, serviceInstance.serviceSnapshot.Namespace, serviceInstance.serviceSnapshot.Annotations)
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
func (sm *Manager) upnpMap(ctx context.Context, s *Instance) {
	if !isUPNPEnabled(s.serviceSnapshot) {
		// Skip services missing the annotation
		return
	}
	if !sm.upnp {
		log.Warn("[UPNP] Found kube-vip.io/forwardUPNP on service while UPNP forwarding is disabled in the kube-vip config. Not forwarding", "service", s.serviceSnapshot.Name)
	}
	// If upnp is enabled then update the gateway/router with the address
	// TODO - check if this implementation for dualstack is correct

	gateways := upnp.GetGatewayClients(ctx)

	// Reset Gateway IPs to remove stale addresses
	s.upnpGatewayIPs = make([]string, 0)

	for _, vip := range fetchServiceAddresses(s.serviceSnapshot) {
		for _, port := range s.serviceSnapshot.Spec.Ports {
			for _, gw := range gateways {
				log.Info("[UPNP] Adding map", "vip", vip, "port", port.Port, "service", s.serviceSnapshot.Name, "gateway", gw.ConnectionClient.GetServiceClient().Location)

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
					portMappingErr := gw.ConnectionClient.AddPortMapping("0.0.0.0", uint16(port.Port), strings.ToUpper(string(port.Protocol)), uint16(port.Port), vip, true, s.serviceSnapshot.Name, 3600) //nolint  TODO
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
						s.upnpGatewayIPs = append(s.upnpGatewayIPs, ip)
					}
				}
			}
		}
	}

	// Remove duplicate IPs
	slices.Sort(s.upnpGatewayIPs)
	s.upnpGatewayIPs = slices.Compact(s.upnpGatewayIPs)
}

func (sm *Manager) updateStatus(i *Instance) error {
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
		currentService, err := sm.clientSet.CoreV1().Services(i.serviceSnapshot.Namespace).Get(context.TODO(), i.serviceSnapshot.Name, metav1.GetOptions{})
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
		if i.dhcpInterfaceHwaddr != "" || i.dhcpInterfaceIP != "" {
			currentServiceCopy.Annotations[hwAddrKey] = i.dhcpInterfaceHwaddr
			currentServiceCopy.Annotations[requestedIP] = i.dhcpInterfaceIP
		}

		if currentService.Annotations["development.kube-vip.io/synthetic-api-server-error-on-update"] == "true" {
			log.Error("(Synthetic error ) updating Spec", "service", i.serviceSnapshot.Name, "err", err)
			return fmt.Errorf("(Synthetic) simulating api server errors")
		}

		if !cmp.Equal(currentService, currentServiceCopy) {
			currentService, err = sm.clientSet.CoreV1().Services(currentServiceCopy.Namespace).Update(context.TODO(), currentServiceCopy, metav1.UpdateOptions{})
			if err != nil {
				log.Error("updating Spec", "service", i.serviceSnapshot.Name, "err", err)
				return err
			}
		}

		ports := make([]v1.PortStatus, 0, len(i.serviceSnapshot.Spec.Ports))
		for _, port := range i.serviceSnapshot.Spec.Ports {
			ports = append(ports, v1.PortStatus{
				Port:     port.Port,
				Protocol: port.Protocol,
			})
		}

		ingresses := []v1.LoadBalancerIngress{}

		for _, c := range i.vipConfigs {
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
				for _, ip := range i.upnpGatewayIPs {
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
				log.Error("updating Service", "namespace", i.serviceSnapshot.Namespace, "name", i.serviceSnapshot.Name, "err", err)
				return err
			}
		}
		return nil
	})

	return err
}

// fetchServiceAddresses tries to get the addresses from annotations
// kube-vip.io/loadbalancerIPs, then from spec.loadbalancerIP
func fetchServiceAddresses(s *v1.Service) []string {
	annotationAvailable := false
	if s.Annotations != nil {
		if v, annotationAvailable := s.Annotations[loadbalancerIPAnnotation]; annotationAvailable {
			ips := strings.Split(v, ",")
			var trimmedIPs []string
			for _, ip := range ips {
				trimmedIPs = append(trimmedIPs, strings.TrimSpace(ip))
			}
			return trimmedIPs
		}
	}

	lbStatusAddresses := []string{}
	if !annotationAvailable {
		if len(s.Status.LoadBalancer.Ingress) > 0 {
			for _, ingress := range s.Status.LoadBalancer.Ingress {
				lbStatusAddresses = append(lbStatusAddresses, ingress.IP)
			}
		}
	}

	lbIP := net.ParseIP(s.Spec.LoadBalancerIP)
	isLbIPv4 := vip.IsIPv4(s.Spec.LoadBalancerIP)

	if len(lbStatusAddresses) > 0 {
		for _, a := range lbStatusAddresses {
			if lbStatusIP := net.ParseIP(a); lbStatusIP != nil && lbIP != nil && vip.IsIPv4(a) == isLbIPv4 && !lbIP.Equal(lbStatusIP) {
				return []string{s.Spec.LoadBalancerIP}
			}
		}
		return lbStatusAddresses
	}

	if s.Spec.LoadBalancerIP != "" {
		return []string{s.Spec.LoadBalancerIP}
	}

	return []string{}
}

func isUPNPEnabled(s *v1.Service) bool {
	return metav1.HasAnnotation(s.ObjectMeta, upnpEnabled) && s.Annotations[upnpEnabled] == "true"
}
