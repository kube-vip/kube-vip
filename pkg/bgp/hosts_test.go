package bgp

import (
	"context"
	"errors"
	"net/netip"
	"testing"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	api "github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/apiutil"
	bgppacket "github.com/osrg/gobgp/v4/pkg/packet/bgp"
)

func newHostTestBGPServer(t *testing.T) *Server {
	t.Helper()
	return newStartedTestBGPServer(t, kubevip.BGPConfig{
		AS:       65000,
		RouterID: "192.0.2.1",
		Peers:    []kubevip.BGPPeer{{Address: "192.0.2.10", AS: 65001}},
	})
}

// Inspect the embedded server's GLOBAL RIB, not just kube-vip's bookkeeping.
// ListPath is synchronous, so no peer sessions, sleeps or polling are needed.
func assertHostTestPath(t *testing.T, server *Server, prefix, nextHop string, wantCount int) {
	t.Helper()
	family := bgppacket.RF_IPv4_UC
	if netip.MustParsePrefix(prefix).Addr().Is6() {
		family = bgppacket.RF_IPv6_UC
	}
	var got []*apiutil.Path
	err := server.s.ListPath(apiutil.ListPathRequest{
		TableType: api.TableType_TABLE_TYPE_GLOBAL,
		Family:    family,
	}, func(nlri bgppacket.NLRI, paths []*apiutil.Path) {
		if nlri.String() == prefix {
			got = append(got, paths...)
		}
	})
	if err != nil {
		t.Fatalf("ListPath(%s): %v", prefix, err)
	}
	if len(got) != wantCount {
		t.Fatalf("RIB paths for %s = %d, want %d", prefix, len(got), wantCount)
	}
	for _, path := range got {
		if path.Withdrawal || path.Family != family || path.Nlri.String() != prefix {
			t.Fatalf("unexpected RIB path for %s: %+v", prefix, path)
		}
		var hops []netip.Addr
		for _, attr := range path.Attrs {
			switch attr := attr.(type) {
			case *bgppacket.PathAttributeNextHop:
				if family != bgppacket.RF_IPv4_UC {
					t.Fatal("IPv6 path uses NEXT_HOP instead of MP_REACH_NLRI")
				}
				hops = append(hops, attr.Value)
			case *bgppacket.PathAttributeMpReachNLRI:
				if family != bgppacket.RF_IPv6_UC {
					t.Fatal("IPv4 path uses MP_REACH_NLRI instead of NEXT_HOP")
				}
				hops = append(hops, attr.Nexthop)
			}
		}
		if len(hops) != 1 || hops[0] != netip.MustParseAddr(nextHop) {
			t.Fatalf("RIB next hops for %s = %v, want [%s]", prefix, hops, nextHop)
		}
	}
}

func assertHostTestOwners(t *testing.T, server *Server, prefix, nextHop string, owners ...string) {
	t.Helper()
	server.mtx.Lock()
	defer server.mtx.Unlock()
	objects, tracked := server.tracker[prefix]
	hop, hopTracked := server.nextHops[prefix]
	if len(owners) == 0 {
		if tracked || hopTracked {
			t.Fatalf("deleted prefix %s still tracked: owners=%v, nextHop=%q (present=%t)", prefix, objects, hop, hopTracked)
		}
		return
	}
	if !tracked || len(objects) != len(owners) || !hopTracked || hop != nextHop {
		t.Fatalf("state for %s: owners=%v, nextHop=%q (present=%t); want owners=%v, nextHop=%q", prefix, objects, hop, hopTracked, owners, nextHop)
	}
	for _, owner := range owners {
		if !objects[owner] {
			t.Errorf("prefix %s is missing owner %q", prefix, owner)
		}
	}
}

