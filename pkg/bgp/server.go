package bgp

import (
	"context"
	"fmt"
	"sync"
	"time"

	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	api "github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/apiutil"
	gobgp "github.com/osrg/gobgp/v4/pkg/server"
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
func (b *Server) Start(ctx context.Context, peerStateChangeCallback func(*apiutil.WatchEventMessage_PeerEvent)) (err error) {
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

	if err = b.s.WatchEvent(ctx, gobgp.WatchEventMessageCallbacks{
		OnPeerUpdate: func(p *apiutil.WatchEventMessage_PeerEvent, _ time.Time) {
			log.Info("[BGP]", "peer", fmt.Sprintf("%+v", p))
			if peerStateChangeCallback != nil {
				peerStateChangeCallback(p)
			}
		},
	}, gobgp.WatchPeer()); err != nil {
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return b.s.StopBgp(ctx, &api.StopBgpRequest{})
}
