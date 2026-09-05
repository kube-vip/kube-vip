package bgp

import (
	"context"
	"testing"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	api "github.com/osrg/gobgp/v4/api"
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
