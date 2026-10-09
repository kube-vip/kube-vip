//go:build linux

package vip

import (
	"net"
	"runtime"
	"testing"

	"github.com/kube-vip/kube-vip/pkg/networkinterface"
	"github.com/kube-vip/kube-vip/pkg/route"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

func TestPrepareRouteScope(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cidr      string
		routeType int
		scope     netlink.Scope
	}{
		{"IPv4 local", "192.0.2.10/32", unix.RTN_LOCAL, netlink.SCOPE_HOST},
		{"IPv6 local", "2001:db8::10/128", unix.RTN_LOCAL, netlink.SCOPE_HOST},
		{"IPv4 unicast", "192.0.2.10/32", unix.RTN_UNICAST, netlink.SCOPE_UNIVERSE},
		{"IPv6 unicast", "2001:db8::10/128", unix.RTN_UNICAST, netlink.SCOPE_UNIVERSE},
	} {
		t.Run(tc.name, func(t *testing.T) {
			address, err := netlink.ParseAddr(tc.cidr)
			if err != nil {
				t.Fatal(err)
			}
			configurator := &network{
				address:          address,
				link:             networkinterface.NewManager().Get(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Index: 1}}),
				routeTable:       unix.RT_TABLE_LOCAL,
				routingTableType: tc.routeType,
				routingProtocol:  249,
			}
			if route := configurator.PrepareRoute(); route.Scope != tc.scope {
				t.Fatalf("route scope = %v, want %v", route.Scope, tc.scope)
			}
		})
	}
}

func TestLocalRouteRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ip     string
		other  string
		family int
	}{
		{"IPv4", "192.0.2.10", "192.0.2.11", netlink.FAMILY_V4},
		{"IPv6", "2001:db8::10", "2001:db8::11", netlink.FAMILY_V6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lo := localRouteTestNamespace(t)
			networks, err := NewConfig(tc.ip, "lo", false, "32,128", false, "", false, false,
				unix.RT_TABLE_LOCAL, unix.RTN_LOCAL, 249, "", "", "", false, 0, false,
				networkinterface.NewManager(), false, false)
			if err != nil {
				t.Fatal(err)
			}
			configured := networks[0]
			sentinel := *configured.PrepareRoute()
			sentinel.Dst = &net.IPNet{IP: net.ParseIP(tc.other), Mask: sentinel.Dst.Mask}
			sentinel.Protocol = 248
			sentinel.Scope = netlink.SCOPE_HOST
			if err := netlink.RouteAdd(&sentinel); err != nil {
				t.Fatalf("adding unrelated route: %v", err)
			}
			if added, err := configured.AddRoute(false); err != nil || !added {
				t.Fatalf("adding local route: added=%v, err=%v", added, err)
			}
			filter := netlink.RT_FILTER_TABLE | netlink.RT_FILTER_DST | netlink.RT_FILTER_PROTOCOL | netlink.RT_FILTER_TYPE | netlink.RT_FILTER_OIF
			routes, err := netlink.RouteListFiltered(tc.family, configured.PrepareRoute(), filter)
			if err != nil || len(routes) != 1 {
				t.Fatalf("reading local route: routes=%v, err=%v", routes, err)
			}
			if tc.family == netlink.FAMILY_V4 && routes[0].Scope != netlink.SCOPE_HOST {
				t.Fatalf("kernel route scope=%v, want host", routes[0].Scope)
			}
			addresses, err := netlink.AddrList(lo, tc.family)
			if err != nil {
				t.Fatal(err)
			}
			for _, address := range addresses {
				if address.IP.Equal(net.ParseIP(tc.ip)) {
					t.Fatalf("VIP assigned to loopback: %v", address)
				}
			}
			if err := configured.DeleteRoute(); err != nil {
				t.Fatalf("deleting local route: %v", err)
			}
			routes, err = netlink.RouteListFiltered(tc.family, configured.PrepareRoute(), filter)
			if err != nil || len(routes) != 0 {
				t.Fatalf("local route remains: routes=%v, err=%v", routes, err)
			}
			routes, err = netlink.RouteListFiltered(tc.family, &sentinel, filter)
			if err != nil || len(routes) != 1 {
				t.Fatalf("unrelated route changed: routes=%v, err=%v", routes, err)
			}
		})
	}
}

