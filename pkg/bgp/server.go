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
	bgp "github.com/osrg/gobgp/v4/pkg/packet/bgp"
	gobgp "github.com/osrg/gobgp/v4/pkg/server"
)

type BGPManager interface {
	AddHost(ctx context.Context, addr string, object string) error
	DelHost(ctx context.Context, addr string, object string) error
}

// Server manages a server object
type Server struct {
	s                 *gobgp.BgpServer
	c                 *kubevip.BGPConfig
	mtx               sync.Mutex
	tracker           map[string]map[string]bool
	withdrawOnce      sync.Once
	withdrawCloseOnce sync.Once
	withdrawQueue     chan withdrawalRequest
	withdrawStop      chan struct{}
	withdrawDone      chan struct{}
	deletePathFunc    func(apiutil.DeletePathRequest) error
}

const withdrawalQueueSize = 32

type withdrawalRequest struct {
	ctx    context.Context
	req    apiutil.DeletePathRequest
	result chan error
}

// NewBGPServer takes a configuration and returns a running BGP server instance
func NewBGPServer(c kubevip.BGPConfig, logLevel log.Level) (b *Server, err error) {
	if c.AS == 0 {
		return nil, fmt.Errorf("you need to provide AS")
	}

	if c.SourceIP != "" && c.SourceIF != "" {
		return nil, fmt.Errorf("sourceIP and SourceIF are mutually exclusive")
	}

	if len(c.Peers) == 0 {
		return nil, fmt.Errorf("you need to provide at least one peer")
	}
	bgpLogger := log.Default()
	lvl := &log.LevelVar{}
	lvl.Set(logLevel)

	b = &Server{
		s:       gobgp.NewBgpServer(gobgp.LoggerOption(bgpLogger, lvl)),
		c:       &c,
		tracker: make(map[string]map[string]bool),
	}
	b.deletePathFunc = b.s.DeletePath
	return
}

func (b *Server) startWithdrawalWorker() {
	b.withdrawOnce.Do(func() {
		b.withdrawQueue = make(chan withdrawalRequest, withdrawalQueueSize)
		b.withdrawStop = make(chan struct{})
		b.withdrawDone = make(chan struct{})
		go b.withdrawalWorker()
	})
}

// withdrawalWorker serializes withdrawals so cancellation cannot create an
// unbounded goroutine-per-call. GoBGP's embedded DeletePath has no context
// parameter; a queued operation therefore remains non-cancellable once it
// starts, but the caller can stop waiting and the bounded worker owns its
// eventual completion.
func (b *Server) withdrawalWorker() {
	defer close(b.withdrawDone)
	for {
		select {
		case <-b.withdrawStop:
			b.cancelQueuedWithdrawals()
			return
		default:
		}
		select {
		case req := <-b.withdrawQueue:
			req.result <- b.deletePathFunc(req.req)
		case <-b.withdrawStop:
			b.cancelQueuedWithdrawals()
			return
		}
	}
}

func (b *Server) cancelQueuedWithdrawals() {
	for {
		select {
		case req := <-b.withdrawQueue:
			req.result <- context.Canceled
		default:
			return
		}
	}
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
	if b.withdrawDone != nil {
		b.withdrawCloseOnce.Do(func() { close(b.withdrawStop) })
		<-b.withdrawDone
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return b.s.StopBgp(ctx, &api.StopBgpRequest{})
}

// ListAdvertisedRoutes retrieves all active routes inside GoBGP's local RIB.
// It queries the GLOBAL table type to find routes that kube-vip has requested GoBGP to advertise.
func (b *Server) ListAdvertisedRoutes(ctx context.Context, isIPv6 bool) ([]*api.Destination, error) {
	afi := bgp.AFI_IP

	if isIPv6 {
		afi = bgp.AFI_IP6
	}

	family := bgp.NewFamily(uint16(afi), bgp.SAFI_UNICAST)

	var destinations []*api.Destination

	req := apiutil.ListPathRequest{
		TableType: api.TableType_TABLE_TYPE_GLOBAL,
		Family:    family,
	}

	// GoBGP's embedded server API uses a callback function to stream results
	// locally without requiring a gRPC client stream setup.
	err := b.s.ListPath(req, func(prefix bgp.NLRI, paths []*apiutil.Path) {
		var newPaths []*api.Path
		for _, p := range paths {
			np, err := apiutil.NewPath(p.Family, p.Nlri, p.Withdrawal, p.Attrs, time.Unix(p.Age, 0))
			if err != nil {
				log.Error("failed to create BGP path details", "err", err)
				continue
			}
			newPaths = append(newPaths, np)
		}
		d := &api.Destination{
			Prefix: prefix.String(),
			Paths:  newPaths,
		}
		destinations = append(destinations, d)
	})

	if err != nil {
		return nil, fmt.Errorf("failed to extract local RIB: %w", err)
	}

	return destinations, nil
}
