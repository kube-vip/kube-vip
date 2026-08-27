//go:build linux

package vip

import (
	"context"
	"net"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

func TestMonitorDefaultInterfaceReturnsErrorWhenLinkGoesDown(t *testing.T) {
	defaultIF := &net.Interface{Index: 7, Name: "test0"}
	routeCh := make(chan netlink.RouteUpdate)
	linkCh := make(chan netlink.LinkUpdate, 1)
	linkCh <- netlink.LinkUpdate{
		Link: &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Index: defaultIF.Index}},
	}

	err := monitorDefaultInterface(context.Background(), defaultIF, routeCh, linkCh)
	if err == nil {
		t.Fatal("expected an error when the default interface goes down")
	}
	if !strings.Contains(err.Error(), "default interface \"test0\" is down") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMonitorDefaultInterfaceReturnsErrorWhenTestLinkIsSetDown(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	originalNS, err := netns.Get()
	if err != nil {
		t.Skipf("getting current network namespace: %v", err)
	}
	defer originalNS.Close()

	testNS, err := netns.New()
	if err != nil {
		t.Skipf("creating isolated network namespace: %v", err)
	}
	defer testNS.Close()
	defer func() {
		if err := netns.Set(originalNS); err != nil {
			t.Errorf("restoring network namespace: %v", err)
		}
	}()

	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "kv-monitor0"}}
	if err := netlink.LinkAdd(link); err != nil {
		t.Fatalf("creating test interface: %v", err)
	}
	defer func() {
		if err := netlink.LinkDel(link); err != nil {
			t.Errorf("deleting test interface: %v", err)
		}
	}()
	if err := netlink.LinkSetUp(link); err != nil {
		t.Fatalf("bringing test interface up: %v", err)
	}

	defaultIF, err := net.InterfaceByName(link.Attrs().Name)
	if err != nil {
		t.Fatalf("getting test interface: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	routeCh, linkCh, err := subscribeDefaultInterface(ctx)
	if err != nil {
		t.Fatalf("subscribing to link updates: %v", err)
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- monitorDefaultInterface(ctx, defaultIF, routeCh, linkCh)
	}()

	if err := netlink.LinkSetDown(link); err != nil {
		t.Fatalf("bringing test interface down: %v", err)
	}

	select {
	case err := <-errCh:
		if err == nil || !strings.Contains(err.Error(), "default interface \"kv-monitor0\" is down") {
			t.Fatalf("monitor error = %v, want default-interface-down error", err)
		}
	case <-time.After(time.Second):
		t.Fatal("monitor did not report the interface going down")
	}
}