func TestUpdateRouteNextHopLifecycle(t *testing.T) {
	for _, tt := range []struct {
		name, prefix, initial, replacement, canonical string
	}{
		{"IPv4", "198.51.100.10/32", "192.0.2.20", "192.0.2.21", "192.0.2.21"},
		{"IPv6", "2001:db8:1::10/128", "2001:db8:2::20", "2001:0db8:0002:0000:0000:0000:0000:0021", "2001:db8:2::21"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := newHostTestBGPServer(t)
			ctx := context.Background()
			route := Route{Prefix: tt.prefix, NextHop: tt.initial, Object: "ns/service"}
			for _, hop := range []string{tt.initial, tt.initial, tt.replacement, tt.replacement} {
				route.NextHop = hop
				if err := server.UpdateRoute(ctx, route); err != nil {
					t.Fatalf("UpdateRoute(%+v): %v", route, err)
				}
				canonical := netip.MustParseAddr(hop).String()
				assertHostTestPath(t, server, tt.prefix, canonical, 1)
				assertHostTestOwners(t, server, tt.prefix, canonical, route.Object)
			}
			assertHostTestOwners(t, server, tt.prefix, tt.canonical, route.Object)
			if err := server.DelHost(ctx, tt.prefix, route.Object); err != nil {
				t.Fatalf("DelHost(): %v", err)
			}
			assertHostTestPath(t, server, tt.prefix, "", 0)
			assertHostTestOwners(t, server, tt.prefix, "")
			if err := server.DelHost(ctx, tt.prefix, route.Object); err != nil {
				t.Fatalf("repeated DelHost(): %v", err)
			}
		})
	}
}

func TestUpdateRouteSharedOwners(t *testing.T) {
	for _, tt := range []struct{ name, prefix, initial, replacement string }{
		{"IPv4", "198.51.100.10/32", "192.0.2.20", "192.0.2.21"},
		{"IPv6", "2001:db8:1::10/128", "2001:db8:2::20", "2001:db8:2::21"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := newHostTestBGPServer(t)
			ctx := context.Background()
			for _, owner := range []string{"first", "first", "second", "second"} {
				if err := server.UpdateRoute(ctx, Route{Prefix: tt.prefix, NextHop: tt.initial, Object: owner}); err != nil {
					t.Fatalf("UpdateRoute(owner=%s): %v", owner, err)
				}
			}
			assertHostTestOwners(t, server, tt.prefix, tt.initial, "first", "second")
			assertHostTestPath(t, server, tt.prefix, tt.initial, 1)
			for _, owner := range []string{"first", "second", "intruder"} {
				if err := server.UpdateRoute(ctx, Route{Prefix: tt.prefix, NextHop: tt.replacement, Object: owner}); err == nil {
					t.Fatalf("conflicting UpdateRoute(owner=%s) succeeded", owner)
				}
				assertHostTestOwners(t, server, tt.prefix, tt.initial, "first", "second")
				assertHostTestPath(t, server, tt.prefix, tt.initial, 1)
			}
			if err := server.AddHost(ctx, tt.prefix, "legacy"); err == nil {
				t.Fatal("AddHost() replaced a shared explicit next hop")
			}
			for _, owner := range []string{"unknown", "first", "first"} {
				if err := server.DelHost(ctx, tt.prefix, owner); err != nil {
					t.Fatalf("DelHost(owner=%s): %v", owner, err)
				}
				assertHostTestPath(t, server, tt.prefix, tt.initial, 1)
			}
			assertHostTestOwners(t, server, tt.prefix, tt.initial, "second")
			if err := server.UpdateRoute(ctx, Route{Prefix: tt.prefix, NextHop: tt.replacement, Object: "second"}); err != nil {
				t.Fatalf("sole remaining owner could not replace route: %v", err)
			}
			assertHostTestOwners(t, server, tt.prefix, tt.replacement, "second")
			assertHostTestPath(t, server, tt.prefix, tt.replacement, 1)
			if err := server.DelHost(ctx, tt.prefix, "second"); err != nil {
				t.Fatalf("last DelHost(): %v", err)
			}
			assertHostTestOwners(t, server, tt.prefix, "")
			assertHostTestPath(t, server, tt.prefix, "", 0)
		})
	}
}

