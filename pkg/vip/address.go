package vip

import (
	"fmt"
	"math"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"

	log "log/slog"

	"github.com/pkg/errors"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
	v1 "k8s.io/api/core/v1"

	iptables "github.com/kube-vip/kube-vip/pkg/iptables"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/utils"

	"github.com/kube-vip/kube-vip/pkg/networkinterface"
)

const (
	defaultValidLft         = 60
	iptablesComment         = "%s kube-vip load balancer IP"
	iptablesCommentMarkRule = "kube-vip load balancer IP set mark for masquerade"

	DefaultMaskIPv4 = 32
	DefaultMaskIPv6 = 128
)

// Network is an interface that enable managing operations for a given IP
type Network interface {
	AddIP(precheck bool, skipDAD bool) (bool, error)
	AddRoute(precheck bool) error
	DeleteIP() (bool, error)
	DeleteRoute() error
	UpdateRoutes() (bool, error)
	IsSet() (bool, error)
	IP() string
	CIDR() string
	IPisLinkLocal() bool
	PrepareRoute() *netlink.Route
	SetIP(ip string) error
	SetServicePorts(service *v1.Service)
	Interface() string
	IsDADFAIL() bool
	IsDNS() bool
	IsDDNS() bool
	DDNSHostName() string
	DNSName() string
	SetMask(mask string) error
	SetHasEndpoints(value bool)
	HasEndpoints() bool
	ARPName() string
	GetPossibleSubnets() string
	DHCPFamily() string
}

// network - This allows network configuration
type network struct {
	mu sync.Mutex

	address        *netlink.Addr
	link           *networkinterface.Link
	ports          []v1.ServicePort
	serviceName    string
	enableSecurity bool
	ignoreSecurity bool

	dnsName string
	isDDNS  bool

	forwardMethod   string
	iptablesBackend string

	routeTable       int
	routingTableType int
	routingProtocol  int

	ipvsEnabled bool

	hasEndpoints bool

	possibleSubnets string

	// used by DHCP to get address of proper family
	dhcpFamily string
}

