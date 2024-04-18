package manager

import (
	"fmt"
	"net"

	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
	v1 "k8s.io/api/core/v1"

	"github.com/kube-vip/kube-vip/pkg/cluster"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/vip"
)

// Instance defines an instance of everything needed to manage vips
type Instance struct {
	// Virtual IP / Load Balancer configuration
	vipConfigs []*kubevip.Config

	// cluster instances
	clusters []*cluster.Cluster

	// Service uses DHCP
	isDHCP              bool
	dhcpInterface       string
	dhcpInterfaceHwaddr string
	dhcpInterfaceIP     string
	dhcpHostname        string
	dhcpClient          *vip.DHCPClient

	// Kubernetes service mapping
	VIPs []string
	Port int32
	UID  string
	Type string

	serviceSnapshot *v1.Service
}

func NewInstance(svc *v1.Service, config *kubevip.Config) (*Instance, error) {
	instanceAddresses := fetchServiceAddresses(svc)
	instanceUID := string(svc.UID)

	// Detect if we're using a specific interface for services
	var svcInterface string
	svcInterface = svc.Annotations[serviceInterface] // If the service has a specific interface defined, then use it

	// If it is still blank then use the
	if svcInterface == "" {
		if config.ServicesInterface != "" {
			svcInterface = config.ServicesInterface
		} else {
			svcInterface = config.Interface
		}
	}
	var newVips []*kubevip.Config

	for _, address := range instanceAddresses {
		// Generate new Virtual IP configuration
		newVips = append(newVips, &kubevip.Config{
			VIP:                    address,
			Interface:              svcInterface,
			SingleNode:             true,
			EnableARP:              config.EnableARP,
			EnableBGP:              config.EnableBGP,
			VIPCIDR:                config.VIPCIDR,
			VIPSubnet:              config.VIPSubnet,
			EnableRoutingTable:     config.EnableRoutingTable,
			RoutingTableID:         config.RoutingTableID,
			RoutingTableType:       config.RoutingTableType,
			RoutingProtocol:        config.RoutingProtocol,
			ArpBroadcastRate:       config.ArpBroadcastRate,
			EnableServiceSecurity:  config.EnableServiceSecurity,
			DNSMode:                config.DNSMode,
			DisableServiceUpdates:  config.DisableServiceUpdates,
			EnableServicesElection: config.EnableServicesElection,
			KubernetesLeaderElection: kubevip.KubernetesLeaderElection{
				EnableLeaderElection: config.EnableLeaderElection,
			},
		})
	}

	// Create new service
	instance := &Instance{
		UID:             instanceUID,
		VIPs:            instanceAddresses,
		serviceSnapshot: svc,
	}
	if len(svc.Spec.Ports) > 0 {
		instance.Type = string(svc.Spec.Ports[0].Protocol)
		instance.Port = svc.Spec.Ports[0].Port
	}

	if svc.Annotations != nil {
		instance.dhcpInterfaceHwaddr = svc.Annotations[hwAddrKey]
		instance.dhcpInterfaceIP = svc.Annotations[requestedIP]
		instance.dhcpHostname = svc.Annotations[loadbalancerHostname]
	}

	// Generate Load Balancer config
	newLB := kubevip.LoadBalancer{
		Name:      fmt.Sprintf("%s-load-balancer", svc.Name),
		Port:      int(instance.Port),
		Type:      instance.Type,
		BindToVip: true,
	}
	for _, vip := range newVips {
		// Add Load Balancer Configuration
		vip.LoadBalancers = append(vip.LoadBalancers, newLB)
	}
	// Create Add configuration to the new service
	instance.vipConfigs = newVips

	// If this was purposely created with the address 0.0.0.0,
	// we will create a macvlan on the main interface and a DHCP client
	// TODO: Consider how best to handle DHCP with multiple addresses
	if len(instanceAddresses) == 1 && instanceAddresses[0] == "0.0.0.0" {
		err := instance.startDHCP()
		if err != nil {
			return nil, err
		}
		select {
		case err := <-instance.dhcpClient.ErrorChannel():
			return nil, fmt.Errorf("error starting DHCP for %s/%s: error: %s",
				instance.serviceSnapshot.Namespace, instance.serviceSnapshot.Name, err)
		case ip := <-instance.dhcpClient.IPChannel():
			instance.vipConfigs[0].VIP = ip
			instance.dhcpInterfaceIP = ip
		}
	}
	for _, vipConfig := range instance.vipConfigs {
		c, err := cluster.InitCluster(vipConfig, false)
		if err != nil {
			log.Errorf("Failed to add Service %s/%s", svc.Namespace, svc.Name)
			return nil, err
		}

		for i := range c.Network {
			c.Network[i].SetServicePorts(svc)
		}

		instance.clusters = append(instance.clusters, c)
		log.Infof("(svcs) adding VIP [%s] via %s for [%s/%s]", vipConfig.VIP, vipConfig.Interface, svc.Namespace, svc.Name)

	}

	return instance, nil
}

func (i *Instance) startDHCP() error {
	if len(i.vipConfigs) != 1 {
		return fmt.Errorf("DHCP requires exactly 1 VIP config, got: %v", len(i.vipConfigs))
	}
	parent, err := netlink.LinkByName(i.vipConfigs[0].Interface)
	if err != nil {
		return fmt.Errorf("error finding VIP Interface, for building DHCP Link : %v", err)
	}

	// Generate name from UID
	interfaceName := fmt.Sprintf("vip-%s", i.UID[0:8])

	// Check if the interface doesn't exist first
	iface, err := net.InterfaceByName(interfaceName)
	if err != nil {
		log.Infof("Creating new macvlan interface for DHCP [%s]", interfaceName)

		hwaddr, err := net.ParseMAC(i.dhcpInterfaceHwaddr)
		if i.dhcpInterfaceHwaddr != "" && err != nil {
			return err
		} else if hwaddr == nil {
			hwaddr, err = net.ParseMAC(vip.GenerateMac())
			if err != nil {
				return err
			}
		}

		log.Infof("New interface [%s] mac is %s", interfaceName, hwaddr)
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
			return fmt.Errorf("could not add %s: %v", interfaceName, err)
		}

		err = netlink.LinkSetUp(mac)
		if err != nil {
			return fmt.Errorf("could not bring up interface [%s] : %v", interfaceName, err)
		}

		iface, err = net.InterfaceByName(interfaceName)
		if err != nil {
			return fmt.Errorf("error finding new DHCP interface by name [%v]", err)
		}
	} else {
		log.Infof("Using existing macvlan interface for DHCP [%s]", interfaceName)
	}

	var initRebootFlag bool
	if i.dhcpInterfaceIP != "" {
		initRebootFlag = true
	}

	client := vip.NewDHCPClient(iface, initRebootFlag, i.dhcpInterfaceIP)

	// Add hostname to dhcp client if annotated
	if i.dhcpHostname != "" {
		log.Infof("Hostname specified for dhcp lease: [%s] - [%s]", interfaceName, i.dhcpHostname)
		client.WithHostName(i.dhcpHostname)
	}

	go client.Start()

	// Set that DHCP is enabled
	i.isDHCP = true
	// Set the name of the interface so that it can be removed on Service deletion
	i.dhcpInterface = interfaceName
	i.dhcpInterfaceHwaddr = iface.HardwareAddr.String()
	// Add the client so that we can call it to stop function
	i.dhcpClient = client

	return nil
}
