package manager

import (
	"fmt"
	"net"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4/nclient4"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/vip"
	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
	v1 "k8s.io/api/core/v1"

	"github.com/kube-vip/kube-vip/pkg/cluster"
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

	ServiceName      string
	ServiceNamespace string
}

func NewInstance(service *v1.Service, config *kubevip.Config) (*Instance, error) {
	instanceAddress := service.Spec.LoadBalancerIP
	instanceUID := string(service.UID)

	// Detect if we're using a specific interface for services
	var serviceInterface string
	if config.ServicesInterface != "" {
		serviceInterface = config.ServicesInterface
	} else {
		serviceInterface = config.Interface
	}

	// Generate new Virtual IP configuration
	newVip := &kubevip.Config{
		VIP:                instanceAddress, //TODO support more than one vip?
		Interface:          serviceInterface,
		SingleNode:         true,
		EnableARP:          config.EnableARP,
		EnableBGP:          config.EnableBGP,
		VIPCIDR:            config.VIPCIDR,
		VIPSubnet:          config.VIPSubnet,
		EnableRoutingTable: config.EnableRoutingTable,
		RoutingTableID:     config.RoutingTableID,
	}

	// Create new service
	instance := &Instance{
		UID:              instanceUID,
		Vip:              instanceAddress,
		ServiceName:      service.Name,
		ServiceNamespace: service.Namespace,
	}
	if len(service.Spec.Ports) > 0 {
		instance.Type = string(service.Spec.Ports[0].Protocol)
		instance.Port = service.Spec.Ports[0].Port
	}
	if service.Annotations != nil {
		instance.dhcpInterfaceHwaddr = service.Annotations[hwAddrKey]
		instance.dhcpInterfaceIP = service.Annotations[requestedIP]
	}

	// Generate Load Balancer config
	newLB := kubevip.LoadBalancer{
		Name:      fmt.Sprintf("%s-load-balancer", instance.ServiceName),
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
		ipChan, err := instance.startDHCP()
		if err != nil {
			return nil, err
		}
		select {
		case <-time.After(dhcpTimeout):
			return nil, fmt.Errorf("timeout to request the IP from DHCP server for service %s/%s",
				instance.ServiceNamespace, instance.ServiceName)
		case ip := <-ipChan:
			instance.vipConfig.VIP = ip
			instance.dhcpInterfaceIP = ip
		}
	}

	c, err := cluster.InitCluster(instance.vipConfig, false)
	if err != nil {
		log.Errorf("Failed to add Service %s/%s", instance.ServiceNamespace, instance.ServiceName)
		return nil, err
	}
	instance.cluster = c

	return instance, nil
}

func (i *Instance) startDHCP() (chan string, error) {
	parent, err := netlink.LinkByName(i.vipConfig.Interface)
	if err != nil {
		return nil, fmt.Errorf("error finding VIP Interface, for building DHCP Link : %v", err)
	}

	// Generate name from UID
	interfaceName := fmt.Sprintf("vip-%s", i.UID[0:8])

	// Check if the interface doesn't exist first
	iface, err := net.InterfaceByName(interfaceName)
	if err != nil {
		log.Infof("Creating new macvlan interface for DHCP [%s]", interfaceName)

		hwaddr, err := net.ParseMAC(i.dhcpInterfaceHwaddr)
		if i.dhcpInterfaceHwaddr != "" && err != nil {
			return nil, err
		} else if hwaddr == nil {
			hwaddr, err = net.ParseMAC(vip.GenerateMac())
			if err != nil {
				return nil, err
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
			return nil, fmt.Errorf("could not add %s: %v", interfaceName, err)
		}

		err = netlink.LinkSetUp(mac)
		if err != nil {
			return nil, fmt.Errorf("could not bring up interface [%s] : %v", interfaceName, err)
		}

		iface, err = net.InterfaceByName(interfaceName)
		if err != nil {
			return nil, fmt.Errorf("error finding new DHCP interface by name [%v]", err)
		}
	} else {
		log.Infof("Using existing macvlan interface for DHCP [%s]", interfaceName)
	}

	var initRebootFlag bool
	if i.dhcpInterfaceIP != "" {
		initRebootFlag = true
	}

	ipChan := make(chan string)

	client := vip.NewDHCPClient(iface, initRebootFlag, i.dhcpInterfaceIP, func(lease *nclient4.Lease) {
		ipChan <- lease.ACK.YourIPAddr.String()

		log.Infof("DHCP VIP [%s] for [%s/%s] ", i.vipConfig.VIP, i.ServiceNamespace, i.ServiceName)
	})

	go client.Start()

	// Set that DHCP is enabled
	i.isDHCP = true
	// Set the name of the interface so that it can be removed on Service deletion
	i.dhcpInterface = interfaceName
	i.dhcpInterfaceHwaddr = iface.HardwareAddr.String()
	// Add the client so that we can call it to stop function
	i.dhcpClient = client

	return ipChan, nil
}