// NewConfig will attempt to provide an interface to the kernel network configuration
func NewConfig(address string, iface string, loGlobalScope bool, subnet string, isDDNS bool,
	dhcpMode string, requireDualStack, isDualStack bool, tableID int, tableType int, routingProtocol int,
	dnsMode, forwardMethod, iptablesBackend string, ipvsEnabled, enableSecurity bool,
	intfMgr *networkinterface.Manager) ([]Network, error) {
	networks := []Network{}

	link, err := netlink.LinkByName(iface)
	if err != nil {
		return networks, errors.Wrapf(err, "could not get link for interface '%s'", iface)
	}

	networkLink := intfMgr.Get(link)

	if utils.IsIP(address) {
		result := &network{
			link:             networkLink,
			routeTable:       tableID,
			routingTableType: tableType,
			routingProtocol:  routingProtocol,
			forwardMethod:    forwardMethod,
			iptablesBackend:  iptablesBackend,
			ipvsEnabled:      ipvsEnabled,
			possibleSubnets:  subnet,
		}

		subnet, err = SelectSubnet(address, subnet)
		if err != nil {
			return networks, fmt.Errorf("unable to select subnet for IP %q from %q: %w", address, subnet, err)
		}

		// Check if the subnet needs overriding
		cidr, err := utils.FormatIPWithSubnetMask(address, subnet)
		if err != nil {
			return networks, errors.Wrapf(err, "could not format address '%s' with subnetMask '%s'", address, subnet)
		}
		result.address, err = netlink.ParseAddr(cidr)
		if err != nil {
			return networks, errors.Wrapf(err, "could not parse address '%s'", address)
		}

		// set address as deprecated so it isn't used as source address according to RFC 3484
		result.address.PreferedLft = 0

		// Also set ValidLft so the netlink library actually sets them
		result.address.ValidLft = math.MaxInt

		if iface == "lo" && !loGlobalScope {
			// set host scope on loopback, otherwise global scope will be used by default
			result.address.Scope = unix.RT_SCOPE_HOST
		}

		networks = append(networks, result)
	} else {
		// try to resolve the address
		log.Debug("looking up host", "address", address, "dnsMode", dnsMode)
		ips, err := utils.LookupHost(address, dnsMode, requireDualStack)
		if (dnsMode == utils.DualFamily && isDDNS && isDualStack) || err != nil {
			// return early for ddns if no IP is allocated for the domain
			// when leader starts, should do get IP from DHCP for the domain
			if isDDNS {
				log.Info("isDDNS true", "dhcpMode", dhcpMode)
				if strings.EqualFold(dhcpMode, utils.IPv4Family) || strings.EqualFold(dhcpMode, utils.DualFamily) {
					result := &network{
						link:             networkLink,
						routeTable:       tableID,
						routingTableType: tableType,
						routingProtocol:  routingProtocol,
						forwardMethod:    forwardMethod,
						iptablesBackend:  iptablesBackend,
						isDDNS:           isDDNS,
						dnsName:          address,
						ipvsEnabled:      ipvsEnabled,
						enableSecurity:   enableSecurity,
						possibleSubnets:  subnet,
						dhcpFamily:       utils.IPv4Family,
					}

					networks = append(networks, result)
				}

				if strings.EqualFold(dhcpMode, utils.IPv6Family) || strings.EqualFold(dhcpMode, utils.DualFamily) {
					result := &network{
						link:             networkLink,
						routeTable:       tableID,
						routingTableType: tableType,
						routingProtocol:  routingProtocol,
						forwardMethod:    forwardMethod,
						iptablesBackend:  iptablesBackend,
						isDDNS:           isDDNS,
						dnsName:          address,
						ipvsEnabled:      ipvsEnabled,
						enableSecurity:   enableSecurity,
						possibleSubnets:  subnet,
						dhcpFamily:       utils.IPv6Family,
					}

					networks = append(networks, result)
				}

				return networks, nil
			}
			return nil, err
		}

		for _, ip := range ips {
			result := &network{
				link:             networkLink,
				routeTable:       tableID,
				routingTableType: tableType,
				routingProtocol:  routingProtocol,
				forwardMethod:    forwardMethod,
				iptablesBackend:  iptablesBackend,
				isDDNS:           isDDNS,
				dnsName:          address,
				ipvsEnabled:      ipvsEnabled,
				enableSecurity:   enableSecurity,
				possibleSubnets:  subnet,
			}

			s, err := SelectSubnet(ip, subnet)
			if err != nil {
				return nil, fmt.Errorf("failed to select subnet: %w", err)
			}

			if result.address, err = netlink.ParseAddr(fmt.Sprintf("%s/%s", ip, s)); err != nil {
				return networks, err
			}
			// set ValidLft so that the VIP expires if the DNS entry is updated, otherwise it'll be refreshed by the DNS prober
			result.address.ValidLft = defaultValidLft

			// set address as deprecated so it isn't used as source address according to RFC 3484
			result.address.PreferedLft = 0

			result.dhcpFamily = strings.ToLower(utils.IPv6Family)
			if net.ParseIP(ip).To4() != nil {
				result.dhcpFamily = strings.ToLower(utils.IPv4Family)
			}

			networks = append(networks, result)
		}

	}

	return networks, nil
}

// ListRoutes returns all routes from selected table with selected protocol
func ListRoutes(table, protocol int) ([]netlink.Route, error) {
	route := &netlink.Route{
		Table:    table,
		Protocol: netlink.RouteProtocol(protocol),
	}
	routes, err := netlink.RouteListFiltered(nl.FAMILY_ALL, route, netlink.RT_FILTER_PROTOCOL|netlink.RT_FILTER_TABLE)
	if err != nil {
		return nil, fmt.Errorf("error getting routes from table [%d] with protocol [%d]: %w", table, protocol, err)
	}
	return routes, nil
}

// ListRoutesByDst returns all routes from selected table with selected destination IP
func ListRoutesByDst(table int, dst *net.IPNet) ([]netlink.Route, error) {
	route := &netlink.Route{
		Dst:   dst,
		Table: table,
	}
	routes, err := netlink.RouteListFiltered(nl.FAMILY_ALL, route, netlink.RT_FILTER_TABLE|netlink.RT_FILTER_DST)
	if err != nil {
		return nil, fmt.Errorf("error getting routes from table [%d] with destination IP [%s]: %w", table, dst.String(), err)
	}
	return routes, nil
}