func TestAddHostDelHostLifecycle(t *testing.T) {
	for _, tt := range []struct{ name, prefix, defaultHop, explicitHop string }{
		{"IPv4", "198.51.100.10/32", "0.0.0.0", "192.0.2.20"},
		{"IPv6", "2001:db8:1::10/128", "::", "2001:db8:2::20"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := newHostTestBGPServer(t)
			ctx := context.Background()
			if err := server.AddHost(ctx, tt.prefix, "first"); err != nil {
				t.Fatalf("AddHost(): %v", err)
			}
			assertHostTestOwners(t, server, tt.prefix, "", "first")
			assertHostTestPath(t, server, tt.prefix, tt.defaultHop, 1)
			if err := server.UpdateRoute(ctx, Route{Prefix: tt.prefix, NextHop: tt.explicitHop, Object: "first"}); err != nil {
				t.Fatalf("legacy to explicit transition: %v", err)
			}
			assertHostTestOwners(t, server, tt.prefix, tt.explicitHop, "first")
			assertHostTestPath(t, server, tt.prefix, tt.explicitHop, 1)
			for _, owner := range []string{"first", "first", "second", "second"} {
				if err := server.AddHost(ctx, tt.prefix, owner); err != nil {
					t.Fatalf("AddHost(owner=%s): %v", owner, err)
				}
				assertHostTestPath(t, server, tt.prefix, tt.defaultHop, 1)
			}
			assertHostTestOwners(t, server, tt.prefix, "", "first", "second")
			if err := server.UpdateRoute(ctx, Route{Prefix: tt.prefix, NextHop: tt.explicitHop, Object: "first"}); err == nil {
				t.Fatal("UpdateRoute() replaced a shared legacy next hop")
			}
			if err := server.DelHost(ctx, tt.prefix, "unknown"); err != nil {
				t.Fatalf("unknown owner DelHost(): %v", err)
			}
			assertHostTestOwners(t, server, tt.prefix, "", "first", "second")
			for _, owner := range []string{"first", "first"} {
				if err := server.DelHost(ctx, tt.prefix, owner); err != nil {
					t.Fatalf("DelHost(owner=%s): %v", owner, err)
				}
				assertHostTestOwners(t, server, tt.prefix, "", "second")
				assertHostTestPath(t, server, tt.prefix, tt.defaultHop, 1)
			}
			if err := server.DelHost(ctx, tt.prefix, "second"); err != nil {
				t.Fatalf("last DelHost(): %v", err)
			}
			assertHostTestOwners(t, server, tt.prefix, "")
			assertHostTestPath(t, server, tt.prefix, "", 0)
			if err := server.DelHost(ctx, "not-a-prefix", "unknown"); err != nil {
				t.Fatalf("untracked DelHost() should be a no-op: %v", err)
			}
		})
	}
}

