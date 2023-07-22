package manager

import (
	"fmt"
	"net"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
	v1 "k8s.io/api/core/v1"

	"github.com/kube-vip/kube-vip/pkg/cluster"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/vip"
)

const dhcpTimeout = 10 * time.Second

// Instance defines an instance of everything needed to manage a vip
type Instance struct {
	// Virtual IP / Load Balancer configuration
	vipConfig *kubevip.Config

	// cluster instance
	cluster *cluster.Cluster

	// Service uses DHCP
	isDHCP              bool
	dhcpInterface       string
	dhcpInterfaceHwaddr string
	dhcpInterfaceIP     string
	dhcpClient          *vip.DHCPClient

	// Kubernetes service mapping
	Vip  string
	Port int32
	UID  string
	Type string

	serviceSnapshot *v1.Service
}

func NewInstance(svc *v1.Service, config *kubevip.Config) (*Instance, error) {
	instanceAddress := fetchServiceAddress(svc)
	instanceUID := string(svc.UID)

	// Detect if we're using a specific interface for services
	var serviceInterface string
	if config.ServicesInterface != "" {
		serviceInterface = config.ServicesInterface
	} else {
		serviceInterface = config.Interface
	}

	// Generate new Virtual IP configuration
	newVip := &kubevip.Config{
		VIP:                   instanceAddress, // TODO support more than one vip?
		Interface:             serviceInterface,
		SingleNode:            true,
		EnableARP:             config.EnableARP,
		EnableBGP:             config.EnableBGP,
		VIPCIDR:               config.VIPCIDR,
		VIPSubnet:             config.VIPSubnet,
		EnableRoutingTable:    config.EnableRoutingTable,
		RoutingTableID:        config.RoutingTableID,
		ArpBroadcastRate:      config.ArpBroadcastRate,
		EnableServiceSecurity: config.EnableServiceSecurity,
	}

	// Create new service
	instance := &Instance{
		UID:             instanceUID,
		Vip:             instanceAddress,
		serviceSnapshot: svc,
	}
	if len(svc.Spec.Ports) > 0 {
		instance.Type = string(svc.Spec.Ports[0].Protocol)
		instance.Port = svc.Spec.Ports[0].Port
	}

	if svc.Annotations != nil {
		instance.dhcpInterfaceHwaddr = svc.Annotations[hwAddrKey]
		instance.dhcpInterfaceIP = svc.Annotations[requestedIP]
	}

	// Generate Load Balancer config
	newLB := kubevip.LoadBalancer{
		Name:      fmt.Sprintf("%s-load-balancer", svc.Name),
		Port:      int(instance.Port),
		Type:      instance.Type,
		BindToVip: true,
	}
	// Add Load Balancer Configuration
	newVip.LoadBalancers = append(newVip.LoadBalancers, newLB)
	// Create Add configuration to the new service
	instance.vipConfig = newVip

	// If this was purposely created with the address 0.0.0.0,
	// we will create a macvlan on the main interface and a DHCP client
	if instanceAddress == "0.0.0.0" {
		err := instance.startDHCP()
		if err != nil {
			return nil, err
		}
		select {
		case err := <-instance.dhcpClient.ErrorChannel():
			return nil, fmt.Errorf("error starting DHCP for %s/%s: error: %s",
				instance.serviceSnapshot.Namespace, instance.serviceSnapshot.Name, err)
		case ip := <-instance.dhcpClient.IPChannel():
			instance.vipConfig.VIP = ip
			instance.dhcpInterfaceIP = ip
		}
	}

	c, err := cluster.InitCluster(instance.vipConfig, false)
	if err != nil {
		log.Errorf("Failed to add Service %s/%s", svc.Namespace, svc.Name)
		return nil, err
	}
	c.Network.SetServicePorts(svc)
	instance.cluster = c

	return instance, nil
}

func (i *Instance) startDHCP() error {
	parent, err := netlink.LinkByName(i.vipConfig.Interface)
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
