package instance

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	log "log/slog"

	"github.com/vishvananda/netlink"
	v1 "k8s.io/api/core/v1"

	"github.com/kube-vip/kube-vip/pkg/arp"
	"github.com/kube-vip/kube-vip/pkg/cluster"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/networkinterface"
	"github.com/kube-vip/kube-vip/pkg/sysctl"
	"github.com/kube-vip/kube-vip/pkg/utils"
	"github.com/kube-vip/kube-vip/pkg/vip"
)

// Instance defines an instance of everything needed to manage vips
type Instance struct {
	// Virtual IP / Load Balancer configuration
	VIPConfigs []*kubevip.Config

	// cluster instances
	Clusters []*cluster.Cluster

	// Service uses DHCP
	IsDHCPv4            bool
	IsDHCPv6            bool
	DHCPInterface       string
	DHCPInterfaceHwaddr string
	DHCPInterfaceIP     string
	DHCPInterfaceIPv4   string
	DHCPInterfaceIPv6   string
	DHCPHostname        string
	DHCPv4Client        vip.DHCPClient
	DHCPv6Client        vip.DHCPClient

	// External Gateway IP the service is forwarded from
	UPNPGatewayIPs []string

	// Kubernetes service mapping
	ServiceSnapshot *v1.Service

	dnsAddresses []string
}

type Port struct {
	Port uint16
	Type string
}