func (configurator *network) PrepareRoute() *netlink.Route {
	routeScope := netlink.SCOPE_UNIVERSE
	if configurator.routingTableType == unix.RTN_LOCAL {
		routeScope = netlink.SCOPE_LINK
	}
	route := &netlink.Route{
		Scope:     routeScope,
		Dst:       configurator.address.IPNet,
		LinkIndex: configurator.link.Intf.Attrs().Index,
		Table:     configurator.routeTable,
		Type:      configurator.routingTableType,
		Protocol:  netlink.RouteProtocol(configurator.routingProtocol),
	}
	return route
}

// AddRoute - Add an IP address to a route table
func (configurator *network) AddRoute(precheck bool) error {
	configurator.link.Lock.Lock()
	defer configurator.link.Lock.Unlock()
	route := configurator.PrepareRoute()

	exists := false
	var err error
	if precheck {
		exists, err = configurator.routeExists(route)
		if err != nil {
			return errors.Wrap(err, "failed to check route")
		}
	}

	if !exists {
		if err := netlink.RouteAdd(route); err != nil {
			return errors.Wrap(err, "failed to add route")
		}
	}

	return nil
}

func (configurator *network) routeExists(route *netlink.Route) (bool, error) {
	routes, err := netlink.RouteList(configurator.link.Intf, netlink.FAMILY_ALL)
	if err != nil {
		return false, errors.Wrap(err, "failed to list routes")
	}

	for _, r := range routes {
		if r.Equal(*route) {
			return true, nil
		}
	}

	return false, nil
}

// DeleteRoute - Delete an IP address from a route table
func (configurator *network) DeleteRoute() error {
	route := configurator.PrepareRoute()
	return netlink.RouteDel(route)
}

// GetRoutes - Get an IP addresses from a route table
func (configurator *network) getRoutes() (*[]netlink.Route, error) {
	routes, err := ListRoutesByDst(configurator.routeTable, configurator.address.IPNet)
	if err != nil {
		return nil, fmt.Errorf("error getting routes: %w", err)
	}
	return &routes, nil
}

func (configurator *network) UpdateRoutes() (bool, error) {
	routes, err := configurator.getRoutes()
	if err != nil {
		return false, fmt.Errorf("error updating routes: %w", err)
	}
	isUpdated := false
	r := configurator.PrepareRoute()
	for _, route := range *routes {
		if route.Protocol == unix.RTPROT_BOOT &&
			(route.Type == r.Type || route.Type == unix.RTN_UNICAST) &&
			route.LinkIndex == r.LinkIndex && route.Scope == r.Scope {
			if err = netlink.RouteReplace(r); err != nil {
				return false, fmt.Errorf("error replacing route: %w", err)
			}
			isUpdated = true
		}
	}
	return isUpdated, nil
}

// AddIP - Add an IP address to the interface
// precheck: if true, check if the IP already exists before adding
// skipDAD: if true, set IFA_F_NODAD flag for IPv6 addresses to skip Duplicate Address Detection
func (configurator *network) AddIP(precheck bool, skipDAD bool) (bool, error) {
	configurator.link.Lock.Lock()
	defer configurator.link.Lock.Unlock()
	exists := false
	var err error
	if precheck {
		if exists, err = configurator.IsSet(); err != nil {
			return false, errors.Wrap(err, "could not check if address exists")
		}
	}

	if exists {
		return false, nil
	}

	// For IPv6 addresses, optionally set NODAD flag to skip Duplicate Address Detection (DAD)
	// This prevents DADFAILED loops when recovering from a previous DADFAILED state
	// The flag tells the kernel to skip DAD, which is safe when we're re-adding
	// an address that we know should be ours (e.g., after DADFAILED recovery)
	if skipDAD && utils.IsIPv6(configurator.address.IP.String()) {
		configurator.address.Flags |= unix.IFA_F_NODAD
		log.Debug("Setting IFA_F_NODAD flag for IPv6 address to skip DAD", "ip", configurator.address.IP.String())
	}

	if err := netlink.AddrReplace(configurator.link.Intf, configurator.address); err != nil {
		return false, errors.Wrap(err, fmt.Sprintf("could not add ip to device %q", configurator.link.Intf.Attrs().Name))
	}

	if err := configurator.configureIPTables(); err != nil {
		return true, errors.Wrap(err, "could not configure IPTables")
	}

	return true, nil
}

