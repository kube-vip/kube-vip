package bgp

import (
	"context"
	"fmt"
	"sync"
	"time"

	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	api "github.com/osrg/gobgp/v3/api"
	gobgp "github.com/osrg/gobgp/v3/pkg/server"
)

// Server manages a server object
type Server struct {
	s       *gobgp.BgpServer
	c       *kubevip.BGPConfig
	mtx     sync.Mutex
	tracker map[string]map[string]bool
}

// NewBGPServer takes a configuration and returns a running BGP server instance
func NewBGPServer(c kubevip.BGPConfig) (b *Server, err error) {
	if c.AS == 0 {
		return nil, fmt.Errorf("you need to provide AS")
	}

	if c.SourceIP != "" && c.SourceIF != "" {
		return nil, fmt.Errorf("sourceIP and SourceIF are mutually exclusive")
	}

	if len(c.Peers) == 0 {
		return nil, fmt.Errorf("you need to provide at least one peer")
	}

	b = &Server{
		s:       gobgp.NewBgpServer(),
		c:       &c,
		tracker: make(map[string]map[string]bool),
	}
	return
}

// Start starts the BGP server
func (b *Server) Start(ctx context.Context, peerStateChangeCallback func(*api.WatchEventResponse_PeerEvent)) (err error) {
	go b.s.Serve()

	if err = b.s.StartBgp(ctx, &api.StartBgpRequest{
		Global: &api.Global{
			Asn:        b.c.AS,
			RouterId:   b.c.RouterID,
			ListenPort: -1,
		},
	}); err != nil {
		return
	}

	if err = b.s.WatchEvent(ctx, &api.WatchEventRequest{Peer: &api.WatchEventRequest_Peer{}}, func(r *api.WatchEventResponse) {
		if p := r.GetPeer(); p != nil && p.Type == api.WatchEventResponse_PeerEvent_STATE {
			log.Info("[BGP]", "peer", p.String())
			if peerStateChangeCallback != nil {
				peerStateChangeCallback(p)
			}
		}
	}); err != nil {
		return
	}

	for _, p := range b.c.Peers {
		if err = b.AddPeer(ctx, p); err != nil {
			return
		}
	}

	if b.c.Zebra.Enabled {
		if err = b.s.EnableZebra(ctx, &api.EnableZebraRequest{
			Url:          b.c.Zebra.URL,
			Version:      b.c.Zebra.Version,
			SoftwareName: b.c.Zebra.SoftwareName,
		}); err != nil {
			log.Error(err.Error())
			return
		}
	}

	return
}

// Close will stop a running BGP Server
func (b *Server) Close() error {
	// create new BGP stop context (independent)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return b.s.StopBgp(ctx, &api.StopBgpRequest{})
}

// ListAdvertisedRoutes retrieves all active routes inside GoBGP's local RIB.
// It queries the GLOBAL table type to find routes that kube-vip has requested GoBGP to advertise.
func (b *Server) ListAdvertisedRoutes(ctx context.Context, isIPv6 bool) ([]*api.Destination, error) {
	family := &api.Family{
		Afi:  api.Family_AFI_IP,
		Safi: api.Family_SAFI_UNICAST,
	}
	if isIPv6 {
		family.Afi = api.Family_AFI_IP6
	}

	var destinations []*api.Destination

	req := &api.ListPathRequest{
		TableType: api.TableType_GLOBAL,
		Family:    family,
	}

	// GoBGP's embedded server API uses a callback function to stream results
	// locally without requiring a gRPC client stream setup.
	err := b.s.ListPath(ctx, req, func(destination *api.Destination) {
		if destination != nil {
			destinations = append(destinations, destination)
		}
	})

	if err != nil {
		return nil, fmt.Errorf("failed to extract local RIB: %w", err)
	}

	return destinations, nil
}
