package manager

import (
	"context"
	"fmt"
	"net"

	"github.com/insomniacslk/dhcp/dhcpv4/nclient4"
	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"

	"github.com/kube-vip/kube-vip/pkg/cluster"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/vip"
)

func (sm *Manager) createDHCPService(newServiceUID string, newVip *kubevip.Config, newService *Instance, service *v1.Service) error {
	parent, err := netlink.LinkByName(sm.config.Interface)
	if err != nil {
		return fmt.Errorf("Error finding VIP Interface, for building DHCP Link : %v", err)
	}

	// Generate name from UID
	interfaceName := fmt.Sprintf("vip-%s", newServiceUID[0:8])

	// Check if the interface doesn't exist first
	iface, err := net.InterfaceByName(interfaceName)
	if err != nil {
		log.Infof("Creating new macvlan interface for DHCP [%s]", interfaceName)

		hwaddr, err := net.ParseMAC(newService.dhcpInterfaceHwaddr)
		if newService.dhcpInterfaceHwaddr != "" && err != nil {
			return err
		}

		mac := &netlink.Macvlan{
			LinkAttrs: netlink.LinkAttrs{
				Name:         interfaceName,
				ParentIndex:  parent.Attrs().Index,
				HardwareAddr: hwaddr,
			},
			Mode: netlink.MACVLAN_MODE_DEFAULT,
		}

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

	var initRebootFlag bool
	if newService.dhcpInterfaceHwaddr != "" {
		initRebootFlag = true
	}

	client := vip.NewDHCPClient(iface, initRebootFlag, newService.dhcpInterfaceIP, func(lease *nclient4.Lease) {
		newVip.VIP = lease.ACK.YourIPAddr.String()

		log.Infof("DHCP VIP [%s] for [%s/%s] ", newVip.VIP, newService.ServiceName, newServiceUID)

		// Create Add configuration to the new service
		newService.vipConfig = *newVip

		// TODO - start VIP
		c, err := cluster.InitCluster(&newService.vipConfig, false)
		if err != nil {
			log.Errorf("Failed to add Service [%s] / [%s]: %v", newService.ServiceName, newService.UID, err)
			return
		}
		err = c.StartLoadBalancerService(&newService.vipConfig, sm.bgpServer)
		if err != nil {
			log.Errorf("Failed to add Load Balancer service Service [%s] / [%s]: %v", newService.ServiceName, newService.UID, err)
			return
		}
		newService.cluster = *c

		retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			// Retrieve the latest version of Deployment before attempting update
			// RetryOnConflict uses exponential backoff to avoid exhausting the apiserver
			currentService, err := sm.clientSet.CoreV1().Services(service.Namespace).Get(context.TODO(), service.Name, metav1.GetOptions{})
			if err != nil {
				return err
			}

			currentServiceCopy := currentService.DeepCopy()
			if currentServiceCopy.Annotations == nil {
				currentServiceCopy.Annotations = make(map[string]string)
			}
			currentServiceCopy.Annotations[hwAddrKey] = iface.HardwareAddr.String()
			currentServiceCopy.Annotations[requestedIP] = newVip.VIP
			updatedService, err := sm.clientSet.CoreV1().Services(currentService.Namespace).Update(context.TODO(), currentServiceCopy, metav1.UpdateOptions{})
			if err != nil {
				log.Errorf("Error updating Service Spec [%s] : %v", newService.ServiceName, err)
				return err
			}

			updatedService.Status.LoadBalancer.Ingress = []v1.LoadBalancerIngress{{IP: newVip.VIP}}
			_, err = sm.clientSet.CoreV1().Services(updatedService.Namespace).UpdateStatus(context.TODO(), updatedService, metav1.UpdateOptions{})
			if err != nil {
				log.Errorf("Error updating Service [%s] Status: %v", newService.ServiceName, err)
				return err
			}
			return nil
		})

		if retryErr != nil {
			log.Errorf("Failed to set Services: %v", retryErr)
		}
		// Find an update our array

		for x := range sm.serviceInstances {
			if sm.serviceInstances[x].UID == newServiceUID {
				sm.serviceInstances[x] = *newService
			}
		}
		sm.upnpMap(*newService)
	})

	// Set that DHCP is enabled
	newService.isDHCP = true
	// Set the name of the interface so that it can be removed on Service deletion
	newService.dhcpInterface = interfaceName
	// Add the client so that we can call it's stop function
	newService.dhcpClient = client

	sm.serviceInstances = append(sm.serviceInstances, *newService)

	go client.Start()

	return nil
}