func (configurator *network) configureIPTables() error {
	if configurator.enableSecurity && !configurator.ignoreSecurity {
		if err := configurator.addIptablesRulesToLimitTrafficPorts(); err != nil {
			return errors.Wrap(err, "could not add iptables rules to limit traffic ports")
		}
	}

	// It seems that masquerading is only reuired with IPv4 for IPVS to work.
	if configurator.ipvsEnabled && configurator.forwardMethod == "masquerade" && configurator.address.IP.To4() != nil {
		if err := configurator.addIptablesRulesForMasquerade(); err != nil {
			return errors.Wrap(err, "could not add iptables rules for masquerade")
		}
	}

	return nil
}

func (configurator *network) addIptablesRulesToLimitTrafficPorts() error {
	ipt, err := iptables.New()
	if err != nil {
		return errors.Wrap(err, "could not create iptables client")
	}

	vip := configurator.address.IP.String()
	comment := fmt.Sprintf(iptablesComment, configurator.serviceName)
	if err := insertCommonIPTablesRules(ipt, vip, comment); err != nil {
		return fmt.Errorf("could not add common iptables rules: %w", err)
	}
	log.Debug("add iptables rules", "vip", vip, "ports", configurator.ports)
	if err := configurator.insertIPTablesRulesForServicePorts(ipt, vip, comment); err != nil {
		return fmt.Errorf("could not add iptables rules for service ports: %v", err)
	}

	return nil
}

func (configurator *network) insertIPTablesRulesForServicePorts(ipt *iptables.IPTables, vip, comment string) error {
	isPortsRuleExisting := make([]bool, len(configurator.ports))

	// delete rules of ports that are not in the service
	rules, err := ipt.List(iptables.TableFilter, iptables.ChainInput)
	if err != nil {
		return fmt.Errorf("could not list iptables rules: %w", err)
	}
	for _, rule := range rules {
		// only handle rules with kube-vip comment
		if iptables.GetIPTablesRuleSpecification(rule, "--comment") != comment {
			continue
		}
		// if the rule is not for the vip, delete it
		if iptables.GetIPTablesRuleSpecification(rule, "-d") != vip {
			if err := ipt.Delete(iptables.TableFilter, iptables.ChainInput, rule); err != nil {
				return fmt.Errorf("could not delete iptables rule: %w", err)
			}
		}

		protocol := iptables.GetIPTablesRuleSpecification(rule, "-p")
		port := iptables.GetIPTablesRuleSpecification(rule, "--dport")
		// ignore DHCP client port
		if protocol == string(v1.ProtocolUDP) && port == dhcpClientPort {
			continue
		}
		// if the rule is for the vip, but its protocol and port are not in the service, delete it
		toBeDeleted := true
		for i, p := range configurator.ports {
			if string(p.Protocol) == protocol && strconv.Itoa(int(p.Port)) == port {
				// the rule is for the vip and its protocol and port are in the service, keep it and mark it as existing
				toBeDeleted = false
				isPortsRuleExisting[i] = true
			}
		}
		if toBeDeleted {
			if err := ipt.Delete(iptables.TableFilter, iptables.ChainInput, strings.Split(rule, "")...); err != nil {
				return fmt.Errorf("could not delete iptables rule: %w", err)
			}
		}
	}
	// add rules of ports that are not existing
	// iptables -A INPUT -d <vip> -p <protocol> --dport <port> -j ACCEPT -m comment —comment “<namespace/service-name> kube-vip load balancer IP”
	for i, ok := range isPortsRuleExisting {
		if !ok {
			if err := ipt.InsertUnique(iptables.TableFilter, iptables.ChainInput, 1, "-d", vip, "-p",
				string(configurator.ports[i].Protocol), "--dport", strconv.Itoa(int(configurator.ports[i].Port)),
				"-m", "comment", "--comment", comment, "-j", "ACCEPT"); err != nil {
				return fmt.Errorf("could not add iptables rule to accept the traffic to VIP %s for allowed "+
					"port %d: %v", vip, configurator.ports[i].Port, err)
			}
		}
	}

	return nil
}

