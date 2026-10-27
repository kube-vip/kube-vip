package bgp

import (
	"context"
	"fmt"
	log "log/slog"
	"net"
	"net/netip"

	"github.com/osrg/gobgp/v4/pkg/apiutil"
)

// AddHost will update peers of a host
func (b *Server) AddHost(ctx context.Context, addr string, object string) error {
	return b.updateHost(ctx, Route{Prefix: addr, Object: object})
}

// Route describes a Service advertisement with an explicit node next hop.
type Route struct {
	Prefix  string
	NextHop string
	Object  string
}

// UpdateRoute adds or replaces a route without withdrawing its prefix first.
// GoBGP implicitly replaces the locally originated path for the same NLRI.
func (b *Server) UpdateRoute(ctx context.Context, route Route) error {
	prefix, err := netip.ParsePrefix(route.Prefix)
	if err != nil {
		return err
	}
	if prefix.Bits() != prefix.Addr().BitLen() || prefix.Addr().Is4In6() {
		return fmt.Errorf("BGP Service route must be a host prefix: %s", route.Prefix)
	}
	nextHop, err := netip.ParseAddr(route.NextHop)
	if err != nil || !nextHop.IsGlobalUnicast() || nextHop.Is4() != prefix.Addr().Is4() {
		return fmt.Errorf("invalid next hop %q for %s", route.NextHop, route.Prefix)
	}
	route.NextHop = nextHop.String()
	return b.updateHost(ctx, route)
}

func (b *Server) updateHost(ctx context.Context, route Route) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !b.begin() {
		return context.Canceled
	}
	defer b.active.Done()
	addr, object := route.Prefix, route.Object
	if err := b.lockRoute(ctx, addr); err != nil {
		return err
	}
	defer b.mtx.Unlock()

	objects, exists := b.tracker[addr]
	if exists && b.nextHops[addr] != route.NextHop {
		for owner := range objects {
			if owner != object {
				return fmt.Errorf("conflicting next hop for shared BGP prefix %s (owner %s)", addr, owner)
			}
		}
	}

	if !exists || b.nextHops[addr] != route.NextHop {
		ip, _, err := net.ParseCIDR(addr)
		if err != nil {
			return err
		}

		p := b.getPathWithNextHop(ip, route.NextHop)
		if p == nil {
			return fmt.Errorf("failed to get path for %v", ip)
		}

		if _, err := b.s.AddPath(apiutil.AddPathRequest{
			Paths: []*apiutil.Path{p},
		}); err != nil {
			return err
		}
		if b.nextHops == nil {
			b.nextHops = make(map[string]string)
		}
		b.nextHops[addr] = route.NextHop
		log.Debug("[BGP] added host", "addr", addr, "cnt", len(objects)+1, "object", object)
	}

	if !exists {
		objects = make(map[string]bool)
		b.tracker[addr] = objects
	}
	objects[object] = true

	return nil
}

// DelHost will inform peers to remove a host. The withdrawal itself runs on
// the serialized withdrawal worker, so a cancelled ctx bounds how long the
// caller waits even though GoBGP's DeletePath cannot be cancelled.
func (b *Server) DelHost(ctx context.Context, addr string, object string) error {
	if !b.begin() {
		return context.Canceled
	}
	defer b.active.Done()
	if err := b.lockRoute(ctx, addr); err != nil {
		return err
	}

	objects, exists := b.tracker[addr]
	if !exists {
		b.mtx.Unlock()
		log.Debug("[BGP] deleting host - nothing to delete", "addr", addr, "object", object)
		return nil
	}

	ip, _, err := net.ParseCIDR(addr)
	if err != nil {
		b.mtx.Unlock()
		return err
	}

	if !objects[object] {
		b.mtx.Unlock()
		return nil
	}
	if len(objects) > 1 {
		delete(objects, object)
		b.mtx.Unlock()
		log.Debug("[BGP] deleting from tracker only", "addr", addr, "object", object)
		return nil
	}

	p := b.getPathWithNextHop(ip, b.nextHops[addr])
	if p == nil {
		b.mtx.Unlock()
		return nil
	}

	// Mark the withdrawal as pending so other route operations for addr wait
	// for its outcome instead of racing the in-flight DeletePath.
	done := make(chan struct{})
	if b.pending == nil {
		b.pending = make(map[string]chan struct{})
	}
	b.pending[addr] = done
	b.mtx.Unlock()

	complete := func(operationErr error) {
		b.mtx.Lock()
		defer b.mtx.Unlock()
		// On failure keep the reference: the path may still be installed and a
		// later DelHost must retry the withdrawal.
		if operationErr == nil {
			delete(b.tracker, addr)
			delete(b.nextHops, addr)
			log.Debug("[BGP] deleted host", "addr", addr, "object", object)
		}
		delete(b.pending, addr)
		close(done)
	}
	return b.deletePath(ctx, apiutil.DeletePathRequest{Paths: []*apiutil.Path{p}}, complete)
}

// lockRoute acquires b.mtx once no withdrawal of addr is in flight. On success
// the caller owns b.mtx and must unlock it.
func (b *Server) lockRoute(ctx context.Context, addr string) error {
	for {
		b.mtx.Lock()
		if b.closing.Load() {
			b.mtx.Unlock()
			return context.Canceled
		}
		done := b.pending[addr]
		if done == nil {
			return nil
		}
		b.mtx.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// deletePath hands req to the withdrawal worker. complete, if set, is called
// exactly once with the outcome, also when the request could not be queued.
func (b *Server) deletePath(ctx context.Context, req apiutil.DeletePathRequest, complete func(error)) error {
	b.startWithdrawalWorker()
	result := make(chan error, 1)
	request := withdrawalRequest{req: req, result: result, complete: complete}
	select {
	case b.withdrawQueue <- request:
	default:
		// An available queue slot transfers ownership of the non-cancellable
		// GoBGP call even if the caller has just been cancelled. If the
		// bounded queue is full, cancellation remains the escape hatch.
		select {
		case b.withdrawQueue <- request:
		case <-ctx.Done():
			err := fmt.Errorf("withdrawal not queued: %w", ctx.Err())
			if complete != nil {
				complete(err)
			}
			return err
		}
	}
	// Once queued, the worker owns the operation and its completion; the
	// pending entry keeps other operations on the route waiting, so the caller
	// may stop waiting when its context is cancelled.
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
