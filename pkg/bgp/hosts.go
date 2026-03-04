package bgp

import (
	"context"
	"fmt"
	"net"

	api "github.com/osrg/gobgp/v3/api"
)

// AddHost will update peers of a host
func (b *Server) AddHost(ctx context.Context, addr string) (err error) {
	b.mtx.Lock()
	defer b.mtx.Unlock()

	cnt, exists := b.tracker[addr]
	if !exists {
		cnt = new(uint)
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

	*cnt++
	b.tracker[addr] = cnt

	return
}

// DelHost will inform peers to remove a host
func (b *Server) DelHost(ctx context.Context, addr string) error {
	b.mtx.Lock()
	defer b.mtx.Unlock()

	cnt, exists := b.tracker[addr]
	if !exists {
		return nil
	}

	if *cnt > 0 {
		ip, _, err := net.ParseCIDR(addr)
		if err != nil {
			return err
		}
		p := b.getPath(ip)
		if p == nil {
			return nil
		}

		if *cnt < 2 {
			if err := b.s.DeletePath(ctx, &api.DeletePathRequest{
				Path: p,
			}); err != nil {
				return err
			}
		}

		*cnt = *cnt - 1
		if *cnt <= 0 {
			delete(b.tracker, addr)
		}

		return nil
	}

	return nil
}
