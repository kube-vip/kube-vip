package bgp

import (
	"context"
	"fmt"
	"log"
	"time"

	api "github.com/osrg/gobgp/api"
	gobgp "github.com/osrg/gobgp/pkg/server"
)

// NewBGPServer takes a configuration and returns a running BGP server instance
func NewBGPServer(c *Config) (b *Server, err error) {
	if c.AS == 0 {
		return nil, fmt.Errorf("You need to provide AS")
	}

	// if c.SourceIP != "" && c.SourceIF != "" {
	// 	return nil, fmt.Errorf("SourceIP and SourceIF are mutually exclusive")
	// }

	if len(c.Peers) == 0 {
		return nil, fmt.Errorf("You need to provide at least one peer")
	}

	b = &Server{
		s: gobgp.NewBgpServer(),
		c: c,
	}
	go b.s.Serve()

	if err = b.s.StartBgp(context.Background(), &api.StartBgpRequest{
		Global: &api.Global{
			As:         c.AS,
			RouterId:   c.RouterID,
			ListenPort: -1,
		},
	}); err != nil {
		return
	}

	if err = b.s.MonitorPeer(context.Background(), &api.MonitorPeerRequest{}, func(p *api.Peer) { log.Println(p) }); err != nil {
		return
	}

	for _, p := range c.Peers {
		if err = b.AddPeer(p); err != nil {
			return
		}
	}

	return
}

// Close will stop a running BGP Server
func (b *Server) Close() error {
	ctx, cf := context.WithTimeout(context.Background(), 5*time.Second)
	defer cf()
	return b.s.StopBgp(ctx, &api.StopBgpRequest{})
}
