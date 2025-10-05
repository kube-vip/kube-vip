package bgp

import (
	"context"
	"fmt"
	"time"

	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	api "github.com/osrg/gobgp/v3/api"
	gobgp "github.com/osrg/gobgp/v3/pkg/server"
	"github.com/prometheus/client_golang/prometheus"
)

// Server manages a server object
type Server struct {
	s *gobgp.BgpServer
	c *kubevip.BGPConfig

	// This is a prometheus gauge indicating the state of the sessions.
	// 1 means "ESTABLISHED", 0 means "NOT ESTABLISHED"
	BGPSessionInfoGauge *prometheus.GaugeVec
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
		s: gobgp.NewBgpServer(),
		c: &c,

		BGPSessionInfoGauge: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "kube_vip",
			Subsystem: "manager",
			Name:      "bgp_session_info",
			Help:      "Display state of session by setting metric for label value with current state to 1",
		}, []string{"state", "peer"}),
	}
	return
}

// Start starts the BGP server
func (b *Server) Start(peerStateChangeCallback func(*api.WatchEventResponse_PeerEvent)) (err error) {
	go b.s.Serve()

	if err = b.s.StartBgp(context.Background(), &api.StartBgpRequest{
		Global: &api.Global{
			Asn:        b.c.AS,
			RouterId:   b.c.RouterID,
			ListenPort: -1,
		},
	}); err != nil {
		return
	}

	if err = b.s.WatchEvent(context.Background(), &api.WatchEventRequest{Peer: &api.WatchEventRequest_Peer{}}, func(r *api.WatchEventResponse) {
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
		if err = b.AddPeer(p); err != nil {
			return
		}
	}

	if b.c.Zebra.Enabled {
		if err = b.s.EnableZebra(context.Background(), &api.EnableZebraRequest{
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
	ctx, cf := context.WithTimeout(context.Background(), 5*time.Second)
	defer cf()
	return b.s.StopBgp(ctx, &api.StopBgpRequest{})
}
