package bgp

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	api "github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/apiutil"
	gobgp "github.com/osrg/gobgp/v4/pkg/server"
)

func TestAddHostDoesNotTrackInvalidCIDR(t *testing.T) {
	b, _ := newEmbeddedHostTestServer(t)

	if err := b.AddHost(context.Background(), "not-a-cidr", "default/service"); err == nil {
		t.Fatal("AddHost unexpectedly accepted an invalid CIDR")
	}
	if len(b.tracker) != 0 {
		t.Fatalf("tracker = %#v, want empty after failed add", b.tracker)
	}
}

func TestAddHostRetriesAfterGoBGPFailure(t *testing.T) {
	b, first := newEmbeddedHostTestServer(t)
	const addr = "10.0.0.34/32"
	const object = "default/service"

	// Stop the embedded daemon to make the first AddPath fail. This is the
	// failure mode seen during a BGP/API restart, not a malformed service VIP.
	if err := first.StopBgp(context.Background(), &api.StopBgpRequest{}); err != nil {
		t.Fatalf("stopping first embedded BGP server: %v", err)
	}
	if err := b.AddHost(context.Background(), addr, object); err == nil {
		t.Fatal("AddHost unexpectedly succeeded while GoBGP was stopped")
	}
	if len(b.tracker) != 0 {
		t.Fatalf("tracker = %#v, want empty after failed AddPath", b.tracker)
	}

	// The kube-vip Server object survives the daemon recovery. A retry must add
	// the path to the replacement daemon instead of trusting the failed tracker
	// entry.
	b.s = startEmbeddedRawBGP(t)
	if err := b.AddHost(context.Background(), addr, object); err != nil {
		t.Fatalf("AddHost retry: %v", err)
	}

	routes, err := b.ListAdvertisedRoutes(context.Background(), false)
	if err != nil {
		t.Fatalf("ListAdvertisedRoutes: %v", err)
	}
	if len(routes) != 1 || routes[0].Prefix != addr {
		t.Fatalf("recovered BGP server routes = %#v, want %q", routes, addr)
	}
}

func TestAddHostReAddsAfterFailedDeleteOnBGPRecovery(t *testing.T) {
	b, first := newEmbeddedHostTestServer(t)
	const addr = "10.0.0.35/32"
	const object = "default/service"

	if err := b.AddHost(context.Background(), addr, object); err != nil {
		t.Fatalf("initial AddHost: %v", err)
	}
	if err := first.StopBgp(context.Background(), &api.StopBgpRequest{}); err != nil {
		t.Fatalf("stopping first embedded BGP server: %v", err)
	}
	if err := b.DelHost(context.Background(), addr, object); err == nil {
		t.Fatal("DelHost unexpectedly succeeded while GoBGP was stopped")
	}

	// A service can be deleted and recreated while the BGP API is unavailable.
	// The replacement must not trust the empty tracker entry left by the failed
	// delete; it still needs to install the route in the recovered daemon.
	b.s = startEmbeddedRawBGP(t)
	if err := b.AddHost(context.Background(), addr, object); err != nil {
		t.Fatalf("recreated service AddHost: %v", err)
	}

	routes, err := b.ListAdvertisedRoutes(context.Background(), false)
	if err != nil {
		t.Fatalf("ListAdvertisedRoutes: %v", err)
	}
	if len(routes) != 1 || routes[0].Prefix != addr {
		t.Fatalf("recreated service routes = %#v, want %q", routes, addr)
	}
}

func TestAddHostDoesNotAddPathForNewReference(t *testing.T) {
	b, raw := newEmbeddedHostTestServer(t)
	const addr = "10.0.0.36/32"

	if err := b.AddHost(context.Background(), addr, "default/first"); err != nil {
		t.Fatalf("initial AddHost: %v", err)
	}
	if err := raw.StopBgp(context.Background(), &api.StopBgpRequest{}); err != nil {
		t.Fatalf("stopping embedded BGP server: %v", err)
	}

	if err := b.AddHost(context.Background(), addr, "default/second"); err != nil {
		t.Fatalf("adding a second reference called AddPath: %v", err)
	}
	if got := len(b.tracker[addr]); got != 2 {
		t.Fatalf("tracker reference count = %d, want 2", got)
	}
	if err := b.DelHost(context.Background(), addr, "default/first"); err != nil {
		t.Fatalf("deleting first reference called DeletePath: %v", err)
	}
	if got := len(b.tracker[addr]); got != 1 {
		t.Fatalf("tracker reference count = %d, want 1", got)
	}
	if err := b.AddHost(context.Background(), addr, "default/second"); err == nil {
		t.Fatal("reconciling an existing reference did not call AddPath")
	}
}

func TestAddHostOutboundUpdates(t *testing.T) {
	tests := []struct {
		name                 string
		addPath              bool
		wantSameReferenceMsg bool
	}{
		{name: "ordinary peer"},
		{name: "Add-Path peer", addPath: true, wantSameReferenceMsg: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const addr = "10.0.0.37/32"
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			b, updates := newPeeredHostTestServer(t, ctx, tt.addPath, addr)
			if err := b.AddHost(ctx, addr, "default/first"); err != nil {
				t.Fatalf("initial AddHost: %v", err)
			}
			waitForHostUpdate(t, ctx, updates, "initial advertisement")

			if err := b.AddHost(ctx, addr, "default/second"); err != nil {
				t.Fatalf("new-reference AddHost: %v", err)
			}
			assertNoHostUpdate(t, updates, "new reference")

			if err := b.AddHost(ctx, addr, "default/first"); err != nil {
				t.Fatalf("same-reference AddHost: %v", err)
			}
			if tt.wantSameReferenceMsg {
				waitForHostUpdate(t, ctx, updates, "Add-Path same-reference advertisement")
			} else {
				assertNoHostUpdate(t, updates, "ordinary same-reference reconciliation")
			}

		})
	}
}

