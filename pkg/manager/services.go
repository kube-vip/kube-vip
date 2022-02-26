package manager

import (
	"context"
	"fmt"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/retry"
	"strings"
	"sync"

	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kube-vip/kube-vip/pkg/cluster"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
)

const (
	hwAddrKey   = "kube-vip.io/hwaddr"
	requestedIP = "kube-vip.io/requestedIP"
)

func (sm *Manager) stopService(uid string) error {
	found := false
	for x := range sm.serviceInstances {
		if sm.serviceInstances[x].UID == uid {
			found = true
			sm.serviceInstances[x].cluster.Stop()
		}
	}
	if !found {
		return fmt.Errorf("unable to find/stop service [%s]", uid)
	}
	return nil
}

func (sm *Manager) deleteService(uid string) error {
	var updatedInstances []Instance
	found := false
	for x := range sm.serviceInstances {
		// Add the running services to the new array
		if sm.serviceInstances[x].UID != uid {
			updatedInstances = append(updatedInstances, sm.serviceInstances[x])
		} else {
			// Flip the found when we match
			found = true
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
		}
	}
	// If we've been through all services and not found the correct one then error
	if !found {
		return fmt.Errorf("unable to find/stop service [%s]", uid)
	}

	// Update the service array
	sm.serviceInstances = updatedInstances

	log.Infof("Removed [%s] from manager, [%d] advertised services remain", uid, len(sm.serviceInstances))

	return nil
}

func (sm *Manager) syncServices(service *v1.Service, wg *sync.WaitGroup) error {
	defer wg.Done()

	log.Debugf("[STARTING] Service Sync")
	// Iterate through the synchronising services
	foundInstance := false
	newServiceAddress := service.Spec.LoadBalancerIP
	newServiceUID := string(service.UID)

	for x := range sm.serviceInstances {
		log.Debugf("service %s", sm.serviceInstances[x].ServiceName)

		if sm.serviceInstances[x].UID == newServiceUID {
			// We have found this instance in the manager, we can determine if it needs updating
			foundInstance = true
		}

	}

	// Detect if we're using a specific interface for services
	var serviceInterface string
	if sm.config.ServicesInterface != "" {
		serviceInterface = sm.config.ServicesInterface
	} else {
		serviceInterface = sm.config.Interface
	}

	// Generate new Virtual IP configuration
	newVip := kubevip.Config{
		VIP:        newServiceAddress, //TODO support more than one vip?
		Interface:  serviceInterface,
		SingleNode: true,
		EnableARP:  sm.config.EnableARP,
		EnableBGP:  sm.config.EnableBGP,
		VIPCIDR:    sm.config.VIPCIDR,
	}

	log.Debugf("foundInstance %t", foundInstance)

	// This instance wasn't found, we need to add it to the manager
	if !foundInstance {
		// Create new service
		var newService Instance
		newService.UID = newServiceUID
		newService.Vip = newServiceAddress
		newService.Type = string(service.Spec.Ports[0].Protocol) //TODO - support multiple port types
		newService.Port = service.Spec.Ports[0].Port
		newService.ServiceName = service.Name
		newService.dhcpInterfaceHwaddr = service.Annotations[hwAddrKey]
		newService.dhcpInterfaceIP = service.Annotations[requestedIP]

		// If this was purposely created with the address 0.0.0.0 then we will create a macvlan on the main interface and try DHCP
		if newServiceAddress == "0.0.0.0" {
			err := sm.createDHCPService(newServiceUID, &newVip, &newService, service)
			if err != nil {
				return err
			}
			return nil
		}

		log.Infof("New VIP [%s] for [%s/%s] ", newService.Vip, newService.ServiceName, newService.UID)

		// Generate Load Balancer config
		newLB := kubevip.LoadBalancer{
			Name:      fmt.Sprintf("%s-load-balancer", newService.ServiceName),
			Port:      int(newService.Port),
			Type:      newService.Type,
			BindToVip: true,
		}

		// Add Load Balancer Configuration
		newVip.LoadBalancers = append(newVip.LoadBalancers, newLB)

		// Create Add configuration to the new service
		newService.vipConfig = newVip

		// TODO - start VIP
		c, err := cluster.InitCluster(&newService.vipConfig, false)
		if err != nil {
			log.Errorf("Failed to add Service [%s] / [%s]", newService.ServiceName, newService.UID)
			return err
		}
		err = c.StartLoadBalancerService(&newService.vipConfig, sm.bgpServer)
		if err != nil {
			log.Errorf("Failed to add Service [%s] / [%s]", newService.ServiceName, newService.UID)
			return err
		}

		sm.upnpMap(newService)

		newService.cluster = *c

		// Begin watching this service
		// TODO - we may need this
		// go sm.serviceWatcher(&newService, sm.config.Namespace)

		update := func() error {
			// Update the "Status" of the LoadBalancer (one or many may do this), as long as one does it
			service.Status.LoadBalancer.Ingress = []v1.LoadBalancerIngress{{IP: newVip.VIP}}
			_, err = sm.clientSet.CoreV1().Services(service.Namespace).UpdateStatus(context.TODO(), service, metav1.UpdateOptions{})
			return err
		}

		err = update()

		if err != nil {
			log.Debugf("Error updating Service [%s] Status: %v", newService.ServiceName, err)
			if errors.IsConflict(err) {
				err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
					// reload service because we had a conflict
					service, _ = sm.clientSet.CoreV1().Services(service.Namespace).Get(context.TODO(), service.Name, metav1.GetOptions{})
					return update()
				})
				if err != nil {
					log.Errorf("Error updating Service [%s] Status: %v", newService.ServiceName, err)
				}
			}
		}

		sm.serviceInstances = append(sm.serviceInstances, newService)
	}

	log.Debugf("[COMPLETE] Service Sync")

	return nil
}

func (sm *Manager) upnpMap(s Instance) {
	// If upnp is enabled then update the gateway/router with the address
	// TODO - work out if we need to mapping.Reclaim()
	if sm.upnp != nil {

		log.Infof("[UPNP] Adding map to [%s:%d - %s]", s.Vip, s.Port, s.ServiceName)
		if err := sm.upnp.AddPortMapping(int(s.Port), int(s.Port), 0, s.Vip, strings.ToUpper(s.Type), s.ServiceName); err == nil {
			log.Infof("Service should be accessible externally on port [%d]", s.Port)
		} else {
			sm.upnp.Reclaim()
			log.Errorf("Unable to map port to gateway [%s]", err.Error())
		}
	}
}
