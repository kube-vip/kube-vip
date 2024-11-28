package bgp

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/golang/protobuf/ptypes" //nolint
	"github.com/golang/protobuf/ptypes/any"
	"github.com/kube-vip/kube-vip/pkg/vip"
	api "github.com/osrg/gobgp/v3/api"
	"github.com/osrg/gobgp/v3/pkg/server"
	"github.com/vishvananda/netlink"
)

// AddPeer will add peers to the BGP configuration
func (b *Server) AddPeer(peer Peer) (err error) {
	p := &api.Peer{
		Conf: &api.PeerConf{
			NeighborAddress: peer.Address,
			PeerAsn:         peer.AS,
			AuthPassword:    peer.Password,
		},

		Timers: &api.Timers{
			Config: &api.TimersConfig{
				ConnectRetry:      10,
				HoldTime:          b.c.HoldTime,
				KeepaliveInterval: b.c.KeepaliveInterval,
			},
		},

		// This enables routes to be sent to routers across multiple hops
		EbgpMultihop: &api.EbgpMultihop{
			Enabled:     peer.MultiHop,
			MultihopTtl: 50,
		},

		Transport: &api.Transport{
			MtuDiscovery:  true,
			RemoteAddress: peer.Address,
			RemotePort:    uint32(179),
		},
	}

	var ipv6Address string
	var ipv4Address string

	if b.c.MpbgpNexthop != "" {
		p.AfiSafis = []*api.AfiSafi{
			{
				Config: &api.AfiSafiConfig{
					Family: &api.Family{
						Afi:  api.Family_AFI_IP,
						Safi: api.Family_SAFI_UNICAST,
					},
					Enabled: true,
				},
			},
			{
				Config: &api.AfiSafiConfig{
					Family: &api.Family{
						Afi:  api.Family_AFI_IP6,
						Safi: api.Family_SAFI_UNICAST,
					},
					Enabled: true,
				},
			},
		}

		switch b.c.MpbgpIPv4 {
		case "fixed":
			if b.c.MpbgpIPv4 == "" || b.c.MpbgpIPv6 == "" {
				return fmt.Errorf("to use MP-BGP with fixed address both IPv4 and IPv6 has to be provided [current - IPv4: %s, IPv6: %s]",
					b.c.MpbgpIPv4, b.c.MpbgpIPv6)
			}
			ipv4Address = b.c.MpbgpIPv4
			ipv6Address = b.c.MpbgpIPv6
		case "auto_sourceip":
			p.Transport.LocalAddress = b.c.SourceIP

			// Resolve the local interface by SourceIP
			iface, err := GetInterfaceByIP(b.c.SourceIP)
			if err != nil {
				return fmt.Errorf("failed to get interface by IP: %v", err)
			}

			if vip.IsIPv4(b.c.SourceIP) {
				// Get the non link-local IPv6 address on that interface
				ipv6Address, err = GetNonLinkLocalIP(iface, netlink.FAMILY_V6)
				if err != nil {
					return fmt.Errorf("failed to get non link-local IPv6 address: %v", err)
				}
			} else {
				// Get the non link-local IPv4 address on that interface
				ipv6Address, err = GetNonLinkLocalIP(iface, netlink.FAMILY_V4)
				if err != nil {
					return fmt.Errorf("failed to get non link-local IPv4 address: %v", err)
				}
			}
		case "auto_sourceif":
			p.Transport.BindInterface = b.c.SourceIF

			iface, err := netlink.LinkByName(b.c.SourceIF)
			if err != nil {
				return fmt.Errorf("failed to get interface by name: %v", err)
			}

			// Get the non link-local IPv4 address on that interface
			ipv4Address, err = GetNonLinkLocalIP(&iface, netlink.FAMILY_V4)
			if err != nil {
				return fmt.Errorf("failed to get non link-local IPv6 address: %v", err)
			}

			// Get the non link-local IPv6 address on that interface
			ipv6Address, err = GetNonLinkLocalIP(&iface, netlink.FAMILY_V6)
			if err != nil {
				return fmt.Errorf("failed to get non link-local IPv6 address: %v", err)
			}
		default:
			return fmt.Errorf("option %s for MP-BPG nexthop is not supported", b.c.MpbgpNexthop)
		}

		mask := "128"
		if vip.IsIPv4(p.Conf.NeighborAddress) {
			mask = "32"
		}

		err = b.s.AddDefinedSet(context.Background(), &api.AddDefinedSetRequest{
			DefinedSet: &api.DefinedSet{
				DefinedType: api.DefinedType_NEIGHBOR,
				Name:        fmt.Sprintf("peer-%s", p.Conf.NeighborAddress),
				List:        []string{fmt.Sprintf("%s/%s", p.Conf.NeighborAddress, mask)},
			},
		})
		if err != nil {
			return fmt.Errorf("failed to add defined set: %v", err)
		}

		if ipv6Address != "" {
			if err := insertPolicy(b.s, ipv6Address, p, api.Family_AFI_IP6); err != nil {
				return fmt.Errorf("failed to add IPv6 policy: %w", err)
			}
		}

		if ipv4Address != "" {
			if err := insertPolicy(b.s, ipv4Address, p, api.Family_AFI_IP); err != nil {
				return fmt.Errorf("failed to add IPv6 policy: %w", err)
			}
		}
	} else {
		if b.c.SourceIP != "" {
			p.Transport.LocalAddress = b.c.SourceIP
		}

		if b.c.SourceIF != "" {
			p.Transport.BindInterface = b.c.SourceIF
		}
	}

	if err := b.s.AddPeer(context.Background(), &api.AddPeerRequest{Peer: p}); err != nil {
		return fmt.Errorf("failed to add peer: %v", err)
	}

	return nil
}

