package manager

import (
	"fmt"
	"net"
	"strconv"

	log "log/slog"

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
	HasEndpoints        bool

	// External Gateway IP the service is forwarded from
	upnpGatewayIPs []string

	// Kubernetes service mapping
	serviceSnapshot *v1.Service
}

type Port struct {
	Port uint16
	Type string
}

func NewInstance(svc *v1.Service, config *kubevip.Config) (*Instance, error) {
	instanceAddresses := fetchServiceAddresses(svc)
	//instanceUID := string(svc.UID)

	var newVips []*kubevip.Config
	var link netlink.Link
	var err error

	for _, address := range instanceAddresses {
		// Detect if we're using a specific interface for services
		var svcInterface string
		svcInterface = svc.Annotations[serviceInterface] // If the service has a specific interface defined, then use it
		if svcInterface == kubevip.Auto {
			link, err = autoFindInterface(address)
			if err != nil {
				log.Error("automatically discover network interface for annotated IP", "address", address, "err", err)
			} else {
				if link == nil {
					log.Error("automatically discover network interface for annotated IP address", "address", address)
				}
			}
			if link == nil {
				svcInterface = ""
			} else {
				svcInterface = getAutoInterfaceName(link, config.Interface)
			}
		}
		// If it is still blank then use the
		if svcInterface == "" {
			switch config.ServicesInterface {
			case kubevip.Auto:
				link, err = autoFindInterface(address)
				if err != nil {
					log.Error("failed to automatically discover network interface for address", "ip", address, "err", err, "interface", config.Interface)
				} else if link == nil {
					log.Error("failed to automatically discover network interface for address", "ip", address, "defaulting to", config.Interface)
				}
				svcInterface = getAutoInterfaceName(link, config.Interface)
			case "":
				svcInterface = config.Interface
			default:
				svcInterface = config.ServicesInterface
			}
		}

		if link == nil {
			if link, err = netlink.LinkByName(svcInterface); err != nil {
				return nil, fmt.Errorf("failed to get interface %s: %w", svcInterface, err)
			}
			if link == nil {
				return nil, fmt.Errorf("failed to get interface %s", svcInterface)
			}
		}

		cidrs := vip.Split(config.VIPSubnet)

		ipv4AutoSubnet := false
		ipv6AutoSubnet := false
		if cidrs[0] == kubevip.Auto {
			ipv4AutoSubnet = true
		}

		if len(cidrs) > 1 && cidrs[1] == kubevip.Auto {
			ipv6AutoSubnet = true
		}

		if (config.Address != "" || config.VIP != "") && (ipv4AutoSubnet || ipv6AutoSubnet) {
			return nil, fmt.Errorf("auto subnet discovery cannot be used if VIP address was provided")
		}

		subnet := ""
		var err error
		if vip.IsIPv4(address) {
			if ipv4AutoSubnet {
				subnet, err = autoFindSubnet(link, address)
				if err != nil {
					return nil, fmt.Errorf("failed to automatically find subnet for service %s/%s with IP address %s on interface %s: %w", svc.Namespace, svc.Name, address, svcInterface, err)
				}
			} else {
				if cidrs[0] != "" && cidrs[0] != kubevip.Auto {
					subnet = cidrs[0]
				} else {
					subnet = "32"
				}
			}
		} else {
			if ipv6AutoSubnet {
				subnet, err = autoFindSubnet(link, address)
				if err != nil {
					return nil, fmt.Errorf("failed to automatically find subnet for service %s/%s with IP address %s on interface %s: %w", svc.Namespace, svc.Name, address, svcInterface, err)
				}
			} else {
				if len(cidrs) > 1 && cidrs[1] != "" && cidrs[1] != kubevip.Auto {
					subnet = cidrs[1]
				} else {
					subnet = "128"
				}
			}
		}

		//log.Info("new instance", "svc", *svc, "interface", svcInterface)

		// Generate new Virtual IP configuration
		newVips = append(newVips, &kubevip.Config{
			VIP:                    address,
			Interface:              svcInterface,
			SingleNode:             true,
			EnableARP:              config.EnableARP,
			EnableBGP:              config.EnableBGP,
			VIPSubnet:              subnet,
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
		//UID:             instanceUID,
		//VIPs:            instanceAddresses,
		serviceSnapshot: svc,
	}
	// for _, port := range svc.Spec.Ports {
	// 	instance.ExternalPorts = append(instance.ExternalPorts, Port{
	// 		Port: uint16(port.Port), //nolint
	// 		Type: string(port.Protocol),
	// 	})
	// }

	if svc.Annotations != nil {
		instance.dhcpInterfaceHwaddr = svc.Annotations[hwAddrKey]
		instance.dhcpInterfaceIP = svc.Annotations[requestedIP]
		instance.dhcpHostname = svc.Annotations[loadbalancerHostname]
	}

	configPorts := make([]kubevip.Port, 0)
	for _, p := range svc.Spec.Ports {
		configPorts = append(configPorts, kubevip.Port{
			Type: string(p.Protocol),
			Port: int(p.Port),
		})
	}
	// Generate Load Balancer config
	newLB := kubevip.LoadBalancer{
		Name:      fmt.Sprintf("%s-load-balancer", svc.Name),
		Ports:     configPorts,
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
			instance.vipConfigs[0].Interface = instance.dhcpInterface
			instance.vipConfigs[0].VIP = ip
			instance.dhcpInterfaceIP = ip
		}
	}

	for _, vipConfig := range instance.vipConfigs {
		c, err := cluster.InitCluster(vipConfig, false)
		if err != nil {
			log.Error("Failed to add Service %s/%s", svc.Namespace, svc.Name)
			return nil, err
		}

		for i := range c.Network {
			c.Network[i].SetServicePorts(svc)
		}

		instance.clusters = append(instance.clusters, c)
		log.Info("(svcs) adding VIP", "ip", vipConfig.VIP, "interface", vipConfig.Interface, "namespace", svc.Namespace, "name", svc.Name)

	}

	return instance, nil
}

func autoFindInterface(ip string) (netlink.Link, error) {
	links, err := netlink.LinkList()
	if err != nil {
		return nil, fmt.Errorf("failed to list network interfaces: %w", err)
	}

	address := net.ParseIP(ip)

	family := netlink.FAMILY_V4

	if address.To4() == nil {
		family = netlink.FAMILY_V6
	}

	for _, link := range links {
		addr, err := netlink.AddrList(link, family)
		if err != nil {
			return nil, fmt.Errorf("failed to get IP addresses for interface %s: %w", link.Attrs().Name, err)
		}
		for _, a := range addr {
			if a.IPNet.Contains(address) {
				return link, nil
			}
		}
	}

	return nil, nil
}

func autoFindSubnet(link netlink.Link, ip string) (string, error) {
	address := net.ParseIP(ip)

	family := netlink.FAMILY_V4
	if address.To4() == nil {
		family = netlink.FAMILY_V6
	}

	addr, err := netlink.AddrList(link, family)
	if err != nil {
		return "", fmt.Errorf("failed to get IP addresses for interface %s: %w", link.Attrs().Name, err)
	}
	for _, a := range addr {
		if a.IPNet.Contains(address) {
			m, _ := a.IPNet.Mask.Size()
			return strconv.Itoa(m), nil
		}
	}
	return "", fmt.Errorf("failed to find suitable subnet for address %s", ip)
}

func getAutoInterfaceName(link netlink.Link, defaultInterface string) string {
	if link == nil {
		return defaultInterface
	}
	return link.Attrs().Name
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
	interfaceName := fmt.Sprintf("vip-%s", i.serviceSnapshot.UID[0:8])

	// Check if the interface doesn't exist first
	iface, err := net.InterfaceByName(interfaceName)
	if err != nil {
		log.Info("creating new macvlan interface for DHCP", "interface", interfaceName)

		hwaddr, err := net.ParseMAC(i.dhcpInterfaceHwaddr)
		if i.dhcpInterfaceHwaddr != "" && err != nil {
			return err
		} else if hwaddr == nil {
			hwaddr, err = net.ParseMAC(vip.GenerateMac())
			if err != nil {
				return err
			}
		}

		log.Info("new macvlan interface", "interface", interfaceName, "hardware address", hwaddr)
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
		log.Info("Using existing macvlan interface for DHCP", "interface", interfaceName)
	}

	var initRebootFlag bool
	if i.dhcpInterfaceIP != "" {
		initRebootFlag = true
	}

	client := vip.NewDHCPClient(iface, initRebootFlag, i.dhcpInterfaceIP)

	// Add hostname to dhcp client if annotated
	if i.dhcpHostname != "" {
		log.Info("Hostname specified for dhcp lease", "interface", interfaceName, "hostname", i.dhcpHostname)
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
