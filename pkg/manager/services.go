package manager

import (
	"context"
	"fmt"
	"net"
	"strings"

	dhclient "github.com/digineo/go-dhclient"
	"github.com/plunder-app/kube-vip/pkg/cluster"
	"github.com/plunder-app/kube-vip/pkg/kubevip"
	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (sm *Manager) stopService(uid string) error {
	found := false
	for x := range sm.serviceInstances {
		if sm.serviceInstances[x].UID == uid {
			found = true
			sm.serviceInstances[x].cluster.Stop()
		}
	}
	if found == false {
		return fmt.Errorf("Unable to find/stop service [%s]", uid)
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
			if sm.serviceInstances[x].dhcp != nil {
				sm.serviceInstances[x].dhcp.dhcpClient.Stop()
				macvlan, err := netlink.LinkByName(sm.serviceInstances[x].dhcp.dhcpInterface)
				if err != nil {
					return fmt.Errorf("Error finding VIP Interface, for building DHCP Link : %v", err)
				}
				netlink.LinkDel(macvlan)
			}
			if sm.serviceInstances[x].vipConfig.EnableBGP {
				cidrVip := fmt.Sprintf("%s/%s", sm.serviceInstances[x].vipConfig.VIP, sm.serviceInstances[x].vipConfig.VIPCIDR)
				err := sm.bgpServer.DelHost(cidrVip)
				return err
			}
		}
	}
	// If we've been through all services and not found the correct one then error
	if found == false {
		return fmt.Errorf("Unable to find/stop service [%s]", uid)
	}

	// Update the service array
	sm.serviceInstances = updatedInstances

	log.Infof("Removed [%s] from manager, [%d] advertised services remain", uid, len(sm.serviceInstances))

	return nil
}

