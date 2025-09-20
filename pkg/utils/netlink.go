package utils

import (
	"fmt"
	"net"

	"github.com/vishvananda/netlink"
)

// GetInterfaceByIP returns the network interface that has the specified IP address assigned.
func GetInterfaceByIP(ipAddr string) (*netlink.Link, error) {
	ip := net.ParseIP(ipAddr)
	if ip == nil {
		return nil, fmt.Errorf("invalid IP address: %s", ipAddr)
	}

	links, err := netlink.LinkList()
	if err != nil {
		return nil, fmt.Errorf("failed to list network interfaces: %v", err)
	}

	for i := range links {
		addrs, err := netlink.AddrList(links[i], netlink.FAMILY_ALL)
		if err != nil {
			return nil, fmt.Errorf("failed to list addresses for interface %s: %v", links[i].Attrs().Name, err)
		}

		for _, addr := range addrs {
			if addr.IP.Equal(ip) {
				return &links[i], nil
			}
		}
	}

	return nil, fmt.Errorf("no interface found with IP address: %s", ipAddr)
}

// GetNonLinkLocalIP returns the first non link-local IPv4/IPv6 address on the given interface.
func GetNonLinkLocalIP(iface *netlink.Link, family int) (string, error) {
	a, err := netlink.AddrList(*iface, family)
	if err != nil {
		return "", fmt.Errorf("failed to list addresses for interface %s: %v", (*iface).Attrs().Name, err)
	}

	for _, addr := range a {
		if addr.IPNet != nil {
			ip := addr.IPNet.IP
			if !ip.IsLinkLocalUnicast() {
				return ip.String(), nil
			}
		}
	}

	return "", fmt.Errorf("failed to find non-local IP on interface: %s", (*iface).Attrs().Name)
}
