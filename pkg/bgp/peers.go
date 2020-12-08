package bgp

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/golang/protobuf/ptypes"
	"github.com/golang/protobuf/ptypes/any"
	api "github.com/osrg/gobgp/api"
)

//AddPeer will add peers to the BGP configuration
func (b *Server) AddPeer(peer Peer) (err error) {
	port := 179

	if t := strings.SplitN(peer.Address, ":", 2); len(t) == 2 {
		peer.Address = t[0]

		if port, err = strconv.Atoi(t[1]); err != nil {
			return fmt.Errorf("Unable to parse port '%s' as int: %s", t[1], err)
		}
	}

	p := &api.Peer{
		Conf: &api.PeerConf{
			NeighborAddress: peer.Address,
			PeerAs:          peer.AS,
			AuthPassword:    peer.Password,
		},

		Timers: &api.Timers{
			Config: &api.TimersConfig{
				ConnectRetry: 10,
			},
		},

		Transport: &api.Transport{
			MtuDiscovery:  true,
			RemoteAddress: peer.Address,
			RemotePort:    uint32(port),
		},
	}

	// if b.c.SourceIP != "" {
	// 	p.Transport.LocalAddress = b.c.SourceIP
	// }

	// if b.c.SourceIF != "" {
	// 	p.Transport.BindInterface = b.c.SourceIF
	// }

	return b.s.AddPeer(context.Background(), &api.AddPeerRequest{
		Peer: p,
	})
}

func (b *Server) getPath(ip net.IP) *api.Path {
	var pfxLen uint32 = 32
	if ip.To4() == nil {
		if !b.c.IPv6 {
			return nil
		}

		pfxLen = 128
	}

	nlri, _ := ptypes.MarshalAny(&api.IPAddressPrefix{
		Prefix:    ip.String(),
		PrefixLen: pfxLen,
	})

	a1, _ := ptypes.MarshalAny(&api.OriginAttribute{
		Origin: 0,
	})

	var nh string
	if b.c.NextHop != "" {
		nh = b.c.NextHop
	} else if b.c.SourceIP != "" {
		nh = b.c.SourceIP
	} else {
		nh = b.c.RouterID
	}

	a2, _ := ptypes.MarshalAny(&api.NextHopAttribute{
		NextHop: nh,
	})

	return &api.Path{
		Family: &api.Family{
			Afi:  api.Family_AFI_IP,
			Safi: api.Family_SAFI_UNICAST,
		},
		Nlri:   nlri,
		Pattrs: []*any.Any{a1, a2},
	}
}
