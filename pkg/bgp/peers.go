package bgp

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"time"

	"github.com/jpillora/backoff"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/vip"
	api "github.com/osrg/gobgp/v4/api"

	"github.com/kube-vip/kube-vip/pkg/utils"
	"github.com/osrg/gobgp/v4/pkg/apiutil"
	"github.com/osrg/gobgp/v4/pkg/config/oc"
	bgp "github.com/osrg/gobgp/v4/pkg/packet/bgp"
	"github.com/osrg/gobgp/v4/pkg/server"
)

const defaultBGPPort uint32 = 179

// AddPeer will add peers to the BGP configuration
func (b *Server) AddPeer(ctx context.Context, peer kubevip.BGPPeer) (err error) {
	p := &api.Peer{
		Conf: &api.PeerConf{
			NeighborAddress:   peer.Address,
			PeerAsn:           peer.AS,
			NeighborInterface: peer.Interface,
			AuthPassword:      peer.Password,
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
			RemotePort:    defaultBGPPort,
		},
	}

	if peer.BFDEnabled {
		p.Bfd = &api.BfdPeerConfig{
			Enabled:                  true,
			DesiredMinimumTxInterval: peer.BFDTransmitInterval,
			RequiredMinimumReceive:   peer.BFDReceiveInterval,
			DetectionMultiplier:      peer.BFDDetectMultiplier,
			Port:                     3784, // TODO: Should this be configurable??
		}
	}

	if peer.Interface != "" {
		neighborAddress, err := getIPv6LinkLocalNeighborAddress(ctx, peer.Interface)
		if err != nil {
			return fmt.Errorf("failed to get link-local address of interface %s: %w", peer.Interface, err)
		}

		p.State = &api.PeerState{
			NeighborAddress: neighborAddress,
		}
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

		mask := strconv.Itoa(vip.DefaultMaskIPv6)
		address := ipv4Address
		family := api.Family_AFI_IP
		if utils.IsIPv4(p.Conf.NeighborAddress) {
			mask = strconv.Itoa(vip.DefaultMaskIPv4)
			address = ipv6Address
			family = api.Family_AFI_IP6
		}

		err = b.s.AddDefinedSet(ctx, &api.AddDefinedSetRequest{
			DefinedSet: &api.DefinedSet{
				DefinedType: api.DefinedType_DEFINED_TYPE_NEIGHBOR,
				Name:        fmt.Sprintf("peer-%s", p.Conf.NeighborAddress),
				List:        []string{fmt.Sprintf("%s/%s", p.Conf.NeighborAddress, mask)},
			},
		})
		if err != nil {
			return fmt.Errorf("failed to add defined set: %v", err)
		}

		if address != "" {
			if err := insertPolicy(ctx, b.s, address, p, family); err != nil {
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

	if err := b.s.AddPeer(ctx, &api.AddPeerRequest{Peer: p}); err != nil {
		return fmt.Errorf("failed to add peer: %v", err)
	}
	slog.Info("[BGP]", "peer", p.Conf.NeighborAddress, "AS", p.Conf.PeerAsn, "BFD", p.Bfd)
	return nil
}

func (b *Server) getPath(ip net.IP) *apiutil.Path {
	isV6 := ip.To4() == nil

	if !isV6 {
		prefix, err := bgp.NewIPAddrPrefix(netip.MustParsePrefix(
			fmt.Sprintf("%s/%d", ip.String(), vip.DefaultMaskIPv4),
		))
		if err != nil {
			return nil
		}

		nh, err := bgp.NewPathAttributeNextHop(netip.MustParseAddr("0.0.0.0"))
		if err != nil {
			return nil
		}

		return &apiutil.Path{
			Family: bgp.RF_IPv4_UC,
			Nlri:   prefix,
			Attrs: []bgp.PathAttributeInterface{
				bgp.NewPathAttributeOrigin(0),
				nh,
			},
		}
	}

	prefix, err := bgp.NewIPAddrPrefix(netip.MustParsePrefix(
		fmt.Sprintf("%s/%d", ip.String(), vip.DefaultMaskIPv6),
	))
	if err != nil {
		return nil
	}

	mpReach, err := bgp.NewPathAttributeMpReachNLRI(
		bgp.RF_IPv6_UC,
		[]bgp.PathNLRI{{NLRI: prefix}},
		netip.MustParseAddr("::"),
	)
	if err != nil {
		return nil
	}

	return &apiutil.Path{
		Family: bgp.RF_IPv6_UC,
		Nlri:   prefix,
		Attrs: []bgp.PathAttributeInterface{
			bgp.NewPathAttributeOrigin(0),
			mpReach,
		},
	}
}
func insertPolicy(ctx context.Context, s *server.BgpServer, address string, p *api.Peer, family api.Family_Afi) error {
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
						Type: api.MatchSet_TYPE_ANY,
						Name: setName,
					},
				},
				Actions: &api.Actions{
					RouteAction: api.RouteAction_ROUTE_ACTION_ACCEPT,
					Nexthop: &api.NexthopAction{
						Address: address,
					},
				},
			},
			{
				Conditions: &api.Conditions{
					NeighborSet: &api.MatchSet{
						Type: api.MatchSet_TYPE_ANY,
						Name: setName,
					},
				},
				Actions: &api.Actions{
					RouteAction: api.RouteAction_ROUTE_ACTION_ACCEPT,
				},
			},
		},
	}

	err := s.AddPolicy(ctx, &api.AddPolicyRequest{
		Policy: policy,
	})
	if err != nil {
		return fmt.Errorf("failed to add policy: %w", err)
	}

	err = s.AddPolicyAssignment(ctx, &api.AddPolicyAssignmentRequest{
		Assignment: &api.PolicyAssignment{
			Name:      "global",
			Direction: api.PolicyDirection_POLICY_DIRECTION_EXPORT,
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

func getIPv6LinkLocalNeighborAddress(ctx context.Context, peerInterface string) (string, error) {
	neighCtx, neighCancel := context.WithTimeout(ctx, time.Minute)
	defer neighCancel()

	bo := backoff.Backoff{
		Factor: 2,
		Jitter: true,
		Min:    1 * time.Second,
		Max:    5 * time.Second,
	}

	maxAttempts := 20.0

	var err error
	for {
		select {
		case <-neighCtx.Done():
			if err != nil {
				return "", fmt.Errorf("failed to get link-local address of interface %s: %w", peerInterface, err)
			}
			return "", fmt.Errorf("failed to get link-local address of interface %s: %w", peerInterface, neighCtx.Err())
		default:
			dur := bo.Duration()
			var neighborAddress string
			neighborAddress, err = oc.GetIPv6LinkLocalNeighborAddress(peerInterface)
			if err != nil && bo.Attempt() >= maxAttempts {
				return "", fmt.Errorf("failed to get link-local address of interface %s: %w", peerInterface, err)
			}
			if neighborAddress != "" {
				return neighborAddress, nil
			}
			t := time.NewTimer(dur)
			select {
			case <-neighCtx.Done():
				t.Stop()
			case <-t.C:
			}
		}

	}
}
