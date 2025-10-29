package vip

import (
	"context"
	"crypto/rand"
	"fmt"
	"net"
	"strings"
	"syscall"

	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/utils"
	"github.com/pkg/errors"
	"github.com/vishvananda/netlink"
)

// getHostName return the hostname from the fqdn
func getHostName(dnsName string) string {
	if dnsName == "" {
		return ""
	}

	fields := strings.Split(dnsName, ".")
	return fields[0]
}

// GetDefaultGatewayInterface return default gateway interface link
func GetDefaultGatewayInterface() (*net.Interface, error) {
	routes, err := netlink.RouteList(nil, syscall.AF_INET)
	if err != nil {
		return nil, err
	}

	routes6, err := netlink.RouteList(nil, syscall.AF_INET6)
	if err != nil {
		return nil, err
	}

	routes = append(routes, routes6...)

	for _, route := range routes {
		if route.Dst == nil || route.Dst.String() == "0.0.0.0/0" || route.Dst.String() == "::/0" {
			if route.LinkIndex <= 0 {
				return nil, errors.New("Found default route but could not determine interface")
			}
			return net.InterfaceByIndex(route.LinkIndex)
		}
	}

	return nil, errors.New("Unable to find default route")
}

// MonitorDefaultInterface monitor the default interface and catch the event of the default route
func MonitorDefaultInterface(ctx context.Context, defaultIF *net.Interface) error {
	routeCh := make(chan netlink.RouteUpdate)
	if err := netlink.RouteSubscribe(routeCh, ctx.Done()); err != nil {
		return fmt.Errorf("subscribe route failed, error: %w", err)
	}

	for {
		select {
		case r := <-routeCh:
			log.Debug(fmt.Sprintf("type: %d, route: %+v", r.Type, r.Route))
			if r.Type == syscall.RTM_DELROUTE && (r.Dst == nil || r.Dst.String() == "0.0.0.0/0") && r.LinkIndex == defaultIF.Index {
				return fmt.Errorf("default route deleted and the default interface may be invalid")
			}
		case <-ctx.Done():
			return nil
		}
	}
}

func GenerateMac() (mac string) {
	buf := make([]byte, 3)
	_, err := rand.Read(buf)
	if err != nil {
		return
	}

	/**
	 * The first 3 bytes need to match a real manufacturer
	 * you can refer to the following lists for examples:
	 * - https://gist.github.com/aallan/b4bb86db86079509e6159810ae9bd3e4
	 * - https://macaddress.io/database-download
	 */
	mac = fmt.Sprintf("%s:%s:%s:%02x:%02x:%02x", "00", "00", "6C", buf[0], buf[1], buf[2])
	log.Info("Generated mac", "address", mac)
	return mac
}

func Split(values string) []string {
	result := strings.Split(values, ",")
	for i := range result {
		result[i] = strings.TrimSpace(result[i])
	}
	return result
}

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

func selectSubnet(address string, subnets []string) string {
	subnet := ""
	if utils.IsIPv4(address) {
		if subnets[0] != "" {
			subnet = subnets[0]
		} else {
			subnet = "32"
		}
	} else {
		if len(subnets) > 1 && subnets[1] != "" {
			subnet = subnets[1]
		} else {
			subnet = "128"
		}
	}

	return subnet
}
