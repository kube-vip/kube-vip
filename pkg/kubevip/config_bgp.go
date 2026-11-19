package kubevip

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	"github.com/kube-vip/kube-vip/pkg/utils"
	api "github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/packet/bmp"

	"github.com/vishvananda/netlink"
)

// Peer defines a BGP Peer
type BGPPeer struct {
	Address      string `yaml:"address"`
	Port         uint16 `yaml:"port"`
	Interface    string `yaml:"interface"`
	AS           uint32 `yaml:"as"`
	Password     string `yaml:"password"`
	MultiHop     bool   `yaml:"multiHop"`
	MpbgpNexthop string `yaml:"mpbgpNexthop"`
	MpbgpIPv4    string `yaml:"mpbgpIPv4"`
	MpbgpIPv6    string `yaml:"mpbgpIPv6"`

	// BFD Configuration
	BFDEnabled          bool   `yaml:"bfdEnabled"`
	BFDReceiveInterval  uint32 `yaml:"bfdReceiveInterval"`
	BFDTransmitInterval uint32 `yaml:"bfdTransmitInterval"`
	BFDDetectMultiplier uint32 `yaml:"bfdDetectMultiplier"`
}

// Config defines the BGP server configuration
type BGPConfig struct {
	AS           uint32 `yaml:"as"`
	RouterID     string `yaml:"routerID"`
	SourceIP     string `yaml:"sourceIP"`
	SourceIF     string `yaml:"sourceIF"`
	MpbgpNexthop string `yaml:"mpbgpNexthop"`
	MpbgpIPv4    string `yaml:"mpbgpIPv4"`
	MpbgpIPv6    string `yaml:"mpbgpIPv6"`

	HoldTime          uint64 `yaml:"holdTime"`
	KeepaliveInterval uint64 `yaml:"keepaliveInterval"`

	Peers []BGPPeer `yaml:"peers"`

	Zebra ZebraConfig `yaml:"zebra"`

	BMP BMPConfig `yaml:"bmp"`
}

// Defines Zebra connection configuration. More on the topic - https://github.com/osrg/gobgp/blob/master/docs/sources/zebra.md#configuration
type ZebraConfig struct {
	Enabled      bool   `yaml:"enabled"`
	URL          string `yaml:"url"`
	Version      uint32 `yaml:"version"`
	SoftwareName string `yaml:"softwareName"`
}

// BMPConfig defines the BGP Monitoring Protocol (RFC 7854) stations that the
// BGP server reports its peers and routes to.
type BMPConfig struct {
	// Servers are the BMP stations, each written as <IPv4 address>[:<port>] or
	// [<IPv6 address>][:<port>]. The port defaults to 11019.
	Servers []string `yaml:"servers"`
	// Policy selects the routes reported in Route Monitoring messages, one of
	// pre-policy, post-policy, local-rib or all. Defaults to local-rib.
	Policy string `yaml:"policy"`
	// StatisticsTimeout is the interval in seconds between Statistics Reports,
	// 0 disables them. Other values must be between 15 and 65535.
	StatisticsTimeout uint16 `yaml:"statisticsTimeout"`
}

const (
	// BMPPolicyPrePolicy reports the routes received from each peer before import policy is applied
	BMPPolicyPrePolicy = "pre-policy"
	// BMPPolicyPostPolicy reports the routes received from each peer after import policy is applied
	BMPPolicyPostPolicy = "post-policy"
	// BMPPolicyLocalRIB reports the best routes in the local RIB, including the VIP routes kube-vip advertises
	BMPPolicyLocalRIB = "local-rib"
	// BMPPolicyAll reports pre-policy, post-policy and local-rib routes
	BMPPolicyAll = "all"

	// bmpMinStatisticsTimeout is the shortest Statistics Report interval GoBGP accepts
	bmpMinStatisticsTimeout = 15
)

var bmpPolicies = map[string]api.AddBmpRequest_MonitoringPolicy{
	BMPPolicyPrePolicy:  api.AddBmpRequest_MONITORING_POLICY_PRE,
	BMPPolicyPostPolicy: api.AddBmpRequest_MONITORING_POLICY_POST,
	BMPPolicyLocalRIB:   api.AddBmpRequest_MONITORING_POLICY_LOCAL,
	BMPPolicyAll:        api.AddBmpRequest_MONITORING_POLICY_ALL,
}

