package manager

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/kube-vip/kube-vip/pkg/vip"
	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
)

const (
	hwAddrKey              = "kube-vip.io/hwaddr"
	requestedIP            = "kube-vip.io/requestedIP"
	vipHost                = "kube-vip.io/vipHost"
	egress                 = "kube-vip.io/egress"
	egressDestinationPorts = "kube-vip.io/egress-destination-ports"
	egressSourcePorts      = "kube-vip.io/egress-source-ports"
	endpoint               = "kube-vip.io/active-endpoint"
	flushContrack          = "kube-vip.io/flush-conntrack"
)

func (sm *Manager) syncServices(ctx context.Context, service *v1.Service, wg *sync.WaitGroup) error {
	defer wg.Done()

	log.Debugf("[STARTING] Service Sync")

	// Iterate through the synchronising services
	foundInstance := false
	newServiceAddress := service.Spec.LoadBalancerIP
	newServiceUID := string(service.UID)

	for x := range sm.serviceInstances {
		if sm.serviceInstances[x].UID == newServiceUID {
			log.Debugf("isDHCP: %t, newServiceAddress: %s", sm.serviceInstances[x].isDHCP, newServiceAddress)
			// If the found instance's DHCP configuration doesn't match the new service, delete it.
			if sm.serviceInstances[x].isDHCP && newServiceAddress != "0.0.0.0" ||
				!sm.serviceInstances[x].isDHCP && newServiceAddress == "0.0.0.0" ||
				!sm.serviceInstances[x].isDHCP && len(service.Status.LoadBalancer.Ingress) > 0 &&
					newServiceAddress != service.Status.LoadBalancer.Ingress[0].IP {
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
		if err := sm.addService(service); err != nil {
			return err
		}
	}

	return nil
}

func (sm *Manager) addService(service *v1.Service) error {
	startTime := time.Now()

	newService, err := NewInstance(service, sm.config)
	if err != nil {
		return err
	}

	log.Infof("[service] adding VIP [%s] for [%s/%s] ", newService.Vip, newService.serviceSnapshot.Namespace, newService.serviceSnapshot.Name)

	newService.cluster.StartLoadBalancerService(newService.vipConfig, sm.bgpServer)

	sm.upnpMap(newService)

	sm.serviceInstances = append(sm.serviceInstances, newService)

	if err := sm.updateStatus(newService); err != nil {
		// delete service to collect garbage
		if deleteErr := sm.deleteService(newService.UID); err != nil {
			return deleteErr
		}
		return err
	}

	// Check if we need to flush any conntrack connections (due to some dangling conntrack connections)
	if service.Annotations[flushContrack] == "true" {
		log.Debugf("Flushing conntrack rules for service [%s]", service.Name)
		err = vip.DeleteExistingSessions(service.Spec.LoadBalancerIP, false)
		if err != nil {
			log.Errorf("Error flushing any remaining egress connections [%s]", err)
		}
		err = vip.DeleteExistingSessions(service.Spec.LoadBalancerIP, true)
		if err != nil {
			log.Errorf("Error flushing any remaining ingress connections [%s]", err)
		}
	}

	// Check if egress is enabled on the service, if so we'll need to configure some rules
	if service.Annotations[egress] == "true" {
		log.Debugf("Enabling egress for the service [%s]", service.Name)
		if service.Annotations[endpoint] != "" {
			// We will need to modify the iptables rules
			err = sm.iptablesCheck()
			if err != nil {
				log.Errorf("Error configuring egress for loadbalancer [%s]", err)
			}
			err = sm.configureEgress(service.Spec.LoadBalancerIP, service.Annotations[endpoint], service.Annotations[egressDestinationPorts], service.Annotations[egressSourcePorts])
			if err != nil {
				log.Errorf("Error configuring egress for loadbalancer [%s]", err)
			} else {
				err = sm.updateServiceEndpointAnnotation(service.Annotations[endpoint], service)
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
	//pretect multiple calls
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	var updatedInstances []*Instance
	found := false
	for x := range sm.serviceInstances {
		log.Debugf("Looking for [%s], found [%s]", uid, sm.serviceInstances[x].UID)
		// Add the running services to the new array
		if sm.serviceInstances[x].UID != uid {
			updatedInstances = append(updatedInstances, sm.serviceInstances[x])
		} else {
			// Flip the found when we match
			found = true
			sm.serviceInstances[x].cluster.Stop()
			if sm.serviceInstances[x].isDHCP {
				sm.serviceInstances[x].dhcpClient.Stop()
				macvlan, err := netlink.LinkByName(sm.serviceInstances[x].dhcpInterface)
				if err != nil {
					return fmt.Errorf("error finding VIP Interface: %v", err)
				}

				err = netlink.LinkDel(macvlan)
				if err != nil {
					return fmt.Errorf("error deleting DHCP Link : %v", err)
				}
			}
			if sm.serviceInstances[x].vipConfig.EnableBGP {
				cidrVip := fmt.Sprintf("%s/%s", sm.serviceInstances[x].vipConfig.VIP, sm.serviceInstances[x].vipConfig.VIPCIDR)
				err := sm.bgpServer.DelHost(cidrVip)
				return err
			}

			// We will need to tear down the egress
			if sm.serviceInstances[x].serviceSnapshot.Annotations[egress] == "true" {
				if sm.serviceInstances[x].serviceSnapshot.Annotations[endpoint] != "" {

					log.Infof("service [%s] has an egress re-write enabled", sm.serviceInstances[x].serviceSnapshot.Name)
					err := TeardownEgress(sm.serviceInstances[x].serviceSnapshot.Annotations[endpoint], sm.serviceInstances[x].serviceSnapshot.Spec.LoadBalancerIP, sm.serviceInstances[x].serviceSnapshot.Annotations[egressDestinationPorts])
					if err != nil {
						log.Errorf("%v", err)
					}
				}
			}
		}
	}
	// If we've been through all services and not found the correct one then error
	if !found {
		// TODO: - fix UX
		//return fmt.Errorf("unable to find/stop service [%s]", uid)
		return nil
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

		updatedService.Status.LoadBalancer.Ingress = []v1.LoadBalancerIngress{{IP: i.vipConfig.VIP}}
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
