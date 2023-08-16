package manager

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

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
	endpoint                 = "kube-vip.io/active-endpoint"
	flushContrack            = "kube-vip.io/flush-conntrack"
	loadbalancerIPAnnotation = "kube-vip.io/loadbalancerIPs"
)

func (sm *Manager) syncServices(_ context.Context, svc *v1.Service, wg *sync.WaitGroup) error {
	defer wg.Done()

	log.Debugf("[STARTING] Service Sync")

	// Iterate through the synchronising services
	foundInstance := false
	newServiceAddress := fetchServiceAddress(svc)
	newServiceUID := string(svc.UID)

	for x := range sm.serviceInstances {
		if sm.serviceInstances[x].UID == newServiceUID {
			log.Debugf("isDHCP: %t, newServiceAddress: %s", sm.serviceInstances[x].isDHCP, newServiceAddress)
			// If the found instance's DHCP configuration doesn't match the new service, delete it.
			if (sm.serviceInstances[x].isDHCP && newServiceAddress != "0.0.0.0") ||
				(!sm.serviceInstances[x].isDHCP && newServiceAddress == "0.0.0.0") ||
				(!sm.serviceInstances[x].isDHCP && len(svc.Status.LoadBalancer.Ingress) > 0 &&
					newServiceAddress != svc.Status.LoadBalancer.Ingress[0].IP) ||
				(len(svc.Status.LoadBalancer.Ingress) > 0 && !comparePortsAndPortStatuses(svc)) ||
				(sm.serviceInstances[x].isDHCP && len(svc.Status.LoadBalancer.Ingress) > 0 &&
					sm.serviceInstances[x].dhcpInterfaceIP != svc.Status.LoadBalancer.Ingress[0].IP) {
				if err := sm.deleteService(newServiceUID); err != nil {
					return err
				}
				break
			}
			foundInstance = true
		}
	}

	// This instance wasn't found, we need to add it to the manager
	if !foundInstance && newServiceAddress != "" {
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

	log.Infof("[service] adding VIP [%s] for [%s/%s]", newService.Vip, newService.serviceSnapshot.Namespace, newService.serviceSnapshot.Name)

	newService.cluster.StartLoadBalancerService(newService.vipConfig, sm.bgpServer)

	sm.upnpMap(newService)

	if newService.isDHCP {
		go func() {
			for ip := range newService.dhcpClient.IPChannel() {
				log.Debugf("IP %s may have changed", ip)
				newService.vipConfig.VIP = ip
				newService.dhcpInterfaceIP = ip
				if err := sm.updateStatus(newService); err != nil {
					log.Warnf("error updating svc: %s", err)
				}
			}
			log.Debugf("IP update channel closed, stopping")
		}()
	}

	sm.serviceInstances = append(sm.serviceInstances, newService)

	if err := sm.updateStatus(newService); err != nil {
		// delete service to collect garbage
		if deleteErr := sm.deleteService(newService.UID); err != nil {
			return deleteErr
		}
		return err
	}

	serviceIP := fetchServiceAddress(svc)

	// Check if we need to flush any conntrack connections (due to some dangling conntrack connections)
	if svc.Annotations[flushContrack] == "true" {
		log.Debugf("Flushing conntrack rules for service [%s]", svc.Name)
		err = vip.DeleteExistingSessions(serviceIP, false)
		if err != nil {
			log.Errorf("Error flushing any remaining egress connections [%s]", err)
		}
		err = vip.DeleteExistingSessions(serviceIP, true)
		if err != nil {
			log.Errorf("Error flushing any remaining ingress connections [%s]", err)
		}
	}

	// Check if egress is enabled on the service, if so we'll need to configure some rules
	if svc.Annotations[egress] == "true" {
		log.Debugf("Enabling egress for the service [%s]", svc.Name)
		if svc.Annotations[endpoint] != "" {
			// We will need to modify the iptables rules
			err = sm.iptablesCheck()
			if err != nil {
				log.Errorf("Error configuring egress for loadbalancer [%s]", err)
			}
			err = sm.configureEgress(serviceIP, svc.Annotations[endpoint], svc.Annotations[egressDestinationPorts], svc.Namespace)
			if err != nil {
				log.Errorf("Error configuring egress for loadbalancer [%s]", err)
			} else {
				err = sm.updateServiceEndpointAnnotation(svc.Annotations[endpoint], svc)
				if err != nil {
					log.Errorf("Error configuring egress annotation for loadbalancer [%s]", err)
				}
			}
		}
	}
	finishTime := time.Since(startTime)
	log.Infof("[service] synchronised in %dms", finishTime.Milliseconds())

	return nil
}

func (sm *Manager) deleteService(uid string) error {
	// pretect multiple calls
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
	} else {
		shared := false
		for x := range updatedInstances {
			if updatedInstances[x].Vip == serviceInstance.Vip {
				shared = true
			}
		}
		if !shared {
			serviceInstance.cluster.Stop()
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
			if serviceInstance.vipConfig.EnableBGP {
				cidrVip := fmt.Sprintf("%s/%s", serviceInstance.vipConfig.VIP, serviceInstance.vipConfig.VIPCIDR)
				err := sm.bgpServer.DelHost(cidrVip)
				return err
			}

			// We will need to tear down the egress
			if serviceInstance.serviceSnapshot.Annotations[egress] == "true" {
				if serviceInstance.serviceSnapshot.Annotations[endpoint] != "" {

					log.Infof("service [%s] has an egress re-write enabled", serviceInstance.serviceSnapshot.Name)
					err := sm.TeardownEgress(serviceInstance.serviceSnapshot.Annotations[endpoint], serviceInstance.serviceSnapshot.Spec.LoadBalancerIP, serviceInstance.serviceSnapshot.Annotations[egressDestinationPorts], serviceInstance.serviceSnapshot.Namespace)
					if err != nil {
						log.Errorf("%v", err)
					}
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
	if sm.upnp != nil {
		log.Infof("[UPNP] Adding map to [%s:%d - %s]", s.Vip, s.Port, s.serviceSnapshot.Name)
		if err := sm.upnp.AddPortMapping(int(s.Port), int(s.Port), 0, s.Vip, strings.ToUpper(s.Type), s.serviceSnapshot.Name); err == nil {
			log.Infof("service should be accessible externally on port [%d]", s.Port)
		} else {
			sm.upnp.Reclaim()
			log.Errorf("unable to map port to gateway [%s]", err.Error())
		}
	}
}

func (sm *Manager) updateStatus(i *Instance) error {
	retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Retrieve the latest version of Deployment before attempting update
		// RetryOnConflict uses exponential backoff to avoid exhausting the apiserver
		currentService, err := sm.clientSet.CoreV1().Services(i.serviceSnapshot.Namespace).Get(context.TODO(), i.serviceSnapshot.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}

		id, err := os.Hostname()
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
			currentServiceCopy.Annotations[vipHost] = id
		}
		if i.dhcpInterfaceHwaddr != "" || i.dhcpInterfaceIP != "" {
			currentServiceCopy.Annotations[hwAddrKey] = i.dhcpInterfaceHwaddr
			currentServiceCopy.Annotations[requestedIP] = i.dhcpInterfaceIP
		}

		updatedService, err := sm.clientSet.CoreV1().Services(currentService.Namespace).Update(context.TODO(), currentServiceCopy, metav1.UpdateOptions{})
		if err != nil {
			log.Errorf("Error updating Service Spec [%s] : %v", i.serviceSnapshot.Name, err)
			return err
		}

		ports := make([]v1.PortStatus, 0, len(i.serviceSnapshot.Spec.Ports))
		for _, port := range i.serviceSnapshot.Spec.Ports {
			ports = append(ports, v1.PortStatus{
				Port:     port.Port,
				Protocol: port.Protocol,
			})
		}
		updatedService.Status.LoadBalancer.Ingress = []v1.LoadBalancerIngress{{
			IP:    i.vipConfig.VIP,
			Ports: ports,
		}}
		_, err = sm.clientSet.CoreV1().Services(updatedService.Namespace).UpdateStatus(context.TODO(), updatedService, metav1.UpdateOptions{})
		if err != nil {
			log.Errorf("Error updating Service %s/%s Status: %v", i.serviceSnapshot.Namespace, i.serviceSnapshot.Name, err)
			return err
		}
		return nil
	})

	if retryErr != nil {
		log.Errorf("Failed to set Services: %v", retryErr)
		return retryErr
	}
	return nil
}

// fetchServiceAddress tries to get the address from annotations
// kube-vip.io/loadbalancerIPs, then from spec.loadbalancerIP
func fetchServiceAddress(s *v1.Service) string {
	if s.Annotations != nil {
		if v, ok := s.Annotations[loadbalancerIPAnnotation]; ok {
			return v
		}
	}
	return s.Spec.LoadBalancerIP
}
