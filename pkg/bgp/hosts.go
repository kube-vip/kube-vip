package bgp

import (
	"context"
	"fmt"
	log "log/slog"
	"net"

	api "github.com/osrg/gobgp/v3/api"
)

// AddHost will update peers of a host
func (b *Server) AddHost(ctx context.Context, addr string) (err error) {
	b.mtx.Lock()
	defer b.mtx.Unlock()

	cnt, exists := b.tracker[addr]
	if !exists {
		cnt = 0
	}

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

	cnt++
	b.tracker[addr] = cnt

	log.Debug("[BGP] added host", "addr", addr, "cnt", cnt)

	return
}

// DelHost will inform peers to remove a host
func (b *Server) DelHost(ctx context.Context, addr string) error {
	b.mtx.Lock()
	defer b.mtx.Unlock()

	cnt, exists := b.tracker[addr]
	if !exists {
		log.Debug("[BGP] deleting host - nothing to delete", "addr", addr)
		return nil
	}

	if cnt > 0 {
		ip, _, err := net.ParseCIDR(addr)
		if err != nil {
			return err
		}
		p := b.getPath(ip)
		if p == nil {
			log.Debug("[BGP] deleting host - no path to delete", "addr", addr, "cnt", cnt)
			return nil
		}

		if cnt <= 1 {
			if err := b.s.DeletePath(ctx, &api.DeletePathRequest{
				Path: p,
			}); err != nil {
				return err
			}
		}

		cnt--
		log.Debug("[BGP] deleted host", "addr", addr, "cnt", cnt)

		if cnt < 1 {
			delete(b.tracker, addr)
		} else {
			b.tracker[addr] = cnt
		}
	}

	return nil
}