func insertCommonIPTablesRules(ipt *iptables.IPTables, vip, comment string) error {
	if err := ipt.InsertUnique(iptables.TableFilter, iptables.ChainInput, 1, "-d", vip, "-p",
		string(v1.ProtocolUDP), "--dport", dhcpClientPort, "-m", "comment", "--comment", comment, "-j", "ACCEPT"); err != nil {
		return fmt.Errorf("could not add iptables rule to accept the traffic to VIP %s for DHCP client port: %w", vip, err)
	}
	// add rule to drop the traffic to VIP that is not allowed
	// iptables -A INPUT -d <vip> -j DROP
	if err := ipt.InsertUnique(iptables.TableFilter, iptables.ChainInput, 2, "-d", vip, "-m",
		"comment", "--comment", comment, "-j", "DROP"); err != nil {
		return fmt.Errorf("could not add iptables rule to drop the traffic to VIP %s: %v", vip, err)
	}
	return nil
}

func deleteCommonIPTablesRules(ipt *iptables.IPTables, vip, comment string) error {
	if err := ipt.DeleteIfExists(iptables.TableFilter, iptables.ChainInput, "-d", vip, "-p",
		string(v1.ProtocolUDP), "--dport", dhcpClientPort, "-m", "comment", "--comment", comment, "-j", "ACCEPT"); err != nil {
		return fmt.Errorf("could not delete iptables rule to accept the traffic to VIP %s for DHCP client port: %w", vip, err)
	}
	// add rule to drop the traffic to VIP that is not allowed
	// iptables -A INPUT -d <vip> -j DROP
	if err := ipt.DeleteIfExists(iptables.TableFilter, iptables.ChainInput, "-d", vip, "-m", "comment",
		"--comment", comment, "-j", "DROP"); err != nil {
		return fmt.Errorf("could not delete iptables rule to drop the traffic to VIP %s: %v", vip, err)
	}
	return nil
}

func (configurator *network) removeIptablesRuleToLimitTrafficPorts() error {
	ipt, err := iptables.New()
	if err != nil {
		return errors.Wrap(err, "could not create iptables client")
	}
	vip := configurator.address.IP.String()
	comment := fmt.Sprintf(iptablesComment, configurator.serviceName)

	if err := deleteCommonIPTablesRules(ipt, vip, comment); err != nil {
		return fmt.Errorf("could not delete common iptables rules: %w", err)
	}

	log.Debug("remove iptables rules", "vip", vip, "ports", configurator.ports)
	for _, port := range configurator.ports {
		// iptables -D INPUT -d  <VIP> -p <protocol> --dport <port> -j ACCEPT
		if err := ipt.DeleteIfExists(iptables.TableFilter, iptables.ChainInput, "-d", vip, "-p", string(port.Protocol),
			"--dport", strconv.Itoa(int(port.Port)), "-m", "comment", "--comment", comment, "-j", "ACCEPT"); err != nil {
			return fmt.Errorf("could not delete iptables rule to accept the traffic to VIP %s for allowed port %d: %v", vip, port.Port, err)
		}
	}

	return nil
}

// DeleteIP - Remove an IP address from the interface
func (configurator *network) DeleteIP() (bool, error) {
	configurator.link.Lock.Lock()
	defer configurator.link.Lock.Unlock()

	result, err := configurator.IsSet()
	if err != nil {
		return false, errors.Wrap(err, "ip check in DeleteIP failed")
	}

	// Nothing to delete
	if !result {
		return false, nil
	}

	if err = netlink.AddrDel(configurator.link.Intf, configurator.address); err != nil {
		return false, errors.Wrap(err, "could not delete ip")
	}

	if configurator.enableSecurity && !configurator.ignoreSecurity {
		if err := configurator.removeIptablesRuleToLimitTrafficPorts(); err != nil {
			return true, errors.Wrap(err, "could not remove iptables rules to limit traffic ports")
		}
	}

	if configurator.ipvsEnabled && configurator.forwardMethod == "masquerade" && configurator.address.IP.To4() != nil {
		if err := configurator.removeIptablesRulesForMasquerade(); err != nil {
			return true, errors.Wrap(err, "could not remove iptables masquerade rules ")
		}
	}

	return true, nil
}

