package kubevip

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/kube-vip/kube-vip/pkg/utils"
	api "github.com/osrg/gobgp/v3/api"

	"github.com/vishvananda/netlink"
)

// Peer defines a BGP Peer
type BGPPeer struct {
	Address      string
	Port         uint16
	AS           uint32
	Password     string
	MultiHop     bool
	MpbgpNexthop string
	MpbgpIPv4    string
	MpbgpIPv6    string
}

// Config defines the BGP server configuration
type BGPConfig struct {
	AS           uint32
	RouterID     string
	SourceIP     string
	SourceIF     string
	MpbgpNexthop string
	MpbgpIPv4    string
	MpbgpIPv6    string

	HoldTime          uint64
	KeepaliveInterval uint64

	Peers []BGPPeer

	Zebra ZebraConfig
}

// Defines Zebra connection configuration. More on the topic - https://github.com/osrg/gobgp/blob/master/docs/sources/zebra.md#configuration
type ZebraConfig struct {
	Enabled      bool
	URL          string
	Version      uint32
	SoftwareName string
}

// ParseBGPPeerConfig - take a string and parses it into an array of peers
func ParseBGPPeerConfig(config string) (bgpPeers []BGPPeer, err error) {
	peers := strings.Split(config, ",")
	if len(peers) == 0 {
		return nil, fmt.Errorf("no BGP Peer configurations found")
	}

	for x := range peers {
		peerStr := peers[x]
		config := strings.Split(peerStr, "/")
		peerStr = config[0]
		if peerStr == "" {
			continue
		}
		isV6Peer := peerStr[0] == '['

		address := ""
		if isV6Peer {
			addressEndPos := strings.IndexByte(peerStr, ']')
			if addressEndPos == -1 {
				return nil, fmt.Errorf("no matching ] found for IPv6 BGP Peer")
			}
			address = peerStr[1:addressEndPos]
			peerStr = peerStr[addressEndPos+1:]
		}

		peer := strings.Split(peerStr, ":")
		if len(peer) < 2 {
			return nil, fmt.Errorf("mandatory peering params <host>:<AS> incomplete")
		}

		if !isV6Peer {
			address = peer[0]
		}

		ASNumber, err := strconv.ParseUint(peer[1], 10, 32)
		if err != nil {
			return nil, fmt.Errorf("BGP Peer AS format error [%s]", peer[1])
		}

		password := ""
		if len(peer) >= 3 {
			password = peer[2]
		}

		multiHop := false
		if len(peer) >= 4 {
			multiHop, err = strconv.ParseBool(peer[3])
			if err != nil {
				return nil, fmt.Errorf("BGP MultiHop format error (true/false) [%s]", peer[1])
			}
		}

		var port uint64
		if len(peer) >= 5 {
			port, err = strconv.ParseUint(peer[4], 10, 16)
			if err != nil {
				return nil, fmt.Errorf("BGP Peer AS format error [%s]", peer[1])
			}
		} else {
			port = 179
		}

		var mpbgpNexthop, mpbgpIPv4, mpbgpIPv6 string

		if len(config) > 1 {
			configData := strings.Split(config[1], ";")
			for _, cfg := range configData {
				c := strings.Split(cfg, "=")
				switch c[0] {
				case "mpbgp_nexthop":
					mpbgpNexthop = c[1]
				case "mpbgp_ipv4":
					mpbgpIPv4 = c[1]
				case "mpbgp_ipv6":
					mpbgpIPv6 = c[1]
				default:
					return nil, fmt.Errorf("peer configuration parameter '%s' is not supported", c[0])
				}
			}
		}

		peerConfig := BGPPeer{
			Address:      address,
			AS:           uint32(ASNumber),
			Port:         uint16(port),
			Password:     password,
			MultiHop:     multiHop,
			MpbgpNexthop: mpbgpNexthop,
			MpbgpIPv4:    mpbgpIPv4,
			MpbgpIPv6:    mpbgpIPv6,
		}

		bgpPeers = append(bgpPeers, peerConfig)
	}
	return
}

func (p *BGPPeer) FindMpbgpAddresses(ap *api.Peer, server *BGPConfig) (string, string, error) {
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
		iface, err := utils.GetInterfaceByIP(server.SourceIP)
		if err != nil {
			return "", "", fmt.Errorf("failed to get interface by IP: %v", err)
		}

		if utils.IsIPv4(server.SourceIP) {
			// Get the non link-local IPv6 address on that interface
			ipv6Address, err = utils.GetNonLinkLocalIP(iface, netlink.FAMILY_V6)
			if err != nil {
				return "", "", fmt.Errorf("failed to get non link-local IPv6 address: %v", err)
			}
		} else {
			// Get the non link-local IPv4 address on that interface
			ipv4Address, err = utils.GetNonLinkLocalIP(iface, netlink.FAMILY_V4)
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
		ipv4Address, err = utils.GetNonLinkLocalIP(&iface, netlink.FAMILY_V4)
		if err != nil {
			return "", "", fmt.Errorf("failed to get non link-local IPv4 address: %v", err)
		}

		// Get the non link-local IPv6 address on that interface
		ipv6Address, err = utils.GetNonLinkLocalIP(&iface, netlink.FAMILY_V6)
		if err != nil {
			return "", "", fmt.Errorf("failed to get non link-local IPv6 address: %v", err)
		}
	default:
		return "", "", fmt.Errorf("option %s for MP-BPG nexthop is not supported", server.MpbgpNexthop)
	}

	return ipv4Address, ipv6Address, nil
}

func (p *BGPPeer) SetMpbgpOptions(server *BGPConfig) {
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
