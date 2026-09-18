//go:build linux

package vip

import (
	"context"
	"net"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

func TestMonitorDefaultInterfaceDetectsDefaultRouteDeletionPerFamily(t *testing.T) {
	for _, test := range []struct {
		name string
		cidr string
	}{
		{name: "IPv4", cidr: "0.0.0.0/0"},
		{name: "IPv6", cidr: "::/0"},
	} {
		t.Run(test.name, func(t *testing.T) {
			defaultIF := &net.Interface{Index: 7, Name: "test0"}
			_, defaultRoute, err := net.ParseCIDR(test.cidr)
			if err != nil {
				t.Fatalf("ParseCIDR() error = %v", err)
			}
			routeCh := make(chan netlink.RouteUpdate, 1)
			routeCh <- netlink.RouteUpdate{
				Type:  syscall.RTM_DELROUTE,
				Route: netlink.Route{Dst: defaultRoute, LinkIndex: defaultIF.Index},
			}
			linkCh := make(chan netlink.LinkUpdate)

			err = monitorDefaultInterfaceForTest(t, context.Background(), defaultIF, routeCh, linkCh)
			if err == nil || !strings.Contains(err.Error(), "default route deleted") {
				t.Fatalf("monitor error = %v, want a default route deletion error", err)
			}
		})
	}
}

func TestMonitorDefaultInterfaceReturnsErrorWhenLinkGoesDown(t *testing.T) {
	defaultIF := &net.Interface{Index: 7, Name: "test0"}
	routeCh := make(chan netlink.RouteUpdate)
	linkCh := make(chan netlink.LinkUpdate, 1)
	linkCh <- netlink.LinkUpdate{
		Link: &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Index: defaultIF.Index}},
	}

	err := monitorDefaultInterfaceForTest(t, context.Background(), defaultIF, routeCh, linkCh)
	if err == nil {
		t.Fatal("expected an error when the default interface goes down")
	}
	if !strings.Contains(err.Error(), "default interface \"test0\" is down") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMonitorDefaultInterfaceHandlesClosedSubscriptions(t *testing.T) {
	for _, test := range []struct {
		name       string
		closeRoute bool
		cancel     bool
		wantErr    string
	}{
		{name: "closed route subscription", closeRoute: true, wantErr: "route subscription closed"},
		{name: "closed link subscription", wantErr: "link subscription closed"},
		{name: "context cancellation with closed route subscription", closeRoute: true, cancel: true},
		{name: "context cancellation with closed link subscription", cancel: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			routeCh := make(chan netlink.RouteUpdate)
			linkCh := make(chan netlink.LinkUpdate)
			if test.closeRoute {
				close(routeCh)
			} else {
				close(linkCh)
			}
			if test.cancel {
				cancel()
			}

			err := monitorDefaultInterfaceForTest(t, ctx, &net.Interface{}, routeCh, linkCh)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("monitor error = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("monitor error = %v, want %q", err, test.wantErr)
			}
		})
	}
}

func monitorDefaultInterfaceForTest(t *testing.T, ctx context.Context, defaultIF *net.Interface,
	routeCh <-chan netlink.RouteUpdate, linkCh <-chan netlink.LinkUpdate) error {
	t.Helper()
	errCh := make(chan error, 1)
	go func() {
		errCh <- monitorDefaultInterface(ctx, defaultIF, routeCh, linkCh)
	}()
	select {
	case err := <-errCh:
		return err
	case <-time.After(time.Second):
		t.Fatal("default interface monitor did not return")
		return nil
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

func TestMonitorDefaultInterfaceRetriesClosedSubscription(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	secondSubscribed := make(chan struct{})
	attempts := 0
	subscribe := func(ctx context.Context) (chan netlink.RouteUpdate, chan netlink.LinkUpdate, error) {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		attempts++
		routeCh := make(chan netlink.RouteUpdate)
		linkCh := make(chan netlink.LinkUpdate)
		if attempts == 1 {
			close(routeCh)
			close(linkCh)
			return routeCh, linkCh, nil
		}
		close(secondSubscribed)
		go func() {
			<-ctx.Done()
			close(routeCh)
			close(linkCh)
		}()
		return routeCh, linkCh, nil
	}
	lookup := func() (*net.Interface, error) {
		return &net.Interface{Index: 2, Name: "refreshed"}, nil
	}
	done := make(chan error, 1)
	go func() {
		done <- monitorDefaultInterfaceWithRetry(ctx, &net.Interface{Index: 1, Name: "original"}, subscribe, lookup, time.Millisecond)
	}()
	select {
	case <-secondSubscribed:
	case <-time.After(time.Second):
		t.Fatal("monitor did not resubscribe after channel closure")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("monitor returned an error after cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("resubscribed monitor did not stop after cancellation")
	}
	if attempts != 2 {
		t.Fatalf("subscription attempts = %d, want 2", attempts)
	}
}

func TestMonitorDefaultInterfaceIgnoresNilLink(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
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
		t.Fatal("monitor did not stop after nil link update")
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
		t.Fatalf("getting current network namespace: %v", err)
	}
	defer originalNS.Close()

	testNS, err := netns.New()
	if err != nil {
		if requireNetworkNamespaces {
			t.Fatalf("creating isolated network namespace: %v", err)
		}
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
