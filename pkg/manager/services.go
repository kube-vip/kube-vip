package manager

import (
	"context"
	"fmt"
	"k8s.io/apimachinery/pkg/util/wait"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/go-cmp/cmp"
	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"

	"github.com/kube-vip/kube-vip/pkg/vip"
)

const (
	hwAddrKey                = "kube-vip.io/hwaddr"
	requestedIP              = "kube-vip.io/requestedIP"
	vipHost                  = "kube-vip.io/vipHost"
	egress                   = "kube-vip.io/egress"
	egressDestinationPorts   = "kube-vip.io/egress-destination-ports"
	egressSourcePorts        = "kube-vip.io/egress-source-ports"
	activeEndpoint           = "kube-vip.io/active-endpoint"
	activeEndpointIPv6       = "kube-vip.io/active-endpoint-ipv6"
	flushContrack            = "kube-vip.io/flush-conntrack"
	loadbalancerIPAnnotation = "kube-vip.io/loadbalancerIPs"
	loadbalancerHostname     = "kube-vip.io/loadbalancerHostname"
	serviceInterface         = "kube-vip.io/serviceInterface"
)

func (sm *Manager) syncServices(_ context.Context, svc *v1.Service, wg *sync.WaitGroup) error {
	defer wg.Done()

	log.Debugf("[STARTING] Service Sync")

	// Iterate through the synchronising services
	foundInstance := false
	newServiceAddresses := fetchServiceAddresses(svc)
	newServiceUID := string(svc.UID)

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
			log.Debugf("isDHCP: %t, newServiceAddress: %s", sm.serviceInstances[x].isDHCP, newServiceAddress)
			if sm.serviceInstances[x].UID == newServiceUID {
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
		if err := sm.addService(svc); err != nil {
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

func (sm *Manager) addService(svc *v1.Service) error {
	startTime := time.Now()

	newService, err := NewInstance(svc, sm.config)
	if err != nil {
		return err
	}

	for x := range newService.vipConfigs {
		newService.clusters[x].StartLoadBalancerService(newService.vipConfigs[x], sm.bgpServer)
	}

	sm.upnpMap(newService)

	if newService.isDHCP && len(newService.vipConfigs) == 1 {
		go func() {
			for ip := range newService.dhcpClient.IPChannel() {
				log.Debugf("IP %s may have changed", ip)
				newService.vipConfigs[0].VIP = ip
				newService.dhcpInterfaceIP = ip
				if !sm.config.DisableServiceUpdates {
					if err := sm.updateStatus(newService); err != nil {
						log.Warnf("error updating svc: %s", err)
					}
				}
			}
			log.Debugf("IP update channel closed, stopping")
		}()
	}

	sm.serviceInstances = append(sm.serviceInstances, newService)

	if !sm.config.DisableServiceUpdates {
		log.Debugf("(svcs) will update [%s/%s]", newService.serviceSnapshot.Namespace, newService.serviceSnapshot.Name)
		if err := sm.updateStatus(newService); err != nil {
			// delete service to collect garbage
			if deleteErr := sm.deleteService(newService.UID); deleteErr != nil {
				return deleteErr
			}
			return err
		}
	}

	serviceIPs := fetchServiceAddresses(svc)

	// Check if we need to flush any conntrack connections (due to some dangling conntrack connections)
	if svc.Annotations[flushContrack] == "true" {

		log.Debugf("Flushing conntrack rules for service [%s]", svc.Name)
		for _, serviceIP := range serviceIPs {
			err = vip.DeleteExistingSessions(serviceIP, false, svc.Annotations[egressDestinationPorts], svc.Annotations[egressSourcePorts])
			if err != nil {
				log.Errorf("Error flushing any remaining egress connections [%s]", err)
			}
			err = vip.DeleteExistingSessions(serviceIP, true, svc.Annotations[egressDestinationPorts], svc.Annotations[egressSourcePorts])
			if err != nil {
				log.Errorf("Error flushing any remaining ingress connections [%s]", err)
			}
		}
	}

	// Check if egress is enabled on the service, if so we'll need to configure some rules
	if svc.Annotations[egress] == "true" && len(serviceIPs) > 0 {
		log.Debugf("Enabling egress for the service [%s]", svc.Name)
		if svc.Annotations[activeEndpoint] != "" {
			// We will need to modify the iptables rules
			err = sm.iptablesCheck()
			if err != nil {
				log.Errorf("Error configuring egress for loadbalancer [%s]", err)
			}
			errList := []error{}
			for _, serviceIP := range serviceIPs {
				podIPs := svc.Annotations[activeEndpoint]
				if sm.config.EnableEndpointSlices && vip.IsIPv6(serviceIP) {
					podIPs = svc.Annotations[activeEndpointIPv6]
				}
				err = sm.configureEgress(serviceIP, podIPs, svc.Annotations[egressDestinationPorts], svc.Namespace)
				if err != nil {
					errList = append(errList, err)
					log.Errorf("Error configuring egress for loadbalancer [%s]", err)
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
					log.Errorf("error configuring egress annotation for loadbalancer [%s]", err)
				}

			}
		}
	}

	finishTime := time.Since(startTime)
	log.Infof("[service] synchronised in %dms", finishTime.Milliseconds())

	return nil
}

func (sm *Manager) deleteService(uid string) error {
	// protect multiple calls
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	var updatedInstances []*Instance
	var serviceInstance *Instance
	found := false
	for x := range sm.serviceInstances {
		log.Debugf("Looking for [%s], found [%s]", uid, sm.serviceInstances[x].UID)
		// Add the running services to the new array
		if sm.serviceInstances[x].UID != uid {
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
	shared := false
	vipSet := make(map[string]interface{})
	for x := range updatedInstances {
		for _, vip := range updatedInstances[x].VIPs {
			vipSet[vip] = nil
		}
	}
	for _, vip := range serviceInstance.VIPs {
		if _, found := vipSet[vip]; found {
			shared = true
		}
	}
	if !shared {
		for x := range serviceInstance.clusters {
			serviceInstance.clusters[x].Stop()
		}
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
		// TODO: Implement dual-stack loadbalancer support if BGP is enabled
		for i := range serviceInstance.vipConfigs {
			if serviceInstance.vipConfigs[i].EnableBGP {
				cidrVip := fmt.Sprintf("%s/%s", serviceInstance.vipConfigs[i].VIP, serviceInstance.vipConfigs[i].VIPCIDR)
				err := sm.bgpServer.DelHost(cidrVip)
				if err != nil {
					return fmt.Errorf("[BGP] error deleting BGP host: %v", err)
				}
				log.Debugf("[BGP] deleted host: %s", cidrVip)
			}
		}

		// We will need to tear down the egress
		if serviceInstance.serviceSnapshot.Annotations[egress] == "true" {
			if serviceInstance.serviceSnapshot.Annotations[activeEndpoint] != "" {
				log.Infof("service [%s] has an egress re-write enabled", serviceInstance.serviceSnapshot.Name)
				err := sm.TeardownEgress(serviceInstance.serviceSnapshot.Annotations[activeEndpoint], serviceInstance.serviceSnapshot.Spec.LoadBalancerIP, serviceInstance.serviceSnapshot.Annotations[egressDestinationPorts], serviceInstance.serviceSnapshot.Namespace)
				if err != nil {
					log.Errorf("%v", err)
				}
			}
		}
	}

	// Update the service array
	sm.serviceInstances = updatedInstances

	log.Infof("Removed [%s] from manager, [%d] advertised services remain", uid, len(sm.serviceInstances))

	return nil
}

func (sm *Manager) upnpMap(s *Instance) {
	// If upnp is enabled then update the gateway/router with the address
	// TODO - work out if we need to mapping.Reclaim()
	// TODO - check if this implementation for dualstack is correct
	if sm.upnp != nil {
		for _, vip := range s.VIPs {
			log.Infof("[UPNP] Adding map to [%s:%d - %s]", vip, s.Port, s.serviceSnapshot.Name)
			if err := sm.upnp.AddPortMapping(int(s.Port), int(s.Port), 0, vip, strings.ToUpper(s.Type), s.serviceSnapshot.Name); err == nil {
				log.Infof("service should be accessible externally on port [%d]", s.Port)
			} else {
				sm.upnp.Reclaim()
				log.Errorf("unable to map port to gateway [%s]", err.Error())
			}
		}
	}
}

func (sm *Manager) updateStatus(i *Instance) error {
	statusUpdateRetry := wait.Backoff{
		Steps:    5,
		Duration: 10 * time.Millisecond,
		Factor:   1.0,
		Jitter:   0.1,
	}
	if sm.config.ServiceStatusUpdateRetrySeconds > 0 {
		statusUpdateRetry.Duration = time.Duration(sm.config.ServiceStatusUpdateRetrySeconds * int(time.Second))
	}
	retryErr := retry.RetryOnConflict(statusUpdateRetry, func() error {
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

		if !cmp.Equal(currentService, currentServiceCopy) {
			currentService, err = sm.clientSet.CoreV1().Services(currentServiceCopy.Namespace).Update(context.TODO(), currentServiceCopy, metav1.UpdateOptions{})
			if err != nil {
				log.Errorf("Error updating Service Spec [%s] : %v", i.serviceSnapshot.Name, err)
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
		}
		if !cmp.Equal(currentService.Status.LoadBalancer.Ingress, ingresses) {
			currentService.Status.LoadBalancer.Ingress = ingresses
			_, err = sm.clientSet.CoreV1().Services(currentService.Namespace).UpdateStatus(context.TODO(), currentService, metav1.UpdateOptions{})
			if err != nil {
				log.Errorf("Error updating Service %s/%s Status: %v", i.serviceSnapshot.Namespace, i.serviceSnapshot.Name, err)
				return err
			}
		}
		return nil
	})

	if retryErr != nil {
		log.Errorf("Failed to set Services: %v", retryErr)
		return retryErr
	}
	return nil
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

	if !annotationAvailable {
		if len(s.Status.LoadBalancer.Ingress) > 0 {
			addresses := []string{}
			for _, ingress := range s.Status.LoadBalancer.Ingress {
				addresses = append(addresses, ingress.IP)
			}
			return addresses
		}
	}

	if s.Spec.LoadBalancerIP != "" {
		return []string{s.Spec.LoadBalancerIP}
	}

	return []string{}
}
