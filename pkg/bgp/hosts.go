package bgp

import (
	"context"
	"fmt"
	log "log/slog"
	"net"

	api "github.com/osrg/gobgp/v3/api"
)

const KVNodename = "kube-vip-control-plane"

// AddHost will update peers of a host
func (b *Server) AddHost(ctx context.Context, addr string, object string) error {
	b.mtx.Lock()
	defer b.mtx.Unlock()

	objects, exists := b.tracker[addr]

	if !exists {
		b.tracker[addr] = make(map[string]bool)
		objects = b.tracker[addr]

		ip, _, err := net.ParseCIDR(addr)
		if err != nil {
			return err
		}

		p := b.getPath(ip)
		if p == nil {
			return fmt.Errorf("failed to get path for %v", ip)
		}

		if _, err := b.s.AddPath(ctx, &api.AddPathRequest{
			Path: p,
		}); err != nil {
			return err
		}
		log.Debug("[BGP] added host", "addr", addr, "cnt", len(objects)+1)
	}

	objects[object] = true

	return nil
}

// DelHost will inform peers to remove a host
func (b *Server) DelHost(ctx context.Context, addr string, object string) error {
	b.mtx.Lock()
	defer b.mtx.Unlock()

	objects, exists := b.tracker[addr]
	if !exists {
		log.Debug("[BGP] deleting host - nothing to delete", "addr", addr)
		return nil
	}

	ip, _, err := net.ParseCIDR(addr)
	if err != nil {
		return err
	}

	delete(objects, object)

	if len(objects) == 0 {
		p := b.getPath(ip)
		if p == nil {
			return nil
		}

		if err := b.s.DeletePath(ctx, &api.DeletePathRequest{
			Path: p,
		}); err != nil {
			return err
		}
		delete(b.tracker, addr)
		log.Debug("[BGP] deleted host", "addr", addr, "cnt", len(objects))
	}

	return nil
}
