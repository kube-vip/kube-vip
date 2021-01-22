package bgp

import (
	"context"
	"net"

	api "github.com/osrg/gobgp/api"
)

// AddHost will update peers of a host
func (b *Server) AddHost(addr string) (err error) {
	ip, _, err := net.ParseCIDR(addr)
	if err != nil {
		return err
	}
	p := b.getPath(ip)
	if p == nil {
		return err
	}

	_, err = b.s.AddPath(context.Background(), &api.AddPathRequest{
		Path: p,
	})

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
