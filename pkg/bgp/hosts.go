package bgp

import (
	"context"
	"fmt"
	log "log/slog"
	"net"

	"github.com/osrg/gobgp/v4/pkg/apiutil"
)

// AddHost will update peers of a host
func (b *Server) AddHost(ctx context.Context, addr string, object string) error {
	b.mtx.Lock()
	defer b.mtx.Unlock()

	objects, exists := b.tracker[addr]

	ip, _, err := net.ParseCIDR(addr)
	if err != nil {
		return err
	}

	p := b.getPath(ip)
	if p == nil {
		return fmt.Errorf("failed to get path for %v", ip)
	}

	// The tracker can outlive the GoBGP server or retain a route after a
	// failed withdrawal. Re-adding the path keeps the daemon and tracker in
	// sync; GoBGP treats an identical path as an idempotent operation.
	if _, err := b.s.AddPath(apiutil.AddPathRequest{
		Paths: []*apiutil.Path{p},
	}); err != nil {
		return err
	}

	if !exists {
		objects = make(map[string]bool)
		b.tracker[addr] = objects
	}

	objects[object] = true
	log.Debug("[BGP] added host", "addr", addr, "cnt", len(objects), "object", object)

	return nil
}

// DelHost will inform peers to remove a host
func (b *Server) DelHost(ctx context.Context, addr string, object string) error {
	b.mtx.Lock()
	defer b.mtx.Unlock()

	objects, exists := b.tracker[addr]
	if !exists {
		log.Debug("[BGP] deleting host - nothing to delete", "addr", addr, "object", object)
		return nil
	}

	ip, _, err := net.ParseCIDR(addr)
	if err != nil {
		return err
	}

	remaining := len(objects)
	if _, ok := objects[object]; ok {
		remaining--
	}

	if remaining == 0 {
		p := b.getPath(ip)
		if p == nil {
			return nil
		}

		if err := b.s.DeletePath(apiutil.DeletePathRequest{
			Paths: []*apiutil.Path{p},
		}); err != nil {
			return err
		}
		delete(b.tracker, addr)
		log.Debug("[BGP] deleted host", "addr", addr, "cnt", remaining, "object", object)
	} else {
		delete(objects, object)
		log.Debug("[BGP] deleting from tracker only", "addr", addr, "object", object)
	}

	return nil
}
