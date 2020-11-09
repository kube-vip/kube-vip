package service

import (
	"context"
	"fmt"
	"net"

	dhclient "github.com/digineo/go-dhclient"
	"github.com/plunder-app/kube-vip/pkg/cluster"
	"github.com/plunder-app/kube-vip/pkg/kubevip"
	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (sm *Manager) stopService(uid string) error {
	found := false
	for x := range sm.serviceInstances {
		if sm.serviceInstances[x].service.UID == uid {
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
	var updatedInstances []serviceInstance
	found := false
	for x := range sm.serviceInstances {
		// Add the running services to the new array
		if sm.serviceInstances[x].service.UID != uid {
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
		}
	}
	// If we've been through all services and not found the correct one then error
	if found == false {
		return fmt.Errorf("Unable to find/stop service [%s]", uid)
	}

	// Update the service array
	sm.serviceInstances = updatedInstances

	log.Debugf("Removed [%s] from manager, [%d] services remain", uid, len(sm.serviceInstances))

	return nil
}

func (sm *Manager) syncServices(s *plndrServices) error {
	log.Debugf("[STARTING] Service Sync")
	// Iterate through the synchronising services
	for x := range s.Services {
		foundInstance := false
		for y := range sm.serviceInstances {
			if s.Services[x].UID == sm.serviceInstances[y].service.UID {
				// We have found this instance in the manager and we can update it
				foundInstance = true
			}
		}

		// Generate new Virtual IP configuration
		newVip := kubevip.Config{
			VIP:           s.Services[x].Vip,
			Interface:     Interface,
			SingleNode:    true,
			GratuitousARP: EnableArp,
		}

		// This instance wasn't found, we need to add it to the manager
		if foundInstance == false {
			// Create new service
			var newService serviceInstance

			// If this was purposely created with the address 0.0.0.0 then we will create a macvlan on the main interface and try DHCP
			if s.Services[x].Vip == "0.0.0.0" {

				parent, err := netlink.LinkByName(Interface)
				if err != nil {
					return fmt.Errorf("Error finding VIP Interface, for building DHCP Link : %v", err)
				}

				// Create macvlan

				// Generate name from UID
				interfaceName := fmt.Sprintf("vip-%s", s.Services[x].UID[0:8])

				mac := &netlink.Macvlan{LinkAttrs: netlink.LinkAttrs{Name: interfaceName, ParentIndex: parent.Attrs().Index}, Mode: netlink.MACVLAN_MODE_BRIDGE}
				err = netlink.LinkAdd(mac)
				if err != nil {
					return fmt.Errorf("Could not add %s: %v", interfaceName, err)
				}

				err = netlink.LinkSetUp(mac)
				if err != nil {
					return fmt.Errorf("Could not bring up interface [%s] : %v", interfaceName, err)
				}

				iface, err := net.InterfaceByName(interfaceName)
				if err != nil {
					return fmt.Errorf("Error finding new DHCP interface by name [%v]", err)
				}

				client := dhclient.Client{
					Iface: iface,
					OnBound: func(lease *dhclient.Lease) {

						// Set VIP to Address from lease
						newVip.VIP = lease.FixedAddress.String()
						log.Infof("New VIP [%s] for [%s/%s] ", newVip.VIP, s.Services[x].ServiceName, s.Services[x].UID)

						// Generate Load Balancer configu
						newLB := kubevip.LoadBalancer{
							Name:      fmt.Sprintf("%s-load-balancer", s.Services[x].ServiceName),
							Port:      s.Services[x].Port,
							Type:      s.Services[x].Type,
							BindToVip: true,
						}

						// Add Load Balancer Configuration
						newVip.LoadBalancers = append(newVip.LoadBalancers, newLB)

						// Create Add configuration to the new service
						newService.vipConfig = newVip
						newService.service = s.Services[x]

						// TODO - start VIP
						c, err := cluster.InitCluster(&newService.vipConfig, false)
						if err != nil {
							log.Errorf("Failed to add Service [%s] / [%s]", newService.service.ServiceName, newService.service.UID)
							//return err
						}
						err = c.StartLoadBalancerService(&newService.vipConfig, false)
						if err != nil {
							log.Errorf("Failed to add Service [%s] / [%s]", newService.service.ServiceName, newService.service.UID)
							//return err
						}
						newService.cluster = *c

						// Begin watching endpoints for this service
						go sm.newWatcher(&newService)

						// Add new service to manager configuration
						sm.serviceInstances = append(sm.serviceInstances, newService)

						// Update the service
						// listOptions := metav1.ListOptions{
						// 	FieldSelector: fmt.Sprintf("metadata.uid=%s", newService.service.UID),
						// }
						ns, err := returnNameSpace()
						if err != nil {
							log.Errorf("Error finding Namespace")
							return
						}
						dhcpService, err := sm.clientSet.CoreV1().Services(ns).Get(context.TODO(), newService.service.ServiceName, metav1.GetOptions{})
						if err != nil {
							log.Errorf("Error finding Service [%s] : %v", newService.service.ServiceName, err)
							return
						}
						dhcpService.Spec.LoadBalancerIP = newVip.VIP
						_, err = sm.clientSet.CoreV1().Services(ns).Update(context.TODO(), dhcpService, metav1.UpdateOptions{})
						if err != nil {
							log.Errorf("Error updating Service [%s] : %v", newService.service.ServiceName, err)
							return
						}
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

			log.Infof("New VIP [%s] for [%s/%s] ", s.Services[x].Vip, s.Services[x].ServiceName, s.Services[x].UID)

			// Generate Load Balancer configu
			newLB := kubevip.LoadBalancer{
				Name:      fmt.Sprintf("%s-load-balancer", s.Services[x].ServiceName),
				Port:      s.Services[x].Port,
				Type:      s.Services[x].Type,
				BindToVip: true,
			}

			// Add Load Balancer Configuration
			newVip.LoadBalancers = append(newVip.LoadBalancers, newLB)

			// Create Add configuration to the new service
			newService.vipConfig = newVip
			newService.service = s.Services[x]

			// TODO - start VIP
			c, err := cluster.InitCluster(&newService.vipConfig, false)
			if err != nil {
				log.Errorf("Failed to add Service [%s] / [%s]", s.Services[x].ServiceName, s.Services[x].UID)
				return err
			}
			err = c.StartLoadBalancerService(&newService.vipConfig, false)
			if err != nil {
				log.Errorf("Failed to add Service [%s] / [%s]", s.Services[x].ServiceName, s.Services[x].UID)
				return err
			}
			newService.cluster = *c

			// Begin watching endpoints for this service
			go sm.newWatcher(&newService)

			// Add new service to manager configuration
			sm.serviceInstances = append(sm.serviceInstances, newService)
		}
	}
	log.Debugf("[COMPLETE] Service Sync")

	return nil
}