func (configurator *network) addIptablesRulesForMasquerade() error {
	ver, err := iptables.GetVersion()
	if err != nil {
		return errors.Wrap(err, "could not get iptables version")
	}

	ipt, err := iptables.New(iptables.EnableNFTables(ver.BackendMode == "nft"))
	if err != nil {
		return errors.Wrap(err, "could not create iptables client")
	}

	vip := configurator.address.IP.String()
	comment := fmt.Sprintf(iptablesComment, vip)
	if err := addMasqueradeRuleForVIP(ipt, vip, comment); err != nil {
		return err
	}

	return nil
}

// addIptablesRulesForMasquerade add iptables rules for MASQUERADE
// insert example
func (configurator *network) removeIptablesRulesForMasquerade() error {
	ver, err := iptables.GetVersion()
	if err != nil {
		return errors.Wrap(err, "could not get iptables version")
	}
	ipt, err := iptables.New(iptables.EnableNFTables(ver.BackendMode == "nft"))
	if err != nil {
		return errors.Wrap(err, "could not create iptables client")
	}
	vip := configurator.address.IP.String()
	comment := fmt.Sprintf(iptablesComment, vip)

	err = delMasqueradeRuleForVIP(ipt, vip, comment)
	if err != nil {
		return err
	}

	return nil
}

// TODO: investigate if adding "--vport <port>" would be better or not quite necessary
// After this rule is added, ipvs kernel module is also loaded
func addMasqueradeRuleForVIP(ipt *iptables.IPTables, vip, comment string) error {
	err := ipt.InsertUnique(iptables.TableNat, iptables.ChainPOSTROUTING,
		1, "-m", "ipvs", "--vaddr", vip, "-j", "MASQUERADE", "-m", "comment", "--comment", comment)
	if err != nil {
		return fmt.Errorf("could not add masquerade rule for VIP %s: %v", vip, err)
	}
	return nil
}

func delMasqueradeRuleForVIP(ipt *iptables.IPTables, vip, comment string) error {
	err := ipt.DeleteIfExists(iptables.TableNat, iptables.ChainPOSTROUTING,
		"-m", "ipvs", "--vaddr", vip, "-j", "MASQUERADE", "-m", "comment", "--comment", comment)
	if err != nil {
		return fmt.Errorf("could not del masquerade rule for VIP %s: %v", vip, err)
	}
	return nil
}

// IsDADFAIL - Returns true if the address is IPv6 and has DADFAILED flag
func (configurator *network) IsDADFAIL() bool {
	configurator.link.Lock.Lock()
	defer configurator.link.Lock.Unlock()

	if configurator.address == nil || !utils.IsIPv6(configurator.address.IP.String()) {
		return false
	}

	// Get all the address
	addresses, err := netlink.AddrList(configurator.link.Intf, netlink.FAMILY_V6)
	if err != nil {
		return false
	}

	// Find the VIP and check if it is DADFAILED
	for _, address := range addresses {
		if address.IP.Equal(configurator.address.IP) && addressHasDADFAILEDFlag(address) {
			return true
		}
	}

	return false
}

func addressHasDADFAILEDFlag(address netlink.Addr) bool {
	return address.Flags&unix.IFA_F_DADFAILED != 0
}

// isSet - Check to see if VIP is set
func (configurator *network) IsSet() (result bool, err error) {
	var addresses []netlink.Addr

	if configurator.address == nil {
		return false, nil
	}

	if configurator.address.Mask == nil {
		return false, nil
	}

	addresses, err = netlink.AddrList(configurator.link.Intf, 0)
	if err != nil {
		err = errors.Wrap(err, "could not list addresses")

		return false, err
	}

	for _, address := range addresses {
		if address.Equal(*configurator.address) {
			return true, nil
		}
	}

	return false, nil
}

