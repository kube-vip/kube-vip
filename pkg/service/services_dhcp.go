package service

import (
	"context"
	"fmt"
	"net"

	"github.com/plunder-app/kube-vip/pkg/cluster"
	"github.com/plunder-app/kube-vip/pkg/kubevip"
	log "github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/client4"
	"github.com/insomniacslk/dhcp/dhcpv6"
	"github.com/insomniacslk/dhcp/dhcpv6/client6"
	"github.com/insomniacslk/dhcp/netboot"
	"github.com/vishvananda/netlink"
)

func dhclient6(ifname string, attempts int, verbose bool) (*netboot.BootConf, error) {
	if attempts < 1 {
		attempts = 1
	}
	llAddr, err := dhcpv6.GetLinkLocalAddr(ifname)
	if err != nil {
		return nil, err
	}
	laddr := net.UDPAddr{
		IP:   llAddr,
		Port: dhcpv6.DefaultClientPort,
		Zone: ifname,
	}
	raddr := net.UDPAddr{
		IP:   dhcpv6.AllDHCPRelayAgentsAndServers,
		Port: dhcpv6.DefaultServerPort,
		Zone: ifname,
	}
	c := client6.NewClient()
	c.LocalAddr = &laddr
	c.RemoteAddr = &raddr
	var conv []dhcpv6.DHCPv6
	for attempt := 0; attempt < attempts; attempt++ {
		log.Printf("Attempt %d of %d", attempt+1, attempts)
		conv, err = c.Exchange(ifname, dhcpv6.WithNetboot)
		if err != nil && attempt < attempts {
			log.Printf("Error: %v", err)
			continue
		}
		break
	}
	if verbose {
		for _, m := range conv {
			log.Print(m.Summary())
		}
	}
	if err != nil {
		return nil, err
	}
	// extract the network configuration
	netconf, err := netboot.ConversationToNetconf(conv)
	return netconf, err
}

func dhclient4(ifname string, attempts int, verbose bool) (*netboot.BootConf, error) {
	if attempts < 1 {
		attempts = 1
	}
	client := client4.NewClient()
	var (
		conv []*dhcpv4.DHCPv4
		err  error
	)
	for attempt := 0; attempt < attempts; attempt++ {
		log.Printf("Attempt %d of %d", attempt+1, attempts)
		conv, err = client.Exchange(ifname)
		if err != nil && attempt < attempts {
			log.Printf("Error: %v", err)
			continue
		}
		break
	}
	if verbose {
		for _, m := range conv {
			log.Print(m.Summary())
		}
	}
	if err != nil {
		return nil, err
	}
	// extract the network configuration
	netconf, err := netboot.ConversationToNetconfv4(conv)
	return netconf, err
}

func (sm *Manager) createDHCPService(newServiceUID string, newVip *kubevip.Config, newService *Instance, service *v1.Service) error {
	parent, err := netlink.LinkByName(sm.config.Interface)
	if err != nil {
		return fmt.Errorf("Error finding VIP Interface, for building DHCP Link : %v", err)
	}

	// Create macvlan

	// Generate name from UID
	interfaceName := fmt.Sprintf("vip-%s", newServiceUID[0:8])

	// Check if the interface doesn't exist first
	_, err = net.InterfaceByName(interfaceName)
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
		_, err = net.InterfaceByName(interfaceName)
		if err != nil {
			return fmt.Errorf("Error finding new DHCP interface by name [%v]", err)
		}
	} else {
		log.Infof("Using existing macvlan interface for DHCP [%s]", interfaceName)
	}
	var bootconf *netboot.BootConf

	bootconf, err = dhclient4(interfaceName, 5, false)
	if err != nil {
		return err
	}
	err = netboot.ConfigureInterface(interfaceName, &bootconf.NetConf)
	if err != nil {
		return err
	}

	newVip.VIP = bootconf.Addresses[0].IPNet.IP.String()
	log.Infof("DHCP VIP [%s] for [%s/%s] ", newVip.VIP, newService.ServiceName, newServiceUID)

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
	newService.vipConfig = *newVip

	// TODO - start VIP
	c, err := cluster.InitCluster(&newService.vipConfig, false)
	if err != nil {
		log.Errorf("Failed to add Service [%s] / [%s]: %v", newService.ServiceName, newService.UID, err)
		return err
	}
	err = c.StartLoadBalancerService(&newService.vipConfig, sm.bgpServer)
	if err != nil {
		log.Errorf("Failed to add Load Balabcer service Service [%s] / [%s]: %v", newService.ServiceName, newService.UID, err)
		return err
	}
	newService.cluster = *c

	// Set that DHCP is enabled
	newService.isDHCP = true
	// Set the name of the interface so that it can be removed on Service deletion
	newService.dhcpInterface = interfaceName

	// Add new service to manager configuration
	service.Spec.LoadBalancerIP = newVip.VIP
	log.Infof("Updating service [%s], with load balancer address [%s]", service.Name, service.Spec.LoadBalancerIP)
	sm.serviceInstances = append(sm.serviceInstances, *newService)

	retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Retrieve the latest version of Deployment before attempting update
		// RetryOnConflict uses exponential backoff to avoid exhausting the apiserver
		currentService, err := sm.clientSet.CoreV1().Services(service.Namespace).Get(context.TODO(), service.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		updatedService, err := sm.clientSet.CoreV1().Services(currentService.Namespace).Update(context.TODO(), currentService, metav1.UpdateOptions{})
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
		return retryErr
	}

	sm.upnpMap(*newService)
	return nil
}