// ParseBMPServers converts BMP stations written as <IPv4 address>[:<port>] or
// [<IPv6 address>][:<port>] into their addresses, using port 11019 when none
// is given. Surrounding whitespace is ignored and duplicates are rejected.
func ParseBMPServers(servers []string) ([]netip.AddrPort, error) {
	addresses := make([]netip.AddrPort, 0, len(servers))
	for _, server := range servers {
		address, err := parseBMPServer(strings.TrimSpace(server))
		if err != nil {
			return nil, err
		}
		for _, existing := range addresses {
			if existing == address {
				return nil, fmt.Errorf("BMP server %s is configured more than once", address)
			}
		}
		addresses = append(addresses, address)
	}
	return addresses, nil
}

func parseBMPServer(server string) (netip.AddrPort, error) {
	if address, err := netip.ParseAddrPort(server); err == nil {
		if address.Port() == 0 {
			return netip.AddrPort{}, fmt.Errorf("BMP server %q is invalid, the port must be between 1 and 65535", server)
		}
		return address, nil
	}

	host := server
	bracketed := strings.HasPrefix(server, "[") && strings.HasSuffix(server, "]")
	if bracketed {
		host = server[1 : len(server)-1]
	}
	address, err := netip.ParseAddr(host)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("BMP server %q is invalid, expected <IPv4 address>[:<port>] or [<IPv6 address>][:<port>]", server)
	}
	if address.Is6() != bracketed {
		return netip.AddrPort{}, fmt.Errorf("BMP server %q is invalid, IPv6 addresses must be enclosed in square brackets and IPv4 addresses must not", server)
	}
	return netip.AddrPortFrom(address, bmp.BMP_DEFAULT_PORT), nil
}

// ParseBMPPolicy converts a BMP route monitoring policy name into its GoBGP
// value. An empty policy selects local-rib.
func ParseBMPPolicy(policy string) (api.AddBmpRequest_MonitoringPolicy, error) {
	if policy == "" {
		policy = BMPPolicyLocalRIB
	}
	value, ok := bmpPolicies[strings.ToLower(strings.TrimSpace(policy))]
	if !ok {
		return 0, fmt.Errorf("BMP route monitoring policy %q is invalid, expected one of %s, %s, %s or %s",
			policy, BMPPolicyPrePolicy, BMPPolicyPostPolicy, BMPPolicyLocalRIB, BMPPolicyAll)
	}
	return value, nil
}

// Parse validates the BMP settings and returns the stations and the route
// monitoring policy to configure the BGP server with.
func (c BMPConfig) Parse() ([]netip.AddrPort, api.AddBmpRequest_MonitoringPolicy, error) {
	servers, err := ParseBMPServers(c.Servers)
	if err != nil {
		return nil, 0, err
	}
	policy, err := ParseBMPPolicy(c.Policy)
	if err != nil {
		return nil, 0, err
	}
	if c.StatisticsTimeout != 0 && c.StatisticsTimeout < bmpMinStatisticsTimeout {
		return nil, 0, fmt.Errorf("BMP statistics timeout %d is invalid, it must be 0 to disable Statistics Reports or between %d and 65535 seconds",
			c.StatisticsTimeout, bmpMinStatisticsTimeout)
	}
	return servers, policy, nil
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

	mode := server.MpbgpNexthop
	if p.MpbgpNexthop != "" {
		mode = p.MpbgpNexthop
	}

	switch mode {
	case "fixed":
		ap.Transport.LocalAddress = server.SourceIP

		ipv4 := server.MpbgpIPv4
		if p.MpbgpIPv4 != "" {
			ipv4 = p.MpbgpIPv4
		}

		ipv6 := server.MpbgpIPv6
		if p.MpbgpIPv6 != "" {
			ipv6 = p.MpbgpIPv6
		}

		if ipv4 == "" && ipv6 == "" {
			return "", "", fmt.Errorf("to use MP-BGP with fixed address at least one IPv4 or IPv6 address has to be provided [current - IPv4: %s, IPv6: %s]",
				ipv4, ipv6)
		}

		if ipv4 != "" {
			if !utils.IsIPv4(ipv4) {
				return "", "", fmt.Errorf("provided address '%s' is not a valid IPv4 address", ipv4)
			}
		}
		if ipv6 != "" {
			if !utils.IsIPv6(ipv6) {
				return "", "", fmt.Errorf("provided address '%s' is not a valid IPv6 address", ipv6)
			}
		}

		ipv4Address = ipv4
		ipv6Address = ipv6
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
		return "", "", fmt.Errorf("option %q for MP-BPG nexthop is not supported", mode)
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