// SetIP updates the IP that is used
func (configurator *network) SetIP(ip string) error {
	configurator.mu.Lock()
	defer configurator.mu.Unlock()

	configurator.link.Lock.Lock()
	defer configurator.link.Lock.Unlock()

	if strings.Contains("/", ip) {
		return fmt.Errorf("ip should not contain CIDR notation got: %s", ip)
	}

	if configurator.address == nil {
		log.Debug("possible", "subnets", configurator.possibleSubnets)
		subnet, err := SelectSubnet(ip, configurator.possibleSubnets)
		if err != nil {
			return fmt.Errorf("unable to select subnet for IP %q from %q: %w", ip, subnet, err)
		}

		// Check if the subnet needs overriding
		cidr, err := utils.FormatIPWithSubnetMask(ip, subnet)
		if err != nil {
			return errors.Wrapf(err, "2 could not format address %q with subnetMask %q", ip, subnet)
		}
		configurator.address, err = netlink.ParseAddr(cidr)
		if err != nil {
			return errors.Wrapf(err, "could not parse address %q", cidr)
		}
	}

	ones, _ := configurator.address.Mask.Size()
	cidr, err := utils.FormatIPWithSubnetMask(ip, strconv.Itoa(ones))
	if err != nil {
		return fmt.Errorf("could not format address '%s' with subnetMask '%s'", ip, strconv.Itoa(ones))
	}
	addr, err := netlink.ParseAddr(cidr)
	if err != nil {
		return err
	}
	if configurator.address != nil && configurator.IsDNS() {
		addr.ValidLft = defaultValidLft
	} else {
		addr.ValidLft = math.MaxInt
	}

	// set address as deprecated so it isn't used as source address according to RFC 3484
	addr.PreferedLft = 0

	configurator.address = addr
	return nil
}

// SetServicePorts updates the service ports from the service
// If you want to limit traffic to the VIP to only the service ports, add service ports to the network firstly.
func (configurator *network) SetServicePorts(service *v1.Service) {
	configurator.mu.Lock()
	defer configurator.mu.Unlock()

	configurator.ports = service.Spec.Ports
	configurator.serviceName = service.Namespace + "/" + service.Name
	configurator.ignoreSecurity = service.Annotations[kubevip.ServiceSecurityIgnore] == "true"
}

// IP - return the IP Address
func (configurator *network) IP() string {
	configurator.mu.Lock()
	defer configurator.mu.Unlock()

	if configurator.address == nil || configurator.address.IP == nil {
		return ""
	}

	return configurator.address.IP.String()
}

func (configurator *network) CIDR() string {
	configurator.mu.Lock()
	defer configurator.mu.Unlock()

	if configurator.address == nil || configurator.address.IPNet == nil {
		return ""
	}

	return configurator.address.IPNet.String()
}

// IP - return the IP Address
func (configurator *network) IPisLinkLocal() bool {
	configurator.mu.Lock()
	defer configurator.mu.Unlock()

	return configurator.address.IP.IsLinkLocalUnicast()
}

// DNSName return the configured dnsName when use DNS
func (configurator *network) DNSName() string {
	return configurator.dnsName
}

// IsDNS - when dnsName is configured
func (configurator *network) IsDNS() bool {
	return configurator.dnsName != ""
}

// IsDDNS - return true if use dynamic dns
func (configurator *network) IsDDNS() bool {
	return configurator.isDDNS
}

// DDNSHostName - return the hostname for dynamic dns
// when dDNSHostName is not empty, use DHCP to get IP for hostname: dDNSHostName
// it's expected that dynamic DNS should be configured so
// the fqdn for apiserver endpoint is dDNSHostName.{LocalDomain}
func (configurator *network) DDNSHostName() string {
	return getHostName(configurator.dnsName)
}

// Interface - return the Interface name
func (configurator *network) Interface() string {
	return configurator.link.Intf.Attrs().Name
}

