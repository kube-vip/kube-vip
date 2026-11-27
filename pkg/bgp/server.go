package bgp

import (
	"context"
	"fmt"
	"net/netip"
	"sync"
	"sync/atomic"
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
	s        *gobgp.BgpServer
	c        *kubevip.BGPConfig
	mtx      sync.Mutex
	tracker  map[string]map[string]bool
	nextHops map[string]string

	// bmpServers are the BGP Monitoring Protocol stations the server reports to
	bmpServers []netip.AddrPort
	bmpPolicy  api.AddBmpRequest_MonitoringPolicy

	// pending holds a channel per route whose withdrawal is in flight; it is
	// closed once the withdrawal completes.
	pending map[string]chan struct{}

	stateMu           sync.Mutex
	active            sync.WaitGroup
	withdrawOnce      sync.Once
	withdrawCloseOnce sync.Once
	withdrawQueue     chan withdrawalRequest
	withdrawStop      chan struct{}
	withdrawDone      chan struct{}
	deletePathFunc    func(apiutil.DeletePathRequest) error // test hook, defaults to GoBGP
	closing           atomic.Bool
	closeOnce         sync.Once
	closeDone         chan struct{}
	closeErr          error
}

func (b *Server) begin() bool {
	b.stateMu.Lock()
	defer b.stateMu.Unlock()
	if b.closing.Load() {
		return false
	}
	b.active.Add(1)
	return true
}

const withdrawalQueueSize = 32

type withdrawalRequest struct {
	req      apiutil.DeletePathRequest
	result   chan error
	complete func(error)
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

	bmpServers, bmpPolicy, err := c.BMP.Parse()
	if err != nil {
		return nil, err
	}

	bgpLogger := log.Default()
	lvl := &log.LevelVar{}
	lvl.Set(logLevel)

	b = &Server{
		s:          gobgp.NewBgpServer(gobgp.LoggerOption(bgpLogger, lvl)),
		c:          &c,
		tracker:    make(map[string]map[string]bool),
		bmpServers: bmpServers,
		bmpPolicy:  bmpPolicy,
		pending:    make(map[string]chan struct{}),
	}
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
	deletePath := b.deletePathFunc
	if deletePath == nil {
		deletePath = b.s.DeletePath
	}
	for {
		select {
		case <-b.withdrawStop:
			b.cancelQueuedWithdrawals()
			return
		default:
		}
		select {
		case req := <-b.withdrawQueue:
			err := deletePath(req.req)
			if req.complete != nil {
				req.complete(err)
			}
			req.result <- err
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
			if req.complete != nil {
				req.complete(context.Canceled)
			}
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

	// Connect the BMP stations before the peers, so they are told about every
	// session from the moment it is established.
	for _, server := range b.bmpServers {
		if err = b.s.AddBmp(ctx, &api.AddBmpRequest{
			Address:           server.Addr().String(),
			Port:              uint32(server.Port()),
			Policy:            b.bmpPolicy,
			StatisticsTimeout: int32(b.c.BMP.StatisticsTimeout),
		}); err != nil {
			return fmt.Errorf("adding BMP server %s: %w", server, err)
		}
		log.Info("[BGP] reporting to BMP server", "server", server.String(), "policy", b.bmpPolicy.String())
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
// Close rejects new route operations, waits for in-flight ones and pending
// withdrawals to finish, and then stops GoBGP. It returns after at most five
// seconds; the shutdown continues in the background if that deadline passes.
func (b *Server) Close() error {
	b.stateMu.Lock()
	b.closing.Store(true)
	b.stateMu.Unlock()
	b.closeOnce.Do(func() {
		b.closeDone = make(chan struct{})
		go func() {
			b.active.Wait()
			if b.withdrawDone != nil {
				b.withdrawCloseOnce.Do(func() { close(b.withdrawStop) })
				<-b.withdrawDone
			}
			b.closeErr = b.s.StopBgp(context.Background(), &api.StopBgpRequest{})
			close(b.closeDone)
		}()
	})
	select {
	case <-b.closeDone:
		return b.closeErr
	case <-time.After(5 * time.Second):
		return context.DeadlineExceeded
	}
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
