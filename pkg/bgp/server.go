package bgp

import (
	"context"
	"fmt"
	"time"

	"github.com/kube-vip/kube-vip/api/v1alpha1"
	api "github.com/osrg/gobgp/v3/api"
	gobgp "github.com/osrg/gobgp/v3/pkg/server"
	log "github.com/sirupsen/logrus"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// NewBGPServer takes a configuration and returns a running BGP server instance
func NewBGPServer(c *v1alpha1.BGPConfig, peerStateChangeCallback func(*api.WatchEventResponse_PeerEvent), ctrlClient client.Client) (b *Server, err error) {
	if c.AS == 0 {
		return nil, fmt.Errorf("you need to provide AS")
	}

	if c.SourceIP != "" && c.SourceIF != "" {
		return nil, fmt.Errorf("sourceIP and SourceIF are mutually exclusive")
	}

	if len(c.Peers) == 0 {
		return nil, fmt.Errorf("you need to provide at least one peer")
	}

	peers := v1alpha1.BGPPeerList{}

	if err := ctrlClient.List(context.Background(), &peers, &client.ListOptions{}); err != nil {
		return nil, fmt.Errorf("failed to list BGP peers from CRDs: %w", err)
	}

	c.UpdatePeers(&peers)

	b = &Server{
		s: gobgp.NewBgpServer(),
		c: c,
	}
	go b.s.Serve()

	if err = b.s.StartBgp(context.Background(), &api.StartBgpRequest{
		Global: &api.Global{
			Asn:        c.AS,
			RouterId:   c.RouterID,
			ListenPort: -1,
		},
	}); err != nil {
		return
	}

	if err = b.s.WatchEvent(context.Background(), &api.WatchEventRequest{Peer: &api.WatchEventRequest_Peer{}}, func(r *api.WatchEventResponse) {
		if p := r.GetPeer(); p != nil && p.Type == api.WatchEventResponse_PeerEvent_STATE {
			log.Infof("[BGP] %s", p.String())
			if peerStateChangeCallback != nil {
				peerStateChangeCallback(p)
			}
		}
	}); err != nil {
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