func newPeeredHostTestServer(t *testing.T, ctx context.Context, addPath bool, addr string) (*Server, <-chan struct{}) {
	t.Helper()
	receiver, port := startHostReceiver(t, ctx, addPath)
	updates := make(chan struct{}, 4)
	if err := receiver.WatchEvent(ctx, gobgp.WatchEventMessageCallbacks{
		OnPathUpdate: func(paths []*apiutil.Path, _ time.Time) {
			for _, path := range paths {
				if !path.Withdrawal && path.Nlri.String() == addr {
					updates <- struct{}{}
				}
			}
		},
	}, gobgp.WatchUpdate(false, "", "")); err != nil {
		t.Fatalf("watching receiver updates: %v", err)
	}

	sender := startPeeredHostSender(t, ctx, receiver, port, addPath)
	return &Server{
		s:       sender,
		c:       &kubevip.BGPConfig{AS: 65000, RouterID: "192.0.2.1"},
		tracker: make(map[string]map[string]bool),
	}, updates
}

func startHostReceiver(t *testing.T, ctx context.Context, addPath bool) (*gobgp.BgpServer, uint32) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving receiver port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).AddrPort().Port()
	if err := listener.Close(); err != nil {
		t.Fatalf("releasing receiver port: %v", err)
	}

	receiver := gobgp.NewBgpServer()
	go receiver.Serve()
	if err := receiver.StartBgp(ctx, &api.StartBgpRequest{Global: &api.Global{
		Asn:             65000,
		RouterId:        "192.0.2.2",
		ListenPort:      int32(port),
		ListenAddresses: []string{"127.0.0.1"},
	}}); err != nil {
		t.Fatalf("starting receiver: %v", err)
	}
	if err := receiver.AddPeer(ctx, &api.AddPeerRequest{Peer: hostTestPeer("127.0.0.2", "127.0.0.1", 0, true, addPath, false)}); err != nil {
		t.Fatalf("adding receiver peer: %v", err)
	}
	t.Cleanup(receiver.Stop)
	return receiver, uint32(port)
}

func startPeeredHostSender(t *testing.T, ctx context.Context, receiver *gobgp.BgpServer, port uint32, addPath bool) *gobgp.BgpServer {
	t.Helper()
	sender := gobgp.NewBgpServer()
	go sender.Serve()
	if err := sender.StartBgp(ctx, &api.StartBgpRequest{Global: &api.Global{
		Asn:        65000,
		RouterId:   "192.0.2.1",
		ListenPort: -1,
	}}); err != nil {
		t.Fatalf("starting sender: %v", err)
	}
	if err := sender.AddPeer(ctx, &api.AddPeerRequest{Peer: hostTestPeer("127.0.0.1", "127.0.0.2", port, false, false, addPath)}); err != nil {
		t.Fatalf("adding sender peer: %v", err)
	}
	waitForEstablishedHostPeers(t, ctx, sender, receiver)
	t.Cleanup(sender.Stop)
	return sender
}

func hostTestPeer(address, localAddress string, port uint32, passive, receiveAddPath, sendAddPath bool) *api.Peer {
	peer := &api.Peer{
		Conf: &api.PeerConf{NeighborAddress: address, PeerAsn: 65000},
		Transport: &api.Transport{
			LocalAddress: localAddress,
			RemotePort:   port,
			PassiveMode:  passive,
		},
	}
	if receiveAddPath || sendAddPath {
		peer.AfiSafis = []*api.AfiSafi{{
			Config:   &api.AfiSafiConfig{Family: &api.Family{Afi: api.Family_AFI_IP, Safi: api.Family_SAFI_UNICAST}, Enabled: true},
			AddPaths: &api.AddPaths{Config: &api.AddPathsConfig{Receive: receiveAddPath, SendMax: map[bool]uint32{true: 2}[sendAddPath]}},
		}}
	}
	return peer
}

func waitForEstablishedHostPeers(t *testing.T, ctx context.Context, servers ...*gobgp.BgpServer) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		allEstablished := true
		for _, server := range servers {
			established := false
			if err := server.ListPeer(ctx, &api.ListPeerRequest{}, func(peer *api.Peer) {
				established = peer.State.SessionState == api.PeerState_SESSION_STATE_ESTABLISHED
			}); err != nil {
				t.Fatalf("listing peer state: %v", err)
			}
			allEstablished = allEstablished && established
		}
		if allEstablished {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for BGP peers: %v", ctx.Err())
		case <-ticker.C:
		}
	}
}

func waitForHostUpdate(t *testing.T, ctx context.Context, updates <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-updates:
	case <-ctx.Done():
		t.Fatalf("waiting for %s: %v", description, ctx.Err())
	}
}

func assertNoHostUpdate(t *testing.T, updates <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-updates:
		t.Fatalf("received unexpected update for %s", description)
	case <-time.After(200 * time.Millisecond):
	}
}

func newEmbeddedHostTestServer(t *testing.T) (*Server, *gobgp.BgpServer) {
	t.Helper()
	raw := startEmbeddedRawBGP(t)
	return &Server{
		s: raw,
		c: &kubevip.BGPConfig{
			AS:       65000,
			RouterID: "192.0.2.1",
		},
		tracker: make(map[string]map[string]bool),
	}, raw
}
