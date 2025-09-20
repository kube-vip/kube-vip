package bgp

import (
	"context"
	"fmt"
	"net"

	//nolint

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	api "github.com/osrg/gobgp/v3/api"

	"github.com/kube-vip/kube-vip/pkg/utils"
	"github.com/osrg/gobgp/v3/pkg/server"
	"google.golang.org/protobuf/types/known/anypb"
)

// AddPeer will add peers to the BGP configuration
func (b *Server) AddPeer(peer kubevip.BGPPeer) (err error) {
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

		peer.SetMpbgpOptions(b.c)

		ipv4Address, ipv6Address, err := peer.FindMpbgpAddresses(p, b.c)
		if err != nil {
			return fmt.Errorf("failed to get MP-BGP addresses: %w", err)
		}

		mask := "128"
		address := ipv4Address
		family := api.Family_AFI_IP
		if utils.IsIPv4(p.Conf.NeighborAddress) {
			mask = "32"
			address = ipv6Address
			family = api.Family_AFI_IP6
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

		if address != "" {
			if err := insertPolicy(b.s, address, p, family); err != nil {
				return fmt.Errorf("failed to add policy: %w", err)
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
	originAttr, _ := anypb.New(&api.OriginAttribute{
		Origin: 0,
	})

	if !isV6 {
		//nolint
		nlri, _ := anypb.New(&api.IPAddressPrefix{
			Prefix:    ip.String(),
			PrefixLen: 32,
		})

		//nolint
		nhAttr, _ := anypb.New(&api.NextHopAttribute{
			NextHop: "0.0.0.0", // gobgp will fill this
		})

		path = &api.Path{
			Family: &api.Family{
				Afi:  api.Family_AFI_IP,
				Safi: api.Family_SAFI_UNICAST,
			},
			Nlri:   nlri,
			Pattrs: []*anypb.Any{originAttr, nhAttr},
		}
	} else {
		//nolint
		nlri, _ := anypb.New(&api.IPAddressPrefix{
			Prefix:    ip.String(),
			PrefixLen: 128,
		})

		v6Family := &api.Family{
			Afi:  api.Family_AFI_IP6,
			Safi: api.Family_SAFI_UNICAST,
		}

		//nolint
		mpAttr, _ := anypb.New(&api.MpReachNLRIAttribute{
			Family:   v6Family,
			NextHops: []string{"::"}, // gobgp will fill this
			Nlris:    []*anypb.Any{nlri},
		})

		path = &api.Path{
			Family: v6Family,
			Nlri:   nlri,
			Pattrs: []*anypb.Any{originAttr, mpAttr},
		}
	}
	return
}

func insertPolicy(s *server.BgpServer, address string, p *api.Peer, family api.Family_Afi) error {
	familyType := "v4"
	if family == api.Family_AFI_IP6 {
		familyType = "v6"
	}

	setName := fmt.Sprintf("peer-%s", p.Conf.NeighborAddress)
	policyName := fmt.Sprintf("%s-%s", setName, familyType)

	policy := &api.Policy{
		Name: policyName,
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
						Name: setName,
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
						Name: setName,
					},
				},
				Actions: &api.Actions{
					RouteAction: api.RouteAction_ACCEPT,
				},
			},
		},
	}

	err := s.AddPolicy(context.Background(), &api.AddPolicyRequest{
		Policy: policy,
	})
	if err != nil {
		return fmt.Errorf("failed to add policy: %w", err)
	}

	err = s.AddPolicyAssignment(context.Background(), &api.AddPolicyAssignmentRequest{
		Assignment: &api.PolicyAssignment{
			Name:      "global",
			Direction: api.PolicyDirection_EXPORT,
			Policies: []*api.Policy{
				{
					Name: policy.Name,
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add policy assignment: %v", err)
	}

	return nil
}