func NewInstance(ctx context.Context, svc *v1.Service, config *kubevip.Config, intfMgr *networkinterface.Manager, arpMgr *arp.Manager) (*Instance, error) {
	instanceAddresses, instanceHostnames := FetchServiceAddresses(svc)
	log.Info("NewInstance used", "instanceAddresses", instanceAddresses, "instanceHostnames", instanceHostnames)

	var newVips []*kubevip.Config
	var link netlink.Link
	var err error
	var dnsAddresses []string

	for _, address := range instanceAddresses {
		// Detect if we're using a specific interface for services
		var svcInterface string
		svcInterface = svc.Annotations[kubevip.ServiceInterface] // If the service has a specific interface defined, then use it
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
		if utils.IsIPv4(address) {
			if ipv4AutoSubnet {
				subnet, err = autoFindSubnet(link, address)
				if err != nil {
					return nil, fmt.Errorf("failed to automatically find subnet for service %s/%s with IP address %s on interface %s: %w", svc.Namespace, svc.Name, address, svcInterface, err)
				}
			} else {
				if cidrs[0] != "" && cidrs[0] != kubevip.Auto {
					subnet = cidrs[0]
				} else {
					subnet = strconv.Itoa(vip.DefaultMaskIPv4)
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
					subnet = strconv.Itoa(vip.DefaultMaskIPv6)
				}
			}
		}

		// Generate new Virtual IP configuration
		newVips = append(newVips, &kubevip.Config{
			VIP:                         address,
			Interface:                   svcInterface,
			SingleNode:                  true,
			EnableARP:                   config.EnableARP,
			EnableBGP:                   config.EnableBGP,
			VIPSubnet:                   subnet,
			EnableRoutingTable:          config.EnableRoutingTable,
			RoutingTableID:              config.RoutingTableID,
			RoutingTableType:            config.RoutingTableType,
			RoutingProtocol:             config.RoutingProtocol,
			ArpBroadcastRate:            config.ArpBroadcastRate,
			EnableServiceSecurity:       config.EnableServiceSecurity,
			DNSMode:                     config.DNSMode,
			DHCPMode:                    config.DHCPMode,
			DisableServiceUpdates:       config.DisableServiceUpdates,
			EnableServicesElection:      config.EnableServicesElection,
			PreserveVIPOnLeadershipLoss: config.PreserveVIPOnLeadershipLoss,
			KubernetesLeaderElection: kubevip.KubernetesLeaderElection{
				EnableLeaderElection: config.EnableLeaderElection,
			},
		})
	}

	for _, hostname := range instanceHostnames {
		log.Info("hostname", "addr", hostname)
		// Detect if we're using a specific interface for services
		var svcInterface string
		svcInterface = svc.Annotations[kubevip.ServiceInterface] // If the service has a specific interface defined, then use it

		// If it is still blank then use the
		if svcInterface == "" {
			switch config.ServicesInterface {
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

		// Generate new Virtual IP configuration
		newVips = append(newVips, &kubevip.Config{
			VIP:                    hostname,
			Interface:              svcInterface,
			SingleNode:             true,
			EnableARP:              config.EnableARP,
			EnableBGP:              config.EnableBGP,
			VIPSubnet:              config.VIPSubnet,
			EnableRoutingTable:     config.EnableRoutingTable,
			RoutingTableID:         config.RoutingTableID,
			RoutingTableType:       config.RoutingTableType,
			RoutingProtocol:        config.RoutingProtocol,
			ArpBroadcastRate:       config.ArpBroadcastRate,
			EnableServiceSecurity:  config.EnableServiceSecurity,
			DNSMode:                config.DNSMode,
			DHCPMode:               config.DHCPMode,
			DisableServiceUpdates:  config.DisableServiceUpdates,
			EnableServicesElection: config.EnableServicesElection,
			KubernetesLeaderElection: kubevip.KubernetesLeaderElection{
				EnableLeaderElection: config.EnableLeaderElection,
			},
		})
	}

	// Create new service
	instance := &Instance{
		ServiceSnapshot: svc,
		dnsAddresses:    dnsAddresses,
	}

	if svc.Annotations != nil {
		instance.DHCPInterfaceHwaddr = svc.Annotations[kubevip.HwAddrKey]
		requestedIP := svc.Annotations[kubevip.RequestedIP]
		if requestedIP != "" {
			requestedIPs := strings.Split(requestedIP, ",")
			if len(requestedIPs) > 2 {
				return nil, fmt.Errorf("annotation %q cannot request more than one IPv4 and one Ipv6 address", kubevip.RequestedIP)
			}
			for _, ip := range requestedIPs {
				netip := net.ParseIP(ip)
				if netip.To4() != nil {
					instance.DHCPInterfaceIPv4 = ip
				} else {
					instance.DHCPInterfaceIPv6 = ip
				}
			}
		}
		instance.DHCPHostname = svc.Annotations[kubevip.LoadbalancerHostname]
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
	instance.VIPConfigs = newVips

	// If this was purposely created with the address '0.0.0.0', or '::'
	// we will create a macvlan on the main interface and a DHCP client
	if len(instanceAddresses) > 2 && (slices.Contains(instanceAddresses, "0.0.0.0") || slices.Contains(instanceAddresses, "::")) {
		return nil, fmt.Errorf("DHCP cannot be used if more than 2 addresses (one IPv4 and one IPv6) were specified")
	}
	for i := range instance.VIPConfigs {
		if instance.VIPConfigs[i].VIP == "0.0.0.0" {
			err := instance.startDHCP(ctx, i)
			if err != nil {
				return nil, err
			}
			select {
			case err := <-instance.DHCPv4Client.ErrorChannel():
				return nil, fmt.Errorf("error starting DHCPv4 for %s/%s: error: %s",
					instance.ServiceSnapshot.Namespace, instance.ServiceSnapshot.Name, err)
			case ip := <-instance.DHCPv4Client.IPChannel():
				instance.VIPConfigs[i].Interface = instance.DHCPInterface
				instance.VIPConfigs[i].VIP = ip
				instance.DHCPInterfaceIPv4 = ip
			}
		}
		if instance.VIPConfigs[i].VIP == "::" {
			err := instance.startDHCP(ctx, i)
			if err != nil {
				return nil, err
			}
			select {
			case err := <-instance.DHCPv6Client.ErrorChannel():
				return nil, fmt.Errorf("error starting DHCPv6 for %s/%s: error: %s",
					instance.ServiceSnapshot.Namespace, instance.ServiceSnapshot.Name, err)
			case ip := <-instance.DHCPv6Client.IPChannel():
				instance.VIPConfigs[i].Interface = instance.DHCPInterface
				instance.VIPConfigs[i].VIP = ip
				instance.DHCPInterfaceIPv6 = ip
			}
		}

		ddnsAnnotation, exists := svc.Annotations[kubevip.ServiceDDNS]

		if exists {
			instance.VIPConfigs[i].DDNS, err = strconv.ParseBool(ddnsAnnotation)
			if err != nil {
				log.Error("Failed to add service", "err", err)
				return nil, err
			}
		}

		if len(svc.Spec.IPFamilies) > 0 {
			if len(svc.Spec.IPFamilies) > 1 {
				instance.VIPConfigs[i].DHCPMode = utils.DualFamily
				instance.VIPConfigs[i].DNSMode = utils.DualFamily
				switch *svc.Spec.IPFamilyPolicy {
				case v1.IPFamilyPolicyRequireDualStack:
					instance.VIPConfigs[i].IsDualStack = true
					instance.VIPConfigs[i].RequireDualStack = true
				case v1.IPFamilyPolicyPreferDualStack:
					instance.VIPConfigs[i].IsDualStack = true
					instance.VIPConfigs[i].RequireDualStack = false
				default:
					instance.VIPConfigs[i].IsDualStack = false
					instance.VIPConfigs[i].RequireDualStack = false
				}
			} else {
				if strings.EqualFold(string(svc.Spec.IPFamilies[0]), utils.IPv4Family) {
					instance.VIPConfigs[i].DHCPMode = strings.ToLower(utils.IPv4Family)
					instance.VIPConfigs[i].DNSMode = strings.ToLower(utils.IPv4Family)
				} else {
					instance.VIPConfigs[i].DHCPMode = strings.ToLower(utils.IPv6Family)
					instance.VIPConfigs[i].DNSMode = strings.ToLower(utils.IPv6Family)
				}
			}
		}

		c, err := cluster.InitCluster(instance.VIPConfigs[i], false, intfMgr, arpMgr)
		if err != nil {
			log.Error("failed to add service", "err", err)
			return nil, err
		}

		for i := range c.Network {
			c.Network[i].SetServicePorts(svc)
		}

		instance.Clusters = append(instance.Clusters, c)
		log.Info("(svcs) adding VIP", "ip", instance.VIPConfigs[i].VIP, "interface", instance.VIPConfigs[i].Interface, "namespace", svc.Namespace, "name", svc.Name)
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

func (i *Instance) startDHCP(ctx context.Context, index int) error {
	if len(i.VIPConfigs) > 2 {
		return fmt.Errorf("DHCP can be used with 2 VIP config maximally, got: %v", len(i.VIPConfigs))
	}
	parent, err := netlink.LinkByName(i.VIPConfigs[index].Interface)
	if err != nil {
		return fmt.Errorf("error finding VIP Interface, for building DHCP Link : %v", err)
	}

	// Generate name from UID
	interfaceName := fmt.Sprintf("vip-%s", i.ServiceSnapshot.UID[0:8])

	// Check if the interface doesn't exist first
	iface, err := net.InterfaceByName(interfaceName)
	if err != nil {
		log.Info("creating new macvlan interface for DHCP", "interface", interfaceName)

		hwaddr, err := net.ParseMAC(i.DHCPInterfaceHwaddr)
		if i.DHCPInterfaceHwaddr != "" && err != nil {
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
	ip := net.ParseIP(i.VIPConfigs[index].VIP)

	var client vip.DHCPClient
	if ip.To4() != nil {
		// Default rp_filter setting (https://github.com/kube-vip/kube-vip/issues/1170)
		rpfilterSetting := "0"

		// Check if we need to set an override rp_filter value for the interface
		if i.ServiceSnapshot.Annotations[kubevip.RPFilter] != "" {
			// Check the rp_filter value
			rpFilter, err := strconv.Atoi(i.ServiceSnapshot.Annotations[kubevip.RPFilter])
			if err != nil {
				log.Error("[DHCP] unable to process rp_filter", "value", rpFilter)
			} else {
				if rpFilter >= 0 && rpFilter < 3 { // Ensure the value is 0,1,2
					rpfilterSetting = i.ServiceSnapshot.Annotations[kubevip.RPFilter]
				} else {
					log.Error("[DHCP] rp_filter value not within range 0-2", "value", rpFilter)
				}
			}
		}

		err = sysctl.WriteProcSys("/proc/sys/net/ipv4/conf/"+interfaceName+"/rp_filter", rpfilterSetting)
		if err != nil {
			log.Error("[DHCP] unable to write rp_filter", "value", rpfilterSetting, "err", err)
		}

		if i.DHCPInterfaceIPv4 != "" {
			initRebootFlag = true
		}

		client = vip.NewDHCPv4Client(iface, initRebootFlag, i.DHCPInterfaceIPv4)

		// Add the client so that we can call it to stop function
		i.DHCPv4Client = client

		// Set that DHCPv4 is enabled
		i.IsDHCPv4 = true
	} else {
		if i.DHCPInterfaceIPv6 != "" {
			initRebootFlag = true
		}

		client, err = vip.NewDHCPv6Client(iface, parent, initRebootFlag, i.DHCPInterfaceIPv6)
		if err != nil {
			return fmt.Errorf("unable to create client: %w", err)
		}

		// Add the client so that we can call it to stop function
		i.DHCPv6Client = client

		// Set that DHCPv6 is enabled
		i.IsDHCPv6 = true
	}

	// Add hostname to dhcp client if annotated
	if i.DHCPHostname != "" {
		log.Info("Hostname specified for dhcp lease", "interface", interfaceName, "hostname", i.DHCPHostname)
		client.WithHostName(i.DHCPHostname)
	}

	go func() {
		if err := client.Start(ctx); err != nil {
			log.Error("[instance] DHCP client error: %w")
		}
	}()

	// Set the name of the interface so that it can be removed on Service deletion
	i.DHCPInterface = interfaceName
	i.DHCPInterfaceHwaddr = iface.HardwareAddr.String()

	return nil
}

// FetchLoadBalancerIngressAddresses tries to get the addresses from status.loadBalancerIP
func FetchLoadBalancerIngress(s *v1.Service) ([]string, []string) {
	// If the service has no status, return empty
	lbStatusAddresses := []string{}
	lbStatusHostnames := []string{}
	if len(s.Status.LoadBalancer.Ingress) == 0 {
		return lbStatusAddresses, lbStatusHostnames
	}

	for _, ingress := range s.Status.LoadBalancer.Ingress {
		if ingress.IP != "" {
			lbStatusAddresses = append(lbStatusAddresses, ingress.IP)
		}
		if ingress.Hostname != "" {
			lbStatusHostnames = append(lbStatusHostnames, ingress.Hostname)
		}
	}
	return lbStatusAddresses, lbStatusHostnames
}

// FetchServiceAddresses tries to get the addresses from annotations
// kube-vip.io/loadbalancerIPs, then from spec.loadbalancerIP
func FetchServiceAddresses(s *v1.Service) ([]string, []string) {
	annotationAvailable := false
	if s.Annotations != nil {

		if v, annotationAvailable := s.Annotations[kubevip.LoadbalancerIPAnnotation]; annotationAvailable {
			ips := strings.Split(v, ",")
			var trimmedIPs []string
			var trimmedHostnames []string
			for _, a := range ips {
				a = strings.TrimSpace(a)
				ip := net.ParseIP(a)
				if ip == nil {
					// this is probably a DNS name
					trimmedHostnames = append(trimmedHostnames, a)
				} else {
					trimmedIPs = append(trimmedIPs, ip.String())
				}
			}
			return trimmedIPs, trimmedHostnames
		}
	}

	lbStatusAddresses := []string{}
	lbStatusHostnames := []string{}
	if !annotationAvailable {
		lbStatusAddresses, lbStatusHostnames = FetchLoadBalancerIngress(s)
	}

	// Spec.LoadBalancerIP legacy handling
	// if the loadBalancerIP is different from Status.LoadBalancer.Ingress IPs
	// return the legacy LB as spec wins over status.
	if lbIP := net.ParseIP(s.Spec.LoadBalancerIP); lbIP != nil && len(lbStatusAddresses) > 0 {
		isLbIPv4 := utils.IsIPv4(s.Spec.LoadBalancerIP)
		for _, a := range lbStatusAddresses {
			if lbStatusIP := net.ParseIP(a); lbStatusIP != nil && utils.IsIPv4(a) == isLbIPv4 && !lbIP.Equal(lbStatusIP) {
				return []string{s.Spec.LoadBalancerIP}, []string{}
			}
		}
	}

	if len(lbStatusAddresses) > 0 || len(lbStatusHostnames) > 0 {
		return lbStatusAddresses, lbStatusHostnames
	}

	if s.Spec.LoadBalancerIP != "" {
		return []string{s.Spec.LoadBalancerIP}, []string{}
	}

	return []string{}, []string{}
}

func FindServiceInstance(svc *v1.Service, instances []*Instance) *Instance {
	log.Debug("finding service", "namespace", svc.Namespace, "name", svc.Name, "UID", svc.UID)
	for i := range instances {
		if instances[i].ServiceSnapshot.UID == svc.UID {
			return instances[i]
		}
	}
	log.Debug("instance not found", "namespace", svc.Namespace, "name", svc.Name, "UID", svc.UID)
	return nil
}

func FindServiceInstanceWithTimeout(svc *v1.Service, instances []*Instance) *Instance {
	log.Debug("finding service with timeout", "namespace", svc.Namespace, "name", svc.Name, "UID", svc.UID)
	ticker := time.NewTicker(time.Millisecond * 200)
	defer ticker.Stop()
	to := time.NewTimer(time.Second * 60)
	defer to.Stop()
	for {
		select {
		case <-to.C:
			return nil
		case <-ticker.C:
			for i := range instances {
				if instances[i].ServiceSnapshot.UID == svc.UID {
					return instances[i]
				}
			}
		}
	}
}
