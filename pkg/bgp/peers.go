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
				ConnectRetry: 10,
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

	if b.c.SourceIP != "" {
		p.Transport.LocalAddress = b.c.SourceIP
	}

	if b.c.SourceIF != "" {
		p.Transport.BindInterface = b.c.SourceIF
	}

	return b.s.AddPeer(context.Background(), &api.AddPeerRequest{
		Peer: p,
	})
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