func (b *Server) getPath(ip net.IP) (path *api.Path) {
	isV6 := ip.To4() == nil

	//nolint
	originAttr, _ := ptypes.MarshalAny(&api.OriginAttribute{
		Origin: 0,
	})

	if !isV6 {
		//nolint
		nlri, _ := ptypes.MarshalAny(&api.IPAddressPrefix{
			Prefix:    ip.String(),
			PrefixLen: 32,
		})

		//nolint
		nhAttr, _ := ptypes.MarshalAny(&api.NextHopAttribute{
			NextHop: "0.0.0.0", // gobgp will fill this
		})

		path = &api.Path{
			Family: &api.Family{
				Afi:  api.Family_AFI_IP,
				Safi: api.Family_SAFI_UNICAST,
			},
			Nlri:   nlri,
			Pattrs: []*any.Any{originAttr, nhAttr},
		}
	} else {
		//nolint
		nlri, _ := ptypes.MarshalAny(&api.IPAddressPrefix{
			Prefix:    ip.String(),
			PrefixLen: 128,
		})

		v6Family := &api.Family{
			Afi:  api.Family_AFI_IP6,
			Safi: api.Family_SAFI_UNICAST,
		}

		//nolint
		mpAttr, _ := ptypes.MarshalAny(&api.MpReachNLRIAttribute{
			Family:   v6Family,
			NextHops: []string{"::"}, // gobgp will fill this
			Nlris:    []*any.Any{nlri},
		})

		path = &api.Path{
			Family: v6Family,
			Nlri:   nlri,
			Pattrs: []*any.Any{originAttr, mpAttr},
		}
	}
	return
}

// ParseBGPPeerConfig - take a string and parses it into an array of peers
func ParseBGPPeerConfig(config string) (bgpPeers []Peer, err error) {
	peers := strings.Split(config, ",")
	if len(peers) == 0 {
		return nil, fmt.Errorf("no BGP Peer configurations found")
	}

	for x := range peers {
		peerStr := peers[x]
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

		peerConfig := Peer{
			Address:  address,
			AS:       uint32(ASNumber),
			Password: password,
			MultiHop: multiHop,
		}

		bgpPeers = append(bgpPeers, peerConfig)
	}
	return
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

// GetNonLinkLocalIPv6 returns the first non link-local IPv6 address on the given interface.
func GetNonLinkLocalIPv6(iface *net.Interface) (string, error) {
	addrs, err := iface.Addrs()
	if err != nil {
		return "", fmt.Errorf("failed to list addresses for interface %s: %v", iface.Name, err)
	}

	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok {
			continue
		}

		ip := ipNet.IP
		if ip.To4() == nil && !ip.IsLinkLocalUnicast() {
			return ip.String(), nil
		}
	}

	return "", fmt.Errorf("no non link-local IPv6 address found on interface %s", iface.Name)
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

func insertPolicy(s *server.BgpServer, address string, p *api.Peer, family api.Family_Afi) error {
	err := s.AddPolicy(context.Background(), &api.AddPolicyRequest{
		Policy: &api.Policy{
			Name: fmt.Sprintf("peer-%s-v6", p.Conf.NeighborAddress),
			Statements: []*api.Statement{
				{
					Conditions: &api.Conditions{
						AfiSafiIn: []*api.Family{
							{
								Afi:  family,
								Safi: api.Family_SAFI_UNICAST,
							},
						},
						NeighborSet: &api.MatchSet{
							Type: api.MatchSet_ANY,
							Name: fmt.Sprintf("peer-%s", p.Conf.NeighborAddress),
						},
					},
					Actions: &api.Actions{
						RouteAction: api.RouteAction_ACCEPT,
						Nexthop: &api.NexthopAction{
							Address: address,
						},
					},
				},
				{
					Conditions: &api.Conditions{
						NeighborSet: &api.MatchSet{
							Type: api.MatchSet_ANY,
							Name: fmt.Sprintf("peer-%s", p.Conf.NeighborAddress),
						},
					},
					Actions: &api.Actions{
						RouteAction: api.RouteAction_ACCEPT,
					},
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add policy: %v", err)
	}

	policyType := "v4"
	if family == api.Family_AFI_IP6 {
		policyType = "v6"
	}

	err = s.AddPolicyAssignment(context.Background(), &api.AddPolicyAssignmentRequest{
		Assignment: &api.PolicyAssignment{
			Name:      "global",
			Direction: api.PolicyDirection_EXPORT,
			Policies: []*api.Policy{
				{
					Name: fmt.Sprintf("peer-%s-%s", p.Conf.NeighborAddress, policyType),
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add policy assignment: %v", err)
	}

	return nil
}