func GarbageCollect(adapter, address string, intfMgr *networkinterface.Manager) (found bool, err error) {
	// Get adapter
	link, err := netlink.LinkByName(adapter)
	if err != nil {
		return true, errors.Wrapf(err, "could not get link for interface '%s'", adapter)
	}

	l := intfMgr.Get(link)

	l.Lock.Lock()
	defer l.Lock.Unlock()

	// Get addresses on adapter
	addrs, err := netlink.AddrList(l.Intf, netlink.FAMILY_ALL)
	if err != nil {
		return false, err
	}

	// Compare all addresses to new service address, and remove if needed
	for _, existing := range addrs {
		if existing.IP.String() == address {
			// We've found the existing address
			found = true
			// linting issue
			existing := existing
			if err = netlink.AddrDel(l.Intf, &existing); err != nil {
				return true, errors.Wrap(err, "could not delete ip")
			}
		}
	}
	return // Didn't find the address on the adapter
}

func (configurator *network) SetMask(mask string) error {
	selectedMask := mask
	var err error

	if mask == "" {
		return fmt.Errorf("no mask provided")
	}

	if configurator.IP() != "" {
		selectedMask, err = SelectSubnet(configurator.IP(), mask)
		if err != nil {
			return fmt.Errorf("failed to select mask %q: %w", mask, err)
		}
	} else if len(strings.Split(mask, ",")) > 1 {
		return fmt.Errorf("cannot select mask from %q when IP address is unknown", mask)
	}

	m, err := strconv.Atoi(selectedMask)
	if err != nil {
		return err
	}

	size := DefaultMaskIPv4
	family := utils.IPv4Family

	if configurator.IP() != "" {
		if utils.IsIPv6(configurator.IP()) {
			size = DefaultMaskIPv6
			family = utils.IPv6Family
		}

		if m > size {
			return fmt.Errorf("provided CIDR mask '%d' is greater than the highest mask value for the %s family (%d)", m, family, size)
		}
	}

	toSet := net.CIDRMask(m, size)
	if toSet == nil {
		return fmt.Errorf("failed to create mask /%d", m)
	}

	configurator.mu.Lock()
	defer configurator.mu.Unlock()

	configurator.address.Mask = toSet
	return nil
}

func (configurator *network) SetHasEndpoints(value bool) {
	log.Debug("setting HasEndpoints", "ip", configurator.IP(), "value", value)
	configurator.hasEndpoints = value
}

func (configurator *network) HasEndpoints() bool {
	log.Debug("getting HasEndpoints", "ip", configurator.IP(), "value", configurator.hasEndpoints)
	return configurator.hasEndpoints
}

func (configurator *network) ARPName() string {
	return fmt.Sprintf("%s-%s", configurator.CIDR(), configurator.Interface())
}

func (configurator *network) GetPossibleSubnets() string {
	return configurator.possibleSubnets
}

func (configurator *network) DHCPFamily() string {
	return configurator.dhcpFamily
}

// SelectSubnet formats an IP address with the appropriate CIDR based on the input.
// The input SubnetMasks can be "32,128" (dual-stack), "32", "128" (SingleStack).
func SelectSubnet(rawIP string, subnetMasks string) (string, error) {
	// Split the SubnetMasks input into DualStack or SingleStack
	// If the input is "32,128", it will be split into ["32", "128"]
	subnetMasksParts := strings.Split(subnetMasks, ",")
	if len(subnetMasksParts) == 0 {
		return "", fmt.Errorf("no subnetMasks provided got: %q", subnetMasks)
	} else if len(subnetMasksParts) > 2 {
		return "", fmt.Errorf("invalid subnetMasks provided got: %q", subnetMasks)
	}
	if slices.Contains(subnetMasksParts, "auto") {
		return "", fmt.Errorf("auto subnet discovery only works for services: %q", subnetMasks)
	}

	// Parse the raw IP address
	ip := net.ParseIP(rawIP)
	if ip == nil {
		return "", fmt.Errorf("invalid IP address: %s", rawIP)
	}
	if ip.To4() != nil {
		return subnetMasksParts[0], nil
	}
	if ip.To16() != nil {
		subnetMask := subnetMasksParts[0]
		if len(subnetMasksParts) == 2 {
			subnetMask = subnetMasksParts[1]
		}
		return subnetMask, nil
	}
	return "", fmt.Errorf("unable to select subnet mask for: IP %q and masks %q", rawIP, subnetMasks)
}
