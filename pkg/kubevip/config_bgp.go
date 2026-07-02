package kubevip

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/kube-vip/kube-vip/pkg/utils"
	api "github.com/osrg/gobgp/v4/api"

	"github.com/vishvananda/netlink"
)

// Peer defines a BGP Peer
type BGPPeer struct {
	Address      string
	Port         uint16
	Interface    string
	AS           uint32
	Password     string
	MultiHop     bool
	MpbgpNexthop string
	MpbgpIPv4    string
	MpbgpIPv6    string

	// BFD Configuration
	BFDEnabled          bool
	BFDReceiveInterval  uint32
	BFDTransmitInterval uint32
	BFDDetectMultiplier uint32
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

// BGP Peer layout is as follows:
// <address>:<AS>:<password>:<multihop>:<port>:<optional mpbgp options>:<BFD options>

// <address> - IP address of the peer. For IPv6 addresses, the address should be enclosed in square brackets (e.g. [fd00:100:64::2]). For unnumbered peers, the address should be prefixed with "unnumbered:" followed by the interface name (e.g. unnumbered:eth0).
// <AS> - Autonomous System number of the peer (e.g. 65000)
// <password> - Optional password for BGP authentication (e.g. secret)
// <multihop> - Optional flag to indicate if this is a multihop peer (true/false, default: false)
// <port> - Optional BGP port number (default: 179)
// <optional mpbgp options> - Optional MP-BGP parameters in the format of key=value pairs separated by ';' (e.g. mpbgp_nexthop=auto_sourceif;mpbgp_ipv4=)
// <BFD options> - Optional BFD parameters (if any) in the format of semicolon-separated values (enable, receive_interval, transmit_interval, detect_multiplier) (e.g. true;300;300;3)

// ParseBGPPeerConfig - take a string and parses it into an array of peers
func ParseBGPPeerConfig(config string) (bgpPeers []BGPPeer, err error) {
	peers := strings.Split(config, ",")
	if len(peers) == 0 || config == "" {
		return nil, fmt.Errorf("no BGP Peer configurations found")
	}

	for x := range peers {
		peerStr := peers[x]
		if peerStr == "" {
			continue
		}

		// Look at address peer
		isV6Peer := peerStr[0] == '['
		isUnnumberedPeer := strings.HasPrefix(peerStr, "unnumbered:")

		address := ""
		if isV6Peer {
			addressEndPos := strings.IndexByte(peerStr, ']')
			if addressEndPos == -1 {
				return nil, fmt.Errorf("no matching ] found for IPv6 BGP Peer")
			}
			address = peerStr[1:addressEndPos]
			peerStr = peerStr[addressEndPos+1:]
		} else if isUnnumberedPeer {
			unnumberedEndPos := strings.IndexByte(peerStr, ':')
			peerStr = peerStr[unnumberedEndPos+1:]
		}

		peer := strings.Split(peerStr, ":")
		if len(peer) < 2 && !isUnnumberedPeer {
			return nil, fmt.Errorf("mandatory peering params <host>:<AS> incomplete")
		}

		iface := ""
		if isUnnumberedPeer {
			iface = peer[0]
		} else if !isV6Peer {
			address = peer[0]
		}

		// Look at peer[1] for AS number
		var ASNumber uint64
		if len(peer) >= 2 {
			ASNumber, err = strconv.ParseUint(peer[1], 10, 32)
			if err != nil {
				return nil, fmt.Errorf("BGP Peer AS format error [%s]", peer[1])
			}
		}

		// Look at peer[2] for password
		password := ""
		if len(peer) >= 3 {
			password = peer[2]
		}

		// Look at peer[3] for multihop
		multiHop := false
		if len(peer) >= 4 && peer[3] != "" {
			multiHop, err = strconv.ParseBool(peer[3])
			if err != nil {
				return nil, fmt.Errorf("BGP MultiHop format error (true/false) [%s]", peer[3])
			}
		}

		// Look at peer[4] for BGP port
		var port uint64
		if len(peer) >= 5 {
			if peer[4] == "" {
				port = 179
			} else {
				port, err = strconv.ParseUint(peer[4], 10, 16)
				if err != nil {
					return nil, fmt.Errorf("BGP Peer Port format error [%s]", peer[4])
				}
			}
		} else if !isUnnumberedPeer {
			port = 179
		}

		// Look at peer[5] for optional MP-BGP parameters
		var mpbgpNexthop, mpbgpIPv4, mpbgpIPv6 string

		if len(peer) >= 6 && peer[5] != "" {
			configData := strings.Split(peer[5], ";")
			for _, cfg := range configData {
				c := strings.Split(cfg, "=")
				if len(c) < 2 {
					return nil, fmt.Errorf("peer configuration parameter '%s' is missing a value (expected key=value)", c[0])
				}
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

		// Look at peer[6] for optional BFD parameters (if any)
		bfdEnabled := false
		bfdReceiveInterval := uint64(300)
		bfdTransmitInterval := uint64(300)
		bfdDetectMultiplier := uint64(3)

		if len(peer) >= 7 && peer[6] != "" {
			c := strings.Split(peer[6], ";")
			if len(c) < 4 {
				return nil, fmt.Errorf("BFD configuration error: at least 4 parameters are required (enable, receive_interval, transmit_interval, detect_multiplier) [%s]", peer[6])
			}
			bfdEnabled, err = strconv.ParseBool(c[0])
			if err != nil {
				return nil, fmt.Errorf("BFD configuration error: invalid value for bfd_enabled (true/false) [%s]", c[0])
			}

			if c[1] != "" {
				bfdReceiveInterval, err = strconv.ParseUint(c[1], 10, 32)
				if err != nil {
					return nil, fmt.Errorf("BFD configuration error: invalid value for bfd_receive_interval [%s]", c[1])
				}
			}

			if c[2] != "" {
				bfdTransmitInterval, err = strconv.ParseUint(c[2], 10, 32)
				if err != nil {
					return nil, fmt.Errorf("BFD configuration error: invalid value for bfd_transmit_interval [%s]", c[2])
				}
			}
			if c[3] != "" {
				bfdDetectMultiplier, err = strconv.ParseUint(c[3], 10, 32)
				if err != nil {
					return nil, fmt.Errorf("BFD configuration error: invalid value for bfd_detect_multiplier [%s]", c[3])
				}
			}
		}

		peerConfig := BGPPeer{
			Address: address,
			//nolint:gosec // previously parsed into uint32
			AS:                  uint32(ASNumber),
			Port:                uint16(port),
			Interface:           iface,
			Password:            password,
			MultiHop:            multiHop,
			MpbgpNexthop:        mpbgpNexthop,
			MpbgpIPv4:           mpbgpIPv4,
			MpbgpIPv6:           mpbgpIPv6,
			BFDEnabled:          bfdEnabled,
			BFDReceiveInterval:  uint32(bfdReceiveInterval),
			BFDTransmitInterval: uint32(bfdTransmitInterval),
			BFDDetectMultiplier: uint32(bfdDetectMultiplier),
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