func TestLocalRouteAdoptsLegacyIPv6LinkScope(t *testing.T) {
	lo := localRouteTestNamespace(t)
	networks, err := NewConfig("2001:db8::10", "lo", false, "32,128", false, "", false, false,
		unix.RT_TABLE_LOCAL, unix.RTN_LOCAL, 249, "", "", "", false, 0, false,
		networkinterface.NewManager(), false, false)
	if err != nil {
		t.Fatal(err)
	}
	configured := networks[0]
	if configured.PrepareRoute().Scope != netlink.SCOPE_HOST {
		t.Fatal("patched controller must request host scope")
	}
	for _, input := range []struct {
		ip       string
		protocol netlink.RouteProtocol
	}{
		{"2001:db8::10", 249},
		{"2001:db8::11", 249},
		{"2001:db8::12", 248},
	} {
		legacy := *configured.PrepareRoute()
		legacy.Dst = &net.IPNet{IP: net.ParseIP(input.ip), Mask: net.CIDRMask(128, 128)}
		legacy.Scope = netlink.SCOPE_LINK
		legacy.Protocol = input.protocol
		if err := netlink.RouteAdd(&legacy); err != nil {
			t.Fatalf("creating legacy link-scope route for %s: %v", input.ip, err)
		}
	}
	readRoutes := func() []netlink.Route {
		t.Helper()
		routes, err := netlink.RouteListFiltered(netlink.FAMILY_V6,
			&netlink.Route{Table: unix.RT_TABLE_LOCAL}, netlink.RT_FILTER_TABLE)
		if err != nil {
			t.Fatal(err)
		}
		return routes
	}
	assertRoutes := func(want []netlink.Route) {
		t.Helper()
		got := readRoutes()
		if len(got) != len(want) {
			t.Fatalf("local table = %v, want %v", got, want)
		}
		for _, expected := range want {
			found := false
			for _, actual := range got {
				if actual.Equal(expected) {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("route changed or removed: %v; local table = %v", expected, got)
			}
		}
	}
	before := readRoutes()
	manager := route.NewManager()
	if err := manager.Add("test/service", configured, false, true); err != nil {
		t.Fatalf("adopting legacy IPv6 local route: %v", err)
	}
	if !manager.Check(configured.RouteHash()) {
		t.Fatal("legacy route was not adopted")
	}
	assertRoutes(before)
	if err := manager.Delete("test/service", configured); err != nil {
		t.Fatalf("removing adopted IPv6 local route: %v", err)
	}
	if manager.Check(configured.RouteHash()) {
		t.Fatal("removed route remains tracked")
	}
	var remaining []netlink.Route
	for _, r := range before {
		if r.Dst == nil || !r.Dst.IP.Equal(net.ParseIP(configured.IP())) {
			remaining = append(remaining, r)
		}
	}
	if len(remaining) != len(before)-1 {
		t.Fatal("expected exactly one legacy VIP route")
	}
	assertRoutes(remaining)
	addresses, err := netlink.AddrList(lo, netlink.FAMILY_V6)
	if err != nil {
		t.Fatal(err)
	}
	for _, address := range addresses {
		if address.IP.Equal(net.ParseIP(configured.IP())) {
			t.Fatalf("VIP assigned to loopback: %v", address)
		}
	}
}

func localRouteTestNamespace(t *testing.T) netlink.Link {
	t.Helper()
	runtime.LockOSThread()
	original, err := netns.Get()
	if err != nil {
		runtime.UnlockOSThread()
		t.Fatal(err)
	}
	testNS, err := netns.New()
	if err != nil {
		original.Close()
		runtime.UnlockOSThread()
		if requireNetworkNamespaces {
			t.Fatalf("creating network namespace: %v", err)
		}
		t.Skipf("creating network namespace: %v", err)
	}
	t.Cleanup(func() {
		if err := netns.Set(original); err != nil {
			t.Errorf("restoring network namespace: %v", err)
		}
		testNS.Close()
		original.Close()
		runtime.UnlockOSThread()
	})
	lo, err := netlink.LinkByName("lo")
	if err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkSetUp(lo); err != nil {
		t.Fatal(err)
	}
	return lo
}