func TestUpdateRouteInvalidInputsDoNotPoisonTracker(t *testing.T) {
	const prefix = "198.51.100.10/32"
	for _, tt := range []struct{ name, prefix, nextHop string }{
		{"empty prefix", "", "192.0.2.20"},
		{"malformed prefix", "not-a-prefix", "192.0.2.20"},
		{"missing mask", "198.51.100.10", "192.0.2.20"},
		{"invalid mask", "198.51.100.10/33", "192.0.2.20"},
		{"non-host prefix", "198.51.100.0/24", "192.0.2.20"},
		{"empty next hop", prefix, ""},
		{"malformed next hop", prefix, "not-an-ip"},
		{"CIDR next hop", prefix, "192.0.2.20/32"},
		{"IPv4 unspecified", prefix, "0.0.0.0"},
		{"IPv4 loopback", prefix, "127.0.0.1"},
		{"IPv4 link local", prefix, "169.254.1.1"},
		{"IPv4 multicast", prefix, "224.0.0.1"},
		{"IPv4 broadcast", prefix, "255.255.255.255"},
		{"IPv6 next hop for IPv4", prefix, "2001:db8::20"},
		{"IPv4 next hop for IPv6", "2001:db8:1::10/128", "192.0.2.20"},
		{"IPv6 unspecified", "2001:db8:1::10/128", "::"},
		{"IPv6 loopback", "2001:db8:1::10/128", "::1"},
		{"IPv6 link local", "2001:db8:1::10/128", "fe80::1"},
		{"IPv6 multicast", "2001:db8:1::10/128", "ff02::1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := newHostTestBGPServer(t)
			ctx := context.Background()
			invalid := Route{Prefix: tt.prefix, NextHop: tt.nextHop, Object: "invalid-owner"}
			if err := server.UpdateRoute(ctx, invalid); err == nil {
				t.Fatalf("UpdateRoute(%+v) succeeded", invalid)
			}
			if len(server.tracker) != 0 || len(server.nextHops) != 0 {
				t.Fatalf("invalid route poisoned state: tracker=%v, nextHops=%v", server.tracker, server.nextHops)
			}
			assertHostTestPath(t, server, prefix, "", 0)
			assertHostTestPath(t, server, "2001:db8:1::10/128", "", 0)
			valid := Route{Prefix: prefix, NextHop: "192.0.2.20", Object: "valid-owner"}
			if parsed, err := netip.ParsePrefix(tt.prefix); err == nil && parsed.Addr().Is6() {
				valid.Prefix, valid.NextHop = tt.prefix, "2001:db8:2::20"
			}
			if err := server.UpdateRoute(ctx, valid); err != nil {
				t.Fatalf("valid retry after invalid route: %v", err)
			}
			if err := server.UpdateRoute(ctx, invalid); err == nil {
				t.Fatal("invalid update of an existing route succeeded")
			}
			assertHostTestOwners(t, server, valid.Prefix, valid.NextHop, valid.Object)
			assertHostTestPath(t, server, valid.Prefix, valid.NextHop, 1)
		})
	}
}

func TestHostRejectedAddsDoNotPoisonTracker(t *testing.T) {
	for _, operation := range []string{"AddHost", "UpdateRoute"} {
		t.Run(operation, func(t *testing.T) {
			server := newHostTestBGPServer(t)
			ctx := context.Background()
			const prefix = "198.51.100.10/32"
			add := func(ctx context.Context, prefix string) error {
				if operation == "AddHost" {
					return server.AddHost(ctx, prefix, "owner")
				}
				return server.UpdateRoute(ctx, Route{Prefix: prefix, NextHop: "192.0.2.20", Object: "owner"})
			}
			if err := add(ctx, "bad-prefix"); err == nil {
				t.Fatal("malformed prefix was accepted")
			}
			cancelled, cancel := context.WithCancel(ctx)
			cancel()
			if err := add(cancelled, prefix); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled add error = %v, want context.Canceled", err)
			}
			if len(server.tracker) != 0 || len(server.nextHops) != 0 {
				t.Fatalf("rejected add poisoned state: tracker=%v, nextHops=%v", server.tracker, server.nextHops)
			}
			assertHostTestPath(t, server, prefix, "", 0)

			// StopBgp also terminates Serve in this GoBGP version. AddPath
			// then fails on the closed management channel, without mocks.
			if err := server.s.StopBgp(ctx, &api.StopBgpRequest{}); err != nil {
				t.Fatalf("StopBgp(): %v", err)
			}
			if err := add(ctx, prefix); err == nil {
				t.Fatal("add succeeded while BGP was stopped")
			}
			if len(server.tracker) != 0 || len(server.nextHops) != 0 {
				t.Fatalf("failed AddPath poisoned state: tracker=%v, nextHops=%v", server.tracker, server.nextHops)
			}
			// A stopped GoBGP server cannot be restarted. Keep kube-vip's
			// tracker and retry against a fresh, live embedded server.
			server.s = newHostTestBGPServer(t).s
			if err := add(ctx, prefix); err != nil {
				t.Fatalf("retry after failed AddPath: %v", err)
			}
			nextHop, trackedHop := "192.0.2.20", "192.0.2.20"
			if operation == "AddHost" {
				nextHop, trackedHop = "0.0.0.0", ""
			}
			assertHostTestOwners(t, server, prefix, trackedHop, "owner")
			assertHostTestPath(t, server, prefix, nextHop, 1)
		})
	}
}
