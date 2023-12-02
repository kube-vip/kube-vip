package vip

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	v1 "k8s.io/api/core/v1"

	"github.com/kube-vip/kube-vip/pkg/iptables"
)

const (
	defaultValidLft                 = 60
	iptablesComment                 = "%s kube-vip load balancer IP"
	ignoreServiceSecurityAnnotation = "kube-vip.io/ignore-service-security"
)

// Network is an interface that enable managing operations for a given IP
type Network interface {
	AddIP() error
	AddRoute() error
	DeleteIP() error
	DeleteRoute() error
	IsSet() (bool, error)
	IP() string
	SetIP(ip string) error
	SetServicePorts(service *v1.Service)
	Interface() string
	IsDADFAIL() bool
	IsDNS() bool
	IsDDNS() bool
	DDNSHostName() string
	DNSName() string
}

// network - This allows network configuration
type network struct {
	mu sync.Mutex

	address        *netlink.Addr
	link           netlink.Link
	ports          []v1.ServicePort
	serviceName    string
	ignoreSecurity bool

	dnsName string
	isDDNS  bool

	routeTable int
}

func netlinkParse(addr string) (*netlink.Addr, error) {
	mask, err := GetFullMask(addr)
	if err != nil {
		return nil, err
	}
	return netlink.ParseAddr(addr + mask)
}

// NewConfig will attempt to provide an interface to the kernel network configuration
func NewConfig(address string, iface string, subnet string, isDDNS bool, tableID int) (Network, error) {
	result := &network{}

	link, err := netlink.LinkByName(iface)
	if err != nil {
		return result, errors.Wrapf(err, "could not get link for interface '%s'", iface)
	}

	result.link = link
	result.routeTable = tableID

	if IsIP(address) {
		// Check if the subnet needs overriding
		if subnet != "" {
			result.address, err = netlink.ParseAddr(address + subnet)
			if err != nil {
				return result, errors.Wrapf(err, "could not parse address '%s'", address)
			}
		} else {
			result.address, err = netlinkParse(address)
			if err != nil {
				return result, errors.Wrapf(err, "could not parse address '%s'", address)
			}
		}
		// Ensure we don't have a global address on loopback
		if iface == "lo" {
			result.address.Scope = unix.RT_SCOPE_HOST
		}
		return result, nil
	}

	// address is DNS
	result.isDDNS = isDDNS
	result.dnsName = address
	// try to resolve the address
	ip, err := lookupHost(address)
	if err != nil {
		// return early for ddns if no IP is allocated for the domain
		// when leader starts, should do get IP from DHCP for the domain
		if isDDNS {
			return result, nil
		}
		return nil, err
	}

	// we're able to resolve store this as the initial IP
	if result.address, err = netlinkParse(ip); err != nil {
		return result, err
	}
	// set ValidLft so that the VIP expires if the DNS entry is updated, otherwise it'll be refreshed by the DNS prober
	result.address.ValidLft = defaultValidLft

	return result, err
}

// AddRoute - Add an IP address to a route table
func (configurator *network) AddRoute() error {
	route := &netlink.Route{
		Scope:     netlink.SCOPE_UNIVERSE,
		Dst:       configurator.address.IPNet,
		LinkIndex: configurator.link.Attrs().Index,
		Table:     configurator.routeTable,
	}
	return netlink.RouteAdd(route)
}

// AddRoute - Add an IP address to a route table
func (configurator *network) DeleteRoute() error {
	route := &netlink.Route{
		Scope:     netlink.SCOPE_UNIVERSE,
		Dst:       configurator.address.IPNet,
		LinkIndex: configurator.link.Attrs().Index,
		Table:     configurator.routeTable,
	}
	return netlink.RouteDel(route)
}

// AddIP - Add an IP address to the interface
func (configurator *network) AddIP() error {
	if err := netlink.AddrReplace(configurator.link, configurator.address); err != nil {
		return errors.Wrap(err, "could not add ip")
	}

	if os.Getenv("enable_service_security") == "true" && !configurator.ignoreSecurity {
		if err := configurator.addIptablesRulesToLimitTrafficPorts(); err != nil {
			return errors.Wrap(err, "could not add iptables rules to limit traffic ports")
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
	log.Debugf("add iptables rules, vip: %s, ports: %+v", vip, configurator.ports)
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

	log.Debugf("remove iptables rules, vip: %s, ports: %+v", vip, configurator.ports)
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
func (configurator *network) DeleteIP() error {
	result, err := configurator.IsSet()
	if err != nil {
		return errors.Wrap(err, "ip check in DeleteIP failed")
	}

	// Nothing to delete
	if !result {
		return nil
	}

	if err = netlink.AddrDel(configurator.link, configurator.address); err != nil {
		return errors.Wrap(err, "could not delete ip")
	}

	if os.Getenv("enable_service_security") == "true" && !configurator.ignoreSecurity {
		if err := configurator.removeIptablesRuleToLimitTrafficPorts(); err != nil {
			return errors.Wrap(err, "could not remove iptables rules to limit traffic ports")
		}
	}

	return nil
}

// IsDADFAIL - Returns true if the address is IPv6 and has DADFAILED flag
func (configurator *network) IsDADFAIL() bool {
	if configurator.address == nil || !IsIPv6(configurator.address.IP.String()) {
		return false
	}

	// Get all the address
	addresses, err := netlink.AddrList(configurator.link, netlink.FAMILY_V6)
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

// IsSet - Check to see if VIP is set
func (configurator *network) IsSet() (result bool, err error) {
	var addresses []netlink.Addr

	if configurator.address == nil {
		return false, nil
	}

	addresses, err = netlink.AddrList(configurator.link, 0)
	if err != nil {
		err = errors.Wrap(err, "could not list addresses")

		return
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

	addr, err := netlinkParse(ip)
	if err != nil {
		return err
	}
	if configurator.address != nil && configurator.IsDNS() {
		addr.ValidLft = defaultValidLft
	}
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
	configurator.ignoreSecurity = service.Annotations[ignoreServiceSecurityAnnotation] == "true"
}

// IP - return the IP Address
func (configurator *network) IP() string {
	configurator.mu.Lock()
	defer configurator.mu.Unlock()

	return configurator.address.IP.String()
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
	return configurator.link.Attrs().Name
}

func GarbageCollect(adapter, address string) (found bool, err error) {

	// Get adapter
	link, err := netlink.LinkByName(adapter)
	if err != nil {
		return true, errors.Wrapf(err, "could not get link for interface '%s'", adapter)
	}

	// Get addresses on adapter
	addrs, err := netlink.AddrList(link, netlink.FAMILY_ALL)
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
			if err = netlink.AddrDel(link, &existing); err != nil {
				return true, errors.Wrap(err, "could not delete ip")
			}
		}
	}
	return // Didn't find the address on the adapter
}
