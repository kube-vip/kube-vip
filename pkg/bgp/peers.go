package bgp

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/golang/protobuf/ptypes" //nolint
	"github.com/golang/protobuf/ptypes/any"
	api "github.com/osrg/gobgp/v3/api"
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

		AfiSafis: []*api.AfiSafi{
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
		},
	}

	var ipv6Address *string

	if b.c.SourceIP != "" {
		p.Transport.LocalAddress = b.c.SourceIP

		// Resolve the local interface by SourceIP
		iface, err := GetInterfaceByIP(b.c.SourceIP)
		if err != nil {
			return fmt.Errorf("failed to get interface by IP: %v", err)
		}

		// Get the non link-local IPv6 address on that interface
		ipv6, err := GetNonLinkLocalIPv6(iface)
		if err != nil {
			return fmt.Errorf("failed to get non link-local IPv6 address: %v", err)
		}

		ipv6Address = &ipv6
	}

	if b.c.SourceIF != "" {
		p.Transport.BindInterface = b.c.SourceIF

		iface, err := net.InterfaceByName(b.c.SourceIF)
		if err != nil {
			return fmt.Errorf("failed to get interface by name: %v", err)
		}

		// Get the non link-local IPv6 address on that interface
		ipv6, err := GetNonLinkLocalIPv6(iface)
		if err != nil {
			return fmt.Errorf("failed to get non link-local IPv6 address: %v", err)
		}

		ipv6Address = &ipv6
	}

	if ipv6Address != nil {
		err := b.s.AddDefinedSet(context.Background(), &api.AddDefinedSetRequest{
			DefinedSet: &api.DefinedSet{
				DefinedType: api.DefinedType_NEIGHBOR,
				Name:        fmt.Sprintf("peer-%s", p.Conf.NeighborAddress),
				List:        []string{fmt.Sprintf("%s/32", p.Conf.NeighborAddress)},
			},
		})
		if err != nil {
			return fmt.Errorf("failed to add defined set: %v", err)
		}

		err = b.s.AddPolicy(context.Background(), &api.AddPolicyRequest{
			Policy: &api.Policy{
				Name: fmt.Sprintf("peer-%s", p.Conf.NeighborAddress),
				Statements: []*api.Statement{
					{
						Conditions: &api.Conditions{
							AfiSafiIn: []*api.Family{
								{
									Afi:  api.Family_AFI_IP6,
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
								Address: *ipv6Address,
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

		err = b.s.AddPolicyAssignment(context.Background(), &api.AddPolicyAssignmentRequest{
			Assignment: &api.PolicyAssignment{
				Name:      "global",
				Direction: api.PolicyDirection_EXPORT,
				Policies: []*api.Policy{
					{
						Name: fmt.Sprintf("peer-%s", p.Conf.NeighborAddress),
					},
				},
			},
		})
		if err != nil {
			return fmt.Errorf("failed to add policy assignment: %v", err)
		}
	}

	err = b.s.AddPeer(context.Background(), &api.AddPeerRequest{
		Peer: p,
	})
	if err != nil {
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
		return nil, fmt.Errorf("No BGP Peer configurations found")
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
func GetInterfaceByIP(ipAddr string) (*net.Interface, error) {
	ip := net.ParseIP(ipAddr)
	if ip == nil {
		return nil, fmt.Errorf("invalid IP address: %s", ipAddr)
	}

	links, err := netlink.LinkList()
	if err != nil {
		return nil, fmt.Errorf("failed to list network interfaces: %v", err)
	}

	for _, link := range links {
		addrs, err := netlink.AddrList(link, netlink.FAMILY_ALL)
		if err != nil {
			return nil, fmt.Errorf("failed to list addresses for interface %s: %v", link.Attrs().Name, err)
		}

		for _, addr := range addrs {
			if addr.IP.Equal(ip) {
				iface, err := net.InterfaceByIndex(link.Attrs().Index)
				if err != nil {
					return nil, fmt.Errorf("failed to get interface by index: %v", err)
				}
				return iface, nil
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
