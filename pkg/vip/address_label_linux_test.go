//go:build linux

package vip

import (
	"net"
	"os"
	"runtime"
	"testing"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

const kubeVIPProtocol = 248

// requireNetworkNamespaces makes the privileged CI job fail instead of silently
// skipping when it cannot enter a network namespace.
var requireNetworkNamespaces = os.Getenv("KUBE_VIP_REQUIRE_NETNS") != ""

func TestAddressProtocolRoundTripsThroughNetlink(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	originalNamespace, err := netns.Get()
	if err != nil {
		t.Fatalf("getting current network namespace: %v", err)
	}
	defer originalNamespace.Close()
	testNamespace, err := netns.New()
	if err != nil {
		if requireNetworkNamespaces {
			t.Fatalf("creating isolated network namespace: %v", err)
		}
		t.Skipf("creating isolated network namespace: %v", err)
	}
	defer testNamespace.Close()
	defer func() {
		if err := netns.Set(originalNamespace); err != nil {
			t.Errorf("restoring network namespace: %v", err)
		}
	}()

	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "kvproto0"}}
	if err := netlink.LinkAdd(link); err != nil {
		t.Fatalf("creating test interface: %v", err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		t.Fatalf("bringing test interface up: %v", err)
	}

	parsed, err := netlink.ParseAddr("192.0.2.10/32")
	if err != nil {
		t.Fatalf("parsing IPv4 address: %v", err)
	}
	markKubeVIPAddress(parsed, kubeVIPProtocol)
	if err := netlink.AddrReplace(link, parsed); err != nil {
		t.Fatalf("adding IPv4 address with protocol: %v", err)
	}
	addresses, err := netlink.AddrList(link, netlink.FAMILY_ALL)
	if err != nil {
		t.Fatalf("listing addresses: %v", err)
	}
	for _, configured := range addresses {
		if configured.IP.Equal(net.ParseIP("192.0.2.10")) {
			if !IsKubeVIPAddress(configured, kubeVIPProtocol) {
				t.Fatalf("configured address = %+v, want kube-vip protocol", configured)
			}
			return
		}
	}
	t.Fatal("IPv4 address with kube-vip protocol was not configured")
}

func TestKubeVIPAddressProtocolRoundTripsThroughIPv6Netlink(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	originalNamespace, err := netns.Get()
	if err != nil {
		t.Fatalf("getting current network namespace: %v", err)
	}
	defer originalNamespace.Close()
	testNamespace, err := netns.New()
	if err != nil {
		if requireNetworkNamespaces {
			t.Fatalf("creating isolated network namespace: %v", err)
		}
		t.Skipf("creating isolated network namespace: %v", err)
	}
	defer testNamespace.Close()
	defer func() {
		if err := netns.Set(originalNamespace); err != nil {
			t.Errorf("restoring network namespace: %v", err)
		}
	}()

	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "kvproto1"}}
	if err := netlink.LinkAdd(link); err != nil {
		t.Fatalf("creating test interface: %v", err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		t.Fatalf("bringing test interface up: %v", err)
	}

	address, err := netlink.ParseAddr("2001:db8::10/128")
	if err != nil {
		t.Fatalf("parsing IPv6 address: %v", err)
	}
	markKubeVIPAddress(address, kubeVIPProtocol)
	if err := netlink.AddrReplace(link, address); err != nil {
		t.Fatalf("adding IPv6 address with protocol: %v", err)
	}
	addresses, err := netlink.AddrList(link, netlink.FAMILY_V6)
	if err != nil {
		t.Fatalf("listing IPv6 addresses: %v", err)
	}
	for _, configured := range addresses {
		if configured.IP.Equal(net.ParseIP("2001:db8::10")) {
			if !IsKubeVIPAddress(configured, kubeVIPProtocol) {
				t.Fatalf("configured IPv6 address = %+v, want kube-vip protocol", configured)
			}
			return
		}
	}
	t.Fatalf("configured IPv6 addresses = %+v, want 2001:db8::10", addresses)
}

func TestCleanupKubeVIPAddressesRemovesOnlyUnretainedProtocolAddresses(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	originalNamespace, err := netns.Get()
	if err != nil {
		t.Fatalf("getting current network namespace: %v", err)
	}
	defer originalNamespace.Close()
	testNamespace, err := netns.New()
	if err != nil {
		if requireNetworkNamespaces {
			t.Fatalf("creating isolated network namespace: %v", err)
		}
		t.Skipf("creating isolated network namespace: %v", err)
	}
	defer testNamespace.Close()
	defer func() {
		if err := netns.Set(originalNamespace); err != nil {
			t.Errorf("restoring network namespace: %v", err)
		}
	}()

	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "kvproto2"}}
	if err := netlink.LinkAdd(link); err != nil {
		t.Fatalf("creating test interface: %v", err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		t.Fatalf("bringing test interface up: %v", err)
	}
	for _, input := range []struct {
		cidr     string
		protocol int
	}{
		{cidr: "192.0.2.10/32", protocol: kubeVIPProtocol},
		{cidr: "192.0.2.11/32", protocol: kubeVIPProtocol},
		{cidr: "192.0.2.12/32", protocol: 0},
		{cidr: "192.0.2.13/32", protocol: kubeVIPProtocol + 1},
		{cidr: "2001:db8::10/128", protocol: kubeVIPProtocol},
		{cidr: "2001:db8::11/128", protocol: kubeVIPProtocol},
	} {
		address, err := netlink.ParseAddr(input.cidr)
		if err != nil {
			t.Fatalf("parsing address: %v", err)
		}
		markKubeVIPAddress(address, input.protocol)
		if err := netlink.AddrReplace(link, address); err != nil {
			t.Fatalf("adding address %s: %v", input.cidr, err)
		}
	}

	retained, err := netlink.ParseAddr("192.0.2.10/32")
	if err != nil {
		t.Fatalf("parsing retained address: %v", err)
	}
	retained.LinkIndex = link.Attrs().Index
	retainedIPv6, err := netlink.ParseAddr("2001:db8::10/128")
	if err != nil {
		t.Fatalf("parsing retained IPv6 address: %v", err)
	}
	retainedIPv6.LinkIndex = link.Attrs().Index
	removed, err := CleanupKubeVIPAddresses(kubeVIPProtocol, map[string]struct{}{
		addressKey(*retained):     {},
		addressKey(*retainedIPv6): {},
	})
	if err != nil {
		t.Fatalf("cleaning kube-vip addresses: %v", err)
	}
	if removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}
	addresses, err := netlink.AddrList(link, netlink.FAMILY_ALL)
	if err != nil {
		t.Fatalf("listing remaining addresses: %v", err)
	}
	wantAddresses := map[string]bool{
		"192.0.2.10":   false,
		"192.0.2.12":   false,
		"192.0.2.13":   false,
		"2001:db8::10": false,
	}
	for _, address := range addresses {
		if _, wanted := wantAddresses[address.IP.String()]; wanted {
			wantAddresses[address.IP.String()] = true
		}
	}
	for address, found := range wantAddresses {
		if !found {
			t.Fatalf("remaining addresses = %+v, missing %s", addresses, address)
		}
	}
}
