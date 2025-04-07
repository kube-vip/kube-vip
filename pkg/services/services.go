package services

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

	"github.com/kube-vip/kube-vip/pkg/egress"
	"github.com/kube-vip/kube-vip/pkg/endpoints/providers"
	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/upnp"
	"github.com/kube-vip/kube-vip/pkg/vip"
)

func (p *Processor) SyncServices(ctx context.Context, svc *v1.Service) error {
	log.Debug("[STARTING] Service Sync")

	// Iterate through the synchronising services
	foundInstance := false
	newServiceAddresses := instance.FetchServiceAddresses(svc)
	newServiceUID := svc.UID

	ingressIPs := []string{}

	for _, ingress := range svc.Status.LoadBalancer.Ingress {
		ingressIPs = append(ingressIPs, ingress.IP)
	}

	shouldBreake := false

	for x := range p.ServiceInstances {
		if shouldBreake {
			break
		}
		for _, newServiceAddress := range newServiceAddresses {
			log.Debug("service", "IsDHCP", p.ServiceInstances[x].IsDHCP, "newServiceAddress", newServiceAddress)
			if p.ServiceInstances[x].ServiceSnapshot.UID == newServiceUID {
				// If the found instance's DHCP configuration doesn't match the new service, delete it.
				if (p.ServiceInstances[x].IsDHCP && newServiceAddress != "0.0.0.0") ||
					(!p.ServiceInstances[x].IsDHCP && newServiceAddress == "0.0.0.0") ||
					(!p.ServiceInstances[x].IsDHCP && len(svc.Status.LoadBalancer.Ingress) > 0 && !slices.Contains(ingressIPs, newServiceAddress)) ||
					(len(svc.Status.LoadBalancer.Ingress) > 0 && !comparePortsAndPortStatuses(svc)) ||
					(p.ServiceInstances[x].IsDHCP && len(svc.Status.LoadBalancer.Ingress) > 0 && !slices.Contains(ingressIPs, p.ServiceInstances[x].DhcpInterfaceIP)) {
					if err := p.deleteService(newServiceUID); err != nil {
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
		if err := p.addService(ctx, svc); err != nil {
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

func (p *Processor) addService(ctx context.Context, svc *v1.Service) error {
	startTime := time.Now()

	newService, err := instance.NewInstance(svc, p.config)
	if err != nil {
		return err
	}

	for x := range newService.VipConfigs {
		newService.Clusters[x].StartLoadBalancerService(newService.VipConfigs[x], p.bgpServer)
	}

	p.upnpMap(ctx, newService)

	if newService.IsDHCP && len(newService.VipConfigs) == 1 {
		go func() {
			for ip := range newService.DhcpClient.IPChannel() {
				log.Debug("IP changed", "ip", ip)
				newService.VipConfigs[0].VIP = ip
				newService.DhcpInterfaceIP = ip
				if !p.config.DisableServiceUpdates {
					if err := p.updateStatus(newService); err != nil {
						log.Warn("updating svc", "err", err)
					}
				}
			}
			log.Debug("IP update channel closed, stopping")
		}()
	}

	p.ServiceInstances = append(p.ServiceInstances, newService)

	if !p.config.DisableServiceUpdates {
		log.Debug("service update", "namespace", newService.ServiceSnapshot.Namespace, "name", newService.ServiceSnapshot.Name)
		if err := p.updateStatus(newService); err != nil {
			log.Error("updating status", "namespace", newService.ServiceSnapshot.Namespace, "name", newService.ServiceSnapshot.Name, "err", err)
		}
	}

	serviceIPs := instance.FetchServiceAddresses(svc)
	// Check if we need to flush any conntrack connections (due to some dangling conntrack connections)
	if svc.Annotations[kubevip.FlushContrack] == "true" {

		log.Debug("Flushing conntrack rules", "service", svc.Name)
		for _, serviceIP := range serviceIPs {
			err = vip.DeleteExistingSessions(serviceIP, false, svc.Annotations[kubevip.EgressDestinationPorts], svc.Annotations[kubevip.EgressSourcePorts])
			if err != nil {
				log.Error("flushing any remaining egress connections", "err", err)
			}
			err = vip.DeleteExistingSessions(serviceIP, true, svc.Annotations[kubevip.EgressDestinationPorts], svc.Annotations[kubevip.EgressSourcePorts])
			if err != nil {
				log.Error("flushing any remaining ingress connections", "err", err)
			}
		}
	}

	// Check if egress is enabled on the service, if so we'll need to configure some rules
	if svc.Annotations[kubevip.Egress] == "true" && len(serviceIPs) > 0 {
		log.Debug("enabling egress", "service", svc.Name)
		// We will need to modify the iptables rules
		err = p.iptablesCheck()
		if err != nil {
			log.Error("configuring egress", "service", svc.Name, "err", err)
		}
		var podIP string
		errList := []error{}

		// Should egress be IPv6
		if svc.Annotations[kubevip.EgressIPv6] == "true" {
			// Does the service have an active IPv6 endpoint
			if svc.Annotations[kubevip.ActiveEndpointIPv6] != "" {
				for _, serviceIP := range serviceIPs {
					if p.config.EnableEndpointSlices && vip.IsIPv6(serviceIP) {

						podIP = svc.Annotations[kubevip.ActiveEndpointIPv6]

						err = p.configureEgress(serviceIP, podIP, svc.Namespace, svc.Annotations)
						if err != nil {
							errList = append(errList, err)
							log.Error("configuring egress", "service", svc.Name, "err", err)
						}
					}
				}
			}
		} else if svc.Annotations[kubevip.ActiveEndpoint] != "" { // Not expected to be IPv6, so should be an IPv4 address
			for _, serviceIP := range serviceIPs {
				podIPs := svc.Annotations[kubevip.ActiveEndpoint]
				if p.config.EnableEndpointSlices && vip.IsIPv6(serviceIP) {
					podIPs = svc.Annotations[kubevip.ActiveEndpointIPv6]
				}
				err = p.configureEgress(serviceIP, podIPs, svc.Namespace, svc.Annotations)
				if err != nil {
					errList = append(errList, err)
					log.Error("configuring egress", "service", svc.Name, "err", err)
				}
			}
		}
		if len(errList) == 0 {
			var provider providers.Provider
			if !p.config.EnableEndpointSlices {
				provider = providers.NewEndpoints()
			} else {
				provider = providers.NewEndpointslices()
			}
			err = provider.UpdateServiceAnnotation(svc.Annotations[kubevip.ActiveEndpoint], svc.Annotations[kubevip.ActiveEndpointIPv6], svc, p.clientSet)
			if err != nil {
				log.Error("configuring egress", "service", svc.Name, "err", err)
			}
		}
	}

	finishTime := time.Since(startTime)
	log.Info("[service] synchronised", "in", fmt.Sprintf("%dms", finishTime.Milliseconds()))

	return nil
}

func (p *Processor) deleteService(uid types.UID) error {
	// protect multiple calls
	p.mutex.Lock()
	defer p.mutex.Unlock()

	var updatedInstances []*instance.Instance
	var serviceInstance *instance.Instance
	found := false
	for x := range p.ServiceInstances {
		log.Debug("service lookup", "target UID", uid, "found UID ", p.ServiceInstances[x].ServiceSnapshot.UID)
		// Add the running services to the new array
		if p.ServiceInstances[x].ServiceSnapshot.UID != uid {
			updatedInstances = append(updatedInstances, p.ServiceInstances[x])
		} else {
			// Flip the found when we match
			found = true
			serviceInstance = p.ServiceInstances[x]
		}
	}
	// If we've been through all services and not found the correct one then error
	if !found {
		// TODO: - fix UX
		// return fmt.Errorf("unable to find/stop service [%s]", uid)
		return nil
	}

	// Determine if this this VIP is shared with other loadbalancers
	shared := false
	vipSet := make(map[string]interface{})
	for x := range updatedInstances {
		for _, vip := range instance.FetchServiceAddresses(updatedInstances[x].ServiceSnapshot) { //updatedInstances[x].ServiceSnapshot.Spec.LoadBalancerIP {
			vipSet[vip] = nil
		}
	}
	for _, vip := range instance.FetchServiceAddresses(serviceInstance.ServiceSnapshot) {
		if _, found := vipSet[vip]; found {
			shared = true
		}
	}
	if !shared {
		for x := range serviceInstance.Clusters {
			serviceInstance.Clusters[x].Stop()
		}
		if serviceInstance.IsDHCP {
			serviceInstance.DhcpClient.Stop()
			macvlan, err := netlink.LinkByName(serviceInstance.DhcpInterface)
			if err != nil {
				return fmt.Errorf("error finding VIP Interface: %v", err)
			}

			err = netlink.LinkDel(macvlan)
			if err != nil {
				return fmt.Errorf("error deleting DHCP Link : %v", err)
			}
		}
		// TODO: Implement dual-stack loadbalancer support if BGP is enabled
		for i := range serviceInstance.VipConfigs {
			if serviceInstance.VipConfigs[i].EnableBGP {
				cidrVip := fmt.Sprintf("%s/%s", serviceInstance.VipConfigs[i].VIP, serviceInstance.VipConfigs[i].VIPCIDR)
				err := p.bgpServer.DelHost(cidrVip)
				if err != nil {
					return fmt.Errorf("[BGP] error deleting BGP host: %v", err)
				}
				log.Debug("[BGP] delete", "host", cidrVip)
			}
		}

		// We will need to tear down the egress
		if serviceInstance.ServiceSnapshot.Annotations[kubevip.Egress] == "true" {
			if serviceInstance.ServiceSnapshot.Annotations[kubevip.ActiveEndpoint] != "" {
				log.Info("egress re-write enabled", "service", serviceInstance.ServiceSnapshot.Name)
				err := egress.Teardown(serviceInstance.ServiceSnapshot.Annotations[kubevip.ActiveEndpoint], serviceInstance.ServiceSnapshot.Spec.LoadBalancerIP, serviceInstance.ServiceSnapshot.Namespace, serviceInstance.ServiceSnapshot.Annotations, p.config.EgressWithNftables)
				if err != nil {
					log.Error("egress teardown", "err", err)
				}
			}
		}
	}

	// Update the service array
	p.ServiceInstances = updatedInstances

	log.Info("Removed instance from manager", "uid", uid, "remaining advertised services", len(p.ServiceInstances))

	return nil
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

	// Reset Gateway IPs to remove stale addresses
	s.UpnpGatewayIPs = make([]string, 0)

	for _, vip := range instance.FetchServiceAddresses(s.ServiceSnapshot) {
		for _, port := range s.ServiceSnapshot.Spec.Ports {
			for _, gw := range gateways {
				log.Info("[UPNP] Adding map", "vip", vip, "port", port.Port, "service", s.ServiceSnapshot.Name, "gateway", gw.WANIPv6FirewallControlClient.Location)

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
						s.UpnpGatewayIPs = append(s.UpnpGatewayIPs, ip)
					}
				}
			}
		}
	}

	// Remove duplicate IPs
	slices.Sort(s.UpnpGatewayIPs)
	s.UpnpGatewayIPs = slices.Compact(s.UpnpGatewayIPs)
}

func (p *Processor) updateStatus(i *instance.Instance) error {
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
		currentService, err := p.clientSet.CoreV1().Services(i.ServiceSnapshot.Namespace).Get(context.TODO(), i.ServiceSnapshot.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}

		currentServiceCopy := currentService.DeepCopy()
		if currentServiceCopy.Annotations == nil {
			currentServiceCopy.Annotations = make(map[string]string)
		}

		// If we're using ARP then we can only broadcast the VIP from one place, add an annotation to the service
		if p.config.EnableARP {
			// Add the current host
			currentServiceCopy.Annotations[kubevip.VipHost] = p.config.NodeName
		}
		if i.DhcpInterfaceHwaddr != "" || i.DhcpInterfaceIP != "" {
			currentServiceCopy.Annotations[kubevip.HwAddrKey] = i.DhcpInterfaceHwaddr
			currentServiceCopy.Annotations[kubevip.RequestedIP] = i.DhcpInterfaceIP
		}

		if currentService.Annotations["development.kube-vip.io/synthetic-api-server-error-on-update"] == "true" {
			log.Error("(Synthetic error ) updating Spec", "service", i.ServiceSnapshot.Name, "err", err)
			return fmt.Errorf("(Synthetic) simulating api server errors")
		}

		if !cmp.Equal(currentService, currentServiceCopy) {
			currentService, err = p.clientSet.CoreV1().Services(currentServiceCopy.Namespace).Update(context.TODO(), currentServiceCopy, metav1.UpdateOptions{})
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

		for _, c := range i.VipConfigs {
			if !vip.IsIP(c.VIP) {
				ips, err := vip.LookupHost(c.VIP, p.config.DNSMode)
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
				for _, ip := range i.UpnpGatewayIPs {
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
			_, err = p.clientSet.CoreV1().Services(currentService.Namespace).UpdateStatus(context.TODO(), currentService, metav1.UpdateOptions{})
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
	return metav1.HasAnnotation(s.ObjectMeta, kubevip.UpnpEnabled) && s.Annotations[kubevip.UpnpEnabled] == "true"
}

// Refresh UPNP Port Forwards for all Service Instances registered in the processor
func (p *Processor) RefreshUPNPForwards() {
	log.Info("Starting UPNP Port Refresher")
	for {
		time.Sleep(300 * time.Second)

		log.Info("[UPNP] Refreshing Instances", "number of instances", len(p.ServiceInstances))
		for i := range p.ServiceInstances {
			p.upnpMap(context.TODO(), p.ServiceInstances[i])
			if err := p.updateStatus(p.ServiceInstances[i]); err != nil {
				log.Warn("[UPNP] Error updating service", "ip", p.ServiceInstances[i].ServiceSnapshot.Name, "err", err)
			}
		}
	}
}