func (sm *Manager) syncServices(service *v1.Service) error {
	log.Debugf("[STARTING] Service Sync")
	// Iterate through the synchronising services
	foundInstance := false
	newServiceAddress := service.Spec.LoadBalancerIP
	newServiceUID := string(service.UID)

	for x := range sm.serviceInstances {
		if sm.serviceInstances[x].UID == newServiceUID {
			// We have found this instance in the manager, we can determine if it needs updating
			foundInstance = true
		}

	}

	// Generate new Virtual IP configuration
	newVip := kubevip.Config{
		VIP:        newServiceAddress, //TODO support more than one vip?
		Interface:  sm.config.Interface,
		SingleNode: true,
		EnableARP:  sm.config.EnableARP,
		EnableBGP:  sm.config.EnableBGP,
		VIPCIDR:    sm.config.VIPCIDR,
	}

	// This instance wasn't found, we need to add it to the manager
	if foundInstance == false {
		// Create new service
		var newService Instance
		newService.UID = newServiceUID
		newService.Vip = newServiceAddress
		newService.Type = string(service.Spec.Ports[0].Protocol) //TODO - support multiple port types
		newService.Port = service.Spec.Ports[0].Port
		newService.ServiceName = service.Name

		// If this was purposely created with the address 0.0.0.0 then we will create a macvlan on the main interface and try DHCP
		if newServiceAddress == "0.0.0.0" {

			parent, err := netlink.LinkByName(sm.config.Interface)
			if err != nil {
				return fmt.Errorf("Error finding VIP Interface, for building DHCP Link : %v", err)
			}

			// Create macvlan

			// Generate name from UID
			interfaceName := fmt.Sprintf("vip-%s", newServiceUID[0:8])

			// Check if the interface doesn't exist first
			iface, err := net.InterfaceByName(interfaceName)
			if err != nil {
				log.Infof("Creating new macvlan interface for DHCP [%s]", interfaceName)

				mac := &netlink.Macvlan{LinkAttrs: netlink.LinkAttrs{Name: interfaceName, ParentIndex: parent.Attrs().Index}, Mode: netlink.MACVLAN_MODE_BRIDGE}

				err = netlink.LinkAdd(mac)
				if err != nil {
					return fmt.Errorf("Could not add %s: %v", interfaceName, err)
				}

				err = netlink.LinkSetUp(mac)
				if err != nil {
					return fmt.Errorf("Could not bring up interface [%s] : %v", interfaceName, err)
				}
				iface, err = net.InterfaceByName(interfaceName)
				if err != nil {
					return fmt.Errorf("Error finding new DHCP interface by name [%v]", err)
				}
			} else {
				log.Infof("Using existing macvlan interface for DHCP [%s]", interfaceName)

			}

			client := dhclient.Client{
				Iface: iface,
				OnBound: func(lease *dhclient.Lease) {

					// Set VIP to Address from lease
					newVip.VIP = lease.FixedAddress.String()
					log.Infof("DHCP VIP [%s] for [%s/%s] ", newVip.VIP, service.Name, newServiceUID)

					// // Generate Load Balancer config
					// newLB := kubevip.LoadBalancer{
					// 	Name:      fmt.Sprintf("%s-load-balancer", s.Services[x].ServiceName),
					// 	Port:      s.Services[x].Port,
					// 	Type:      s.Services[x].Type,
					// 	BindToVip: true,
					// }

					// // Add Load Balancer Configuration
					// newVip.LoadBalancers = append(newVip.LoadBalancers, newLB)

					// Create Add configuration to the new service
					newService.vipConfig = newVip

					// TODO - start VIP
					c, err := cluster.InitCluster(&newService.vipConfig, false)
					if err != nil {
						log.Errorf("Failed to add Service [%s] / [%s]", newService.ServiceName, newService.UID)
						//return err
					}
					err = c.StartLoadBalancerService(&newService.vipConfig, sm.bgpServer)
					if err != nil {
						log.Errorf("Failed to add Service [%s] / [%s]", newService.ServiceName, newService.UID)
						//return err
					}
					newService.cluster = *c

					// Begin watching this service
					// TODO - we may need this
					// go sm.serviceWatcher(&newService, sm.config.Namespace)

					// Add new service to manager configuration
					sm.serviceInstances = append(sm.serviceInstances, newService)

					// Update the service
					// ns, err := returnNameSpace()
					// if err != nil {
					// 	log.Errorf("Error finding Namespace")
					// 	return
					// }
					// dhcpService, err := sm.clientSet.CoreV1().Services(ns).Get(context.TODO(), newService.ServiceName, metav1.GetOptions{})
					// if err != nil {
					// 	log.Errorf("Error finding Service [%s] : %v", newService.ServiceName, err)
					// 	return
					// }

					// Update the service with DHCP information
					service.Spec.LoadBalancerIP = newVip.VIP
					updatedService, err := sm.clientSet.CoreV1().Services(service.Namespace).Update(context.TODO(), service, metav1.UpdateOptions{})
					log.Infof("Updating service [%s], with load balancer address [%s]", updatedService.Name, updatedService.Spec.LoadBalancerIP)
					if err != nil {
						log.Errorf("Error updating Service Spec [%s] : %v", newService.ServiceName, err)
						return
					}
					updatedService.Status.LoadBalancer.Ingress = []v1.LoadBalancerIngress{{IP: newVip.VIP}}
					_, err = sm.clientSet.CoreV1().Services(updatedService.Namespace).UpdateStatus(context.TODO(), updatedService, metav1.UpdateOptions{})
					if err != nil {
						log.Errorf("Error updating Service [%s] Status: %v", newService.ServiceName, err)
						return
					}
					sm.upnpMap(newService)

				},
			}

			newService.dhcp = &dhcpService{
				dhcpClient:    &client,
				dhcpInterface: interfaceName,
			}

			// Start the DHCP Client
			newService.dhcp.dhcpClient.Start()

			// Change the interface name to our new DHCP macvlan interface
			newVip.Interface = interfaceName
			log.Infof("DHCP Interface and Client is up and active [%s]", interfaceName)
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

		// Update the "Status" of the LoadBalancer (one or many may do this), as long as one does it
		service.Status.LoadBalancer.Ingress = []v1.LoadBalancerIngress{{IP: newVip.VIP}}
		_, err = sm.clientSet.CoreV1().Services(service.Namespace).UpdateStatus(context.TODO(), service, metav1.UpdateOptions{})
		if err != nil {
			log.Errorf("Error updating Service [%s] Status: %v", newService.ServiceName, err)
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
