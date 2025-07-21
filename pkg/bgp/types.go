package bgp

import (
	"fmt"
	"net"

	"github.com/kube-vip/kube-vip/pkg/vip"
	api "github.com/osrg/gobgp/v3/api"
	gobgp "github.com/osrg/gobgp/v3/pkg/server"
	"github.com/vishvananda/netlink"
)

// Peer defines a BGP Peer
type Peer struct {
	Address      string
	Port         uint16
	Interface    string
	AS           uint32
	Password     string
	MultiHop     bool
	MpbgpNexthop string
	MpbgpIPv4    string
	MpbgpIPv6    string
}

func (p *Peer) setMpbgpOptions(server *Config) {
	if p.MpbgpNexthop == "" {
		p.MpbgpNexthop = server.MpbgpNexthop
	}

	if p.MpbgpIPv4 == "" {
		p.MpbgpIPv4 = server.MpbgpIPv4
	}

	if p.MpbgpIPv6 == "" {
		p.MpbgpIPv6 = server.MpbgpIPv6
	}
}

func (p *Peer) findMpbgpAddresses(ap *api.Peer, server *Config) (string, string, error) {
	var ipv4Address, ipv6Address string
	switch p.MpbgpNexthop {
	case "fixed":
		ap.Transport.LocalAddress = server.SourceIP
		if p.MpbgpIPv4 == "" && p.MpbgpIPv6 == "" {
			return "", "", fmt.Errorf("to use MP-BGP with fixed address at least one IPv4 or IPv6 address has to be provided [current - IPv4: %s, IPv6: %s]",
				p.MpbgpIPv4, p.MpbgpIPv6)
		}

		if p.MpbgpIPv4 != "" {
			if net.ParseIP(p.MpbgpIPv4) == nil {
				return "", "", fmt.Errorf("provided address '%s' is not a valid IPv4 address", p.MpbgpIPv4)
			}
		}
		if p.MpbgpIPv6 != "" {
			if net.ParseIP(p.MpbgpIPv6) == nil {
				return "", "", fmt.Errorf("provided address '%s' is not a valid IPv6 address", p.MpbgpIPv6)
			}
		}

		ipv4Address = p.MpbgpIPv4
		ipv6Address = p.MpbgpIPv6
	case "auto_sourceip":
		ap.Transport.LocalAddress = server.SourceIP

		// Resolve the local interface by SourceIP
		iface, err := vip.GetInterfaceByIP(server.SourceIP)
		if err != nil {
			return "", "", fmt.Errorf("failed to get interface by IP: %v", err)
		}

		if vip.IsIPv4(server.SourceIP) {
			// Get the non link-local IPv6 address on that interface
			ipv6Address, err = vip.GetNonLinkLocalIP(iface, netlink.FAMILY_V6)
			if err != nil {
				return "", "", fmt.Errorf("failed to get non link-local IPv6 address: %v", err)
			}
		} else {
			// Get the non link-local IPv4 address on that interface
			ipv4Address, err = vip.GetNonLinkLocalIP(iface, netlink.FAMILY_V4)
			if err != nil {
				return "", "", fmt.Errorf("failed to get non link-local IPv4 address: %v", err)
			}
		}
	case "auto_sourceif":
		ap.Transport.BindInterface = server.SourceIF

		iface, err := netlink.LinkByName(server.SourceIF)
		if err != nil {
			return "", "", fmt.Errorf("failed to get interface by name: %v", err)
		}

		// Get the non link-local IPv4 address on that interface
		ipv4Address, err = vip.GetNonLinkLocalIP(&iface, netlink.FAMILY_V4)
		if err != nil {
			return "", "", fmt.Errorf("failed to get non link-local IPv4 address: %v", err)
		}

		// Get the non link-local IPv6 address on that interface
		ipv6Address, err = vip.GetNonLinkLocalIP(&iface, netlink.FAMILY_V6)
		if err != nil {
			return "", "", fmt.Errorf("failed to get non link-local IPv6 address: %v", err)
		}
	default:
		return "", "", fmt.Errorf("option %s for MP-BPG nexthop is not supported", server.MpbgpNexthop)
	}

	return ipv4Address, ipv6Address, nil
}

// Config defines the BGP server configuration
type Config struct {
	AS           uint32
	RouterID     string
	SourceIP     string
	SourceIF     string
	MpbgpNexthop string
	MpbgpIPv4    string
	MpbgpIPv6    string

	HoldTime          uint64
	KeepaliveInterval uint64

	Peers []Peer

	Zebra ZebraConfig
}

// Defines Zebra connection configuration. More on the topic - https://github.com/osrg/gobgp/blob/master/docs/sources/zebra.md#configuration
type ZebraConfig struct {
	Enabled      bool
	URL          string
	Version      uint32
	SoftwareName string
}

// Server manages a server object
type Server struct {
	s *gobgp.BgpServer
	c *Config
}
