package bgp

import (
	"context"
	"errors"
	"fmt"
	log "log/slog"
	"net"

	"github.com/osrg/gobgp/v4/pkg/apiutil"
)

// AddHost will update peers of a host
func (b *Server) AddHost(ctx context.Context, addr string, object string) error {
	if !b.begin() {
		return context.Canceled
	}
	defer b.active.Done()
	for {
		b.mtx.Lock()
		if b.closing.Load() {
			b.mtx.Unlock()
			return context.Canceled
		}
		if done := b.pending[addr]; done != nil {
			b.mtx.Unlock()
			select {
			case <-done:
			case <-ctx.Done():
				return ctx.Err()
			}
			continue
		}
		objects, exists := b.tracker[addr]
		if !exists || len(objects) == 0 {
			b.tracker[addr] = make(map[string]bool)
			objects = b.tracker[addr]
			ip, _, err := net.ParseCIDR(addr)
			if err != nil {
				b.mtx.Unlock()
				return err
			}
			p := b.getPath(ip)
			if p == nil {
				b.mtx.Unlock()
				return fmt.Errorf("failed to get path for %v", ip)
			}
			if _, err := b.s.AddPath(apiutil.AddPathRequest{Paths: []*apiutil.Path{p}}); err != nil {
				b.mtx.Unlock()
				return err
			}
			log.Debug("[BGP] added host", "addr", addr, "cnt", len(objects)+1, "object", object)
		}
		objects[object] = true
		b.mtx.Unlock()
		return nil
	}
}

func (b *Server) DelHost(ctx context.Context, addr string, object string) error {
	if !b.begin() {
		return context.Canceled
	}
	defer b.active.Done()
	for {
		b.mtx.Lock()
		if b.closing.Load() {
			b.mtx.Unlock()
			return context.Canceled
		}
		if done := b.pending[addr]; done != nil {
			b.mtx.Unlock()
			select {
			case <-done:
			case <-ctx.Done():
				return ctx.Err()
			}
			continue
		}
		objects, exists := b.tracker[addr]
		if !exists {
			b.mtx.Unlock()
			return nil
		}
		ip, _, err := net.ParseCIDR(addr)
		if err != nil {
			b.mtx.Unlock()
			return err
		}
		delete(objects, object)
		if len(objects) != 0 {
			b.mtx.Unlock()
			return nil
		}
		p := b.getPath(ip)
		if p == nil {
			delete(b.tracker, addr)
			b.mtx.Unlock()
			return nil
		}
		done := make(chan struct{})
		if b.pending == nil {
			b.pending = make(map[string]chan struct{})
		}
		b.pending[addr] = done
		b.mtx.Unlock()
		complete := func(operationErr error) {
			b.mtx.Lock()
			if operationErr != nil {
				b.tracker[addr] = make(map[string]bool)
			} else {
				delete(b.tracker, addr)
			}
			delete(b.pending, addr)
			close(done)
			b.mtx.Unlock()
		}
		if err := b.deletePath(ctx, apiutil.DeletePathRequest{Paths: []*apiutil.Path{p}}, complete); err != nil {
			if errors.Is(err, errWithdrawalNotQueued) {
				b.mtx.Lock()
				b.tracker[addr][object] = true
				delete(b.pending, addr)
				close(done)
				b.mtx.Unlock()
			}
			return err
		}
		return nil
	}
}

func (b *Server) deletePath(ctx context.Context, req apiutil.DeletePathRequest, callbacks ...func(error)) error {
	var complete func(error)
	if len(callbacks) > 0 {
		complete = callbacks[0]
	}
	b.startWithdrawalWorker()
	result := make(chan error, 1)
	request := withdrawalRequest{ctx: ctx, req: req, result: result, complete: complete}
	select {
	case b.withdrawQueue <- request:
	default:
		// An available queue slot transfers ownership of the non-cancellable
		// GoBGP call even if the caller has just been cancelled. If the
		// bounded queue is full, cancellation remains the escape hatch.
		select {
		case b.withdrawQueue <- request:
		case <-ctx.Done():
			return fmt.Errorf("%w: %w", errWithdrawalNotQueued, ctx.Err())
		}
	}
	// Once queued, the withdrawal owns the serialized route operation. Wait for
	// completion even if the caller context is canceled; returning early would
	// release b.mtx while DeletePath is still pending and allow a new owner to
	// advertise before the old deletion completes.
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
