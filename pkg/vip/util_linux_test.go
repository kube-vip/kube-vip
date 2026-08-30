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

func TestMonitorDefaultInterfaceReturnsErrorWhenSubscriptionCloses(t *testing.T) {
	tests := []struct {
		name    string
		close   func(chan netlink.RouteUpdate, chan netlink.LinkUpdate)
		wantErr string
	}{
		{
			name:    "route",
			close:   func(routeCh chan netlink.RouteUpdate, _ chan netlink.LinkUpdate) { close(routeCh) },
			wantErr: "route subscription closed",
		},
		{
			name:    "link",
			close:   func(_ chan netlink.RouteUpdate, linkCh chan netlink.LinkUpdate) { close(linkCh) },
			wantErr: "link subscription closed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			routeCh := make(chan netlink.RouteUpdate)
			linkCh := make(chan netlink.LinkUpdate)
			tt.close(routeCh, linkCh)

			errCh := make(chan error, 1)
			go func() {
				errCh <- monitorDefaultInterface(context.Background(), &net.Interface{}, routeCh, linkCh)
			}()

			select {
			case err := <-errCh:
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("monitor error = %v, want %q", err, tt.wantErr)
				}
			case <-time.After(time.Second):
				t.Fatal("monitor did not report the closed subscription")
			}
		})
	}
}

func TestMonitorDefaultInterfaceHandlesClosedSubscriptionsAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	routeCh := make(chan netlink.RouteUpdate)
	linkCh := make(chan netlink.LinkUpdate)
	errCh := make(chan error, 1)
	go func() {
		errCh <- monitorDefaultInterface(ctx, &net.Interface{}, routeCh, linkCh)
	}()

	cancel()
	close(routeCh)
	close(linkCh)

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("monitor error = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("monitor did not stop after cancellation")
	}
}

func TestMonitorDefaultInterfaceIgnoresNilLink(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	routeCh := make(chan netlink.RouteUpdate)
	linkCh := make(chan netlink.LinkUpdate)
	errCh := make(chan error, 1)
	go func() {
		errCh <- monitorDefaultInterface(ctx, &net.Interface{}, routeCh, linkCh)
	}()

	linkCh <- netlink.LinkUpdate{}
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("monitor error = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("monitor did not stop after cancellation")
	}
}

func TestDrainDefaultInterfaceSubscriptionsIsBounded(t *testing.T) {
	done := make(chan struct{})
	go func() {
		drainDefaultInterfaceSubscriptions(make(chan netlink.RouteUpdate), make(chan netlink.LinkUpdate))
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("subscription drain did not stop")
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
