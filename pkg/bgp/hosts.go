package bgp

import (
	"context"
	"fmt"
	"net"

	api "github.com/osrg/gobgp/v3/api"
)

// AddHost will update peers of a host
func (b *Server) AddHost(addr string) (err error) {
	ip, _, err := net.ParseCIDR(addr)
	if err != nil {
		return err
	}

	p := b.getPath(ip)
	if p == nil {
		return fmt.Errorf("failed to get path for %v", ip)
	}

	_, err = b.s.AddPath(context.Background(), &api.AddPathRequest{
		Path: p,
	})

	if err != nil {
		return err
	}

	return
}

// DelHost will inform peers to remove a host
func (b *Server) DelHost(addr string) (err error) {
	ip, _, err := net.ParseCIDR(addr)
	if err != nil {
		return err
	}
	p := b.getPath(ip)
	if p == nil {
		return
	}

	return b.s.DeletePath(context.Background(), &api.DeletePathRequest{
		Path: p,
	})
}
