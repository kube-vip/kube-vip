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
	b.mtx.Lock()
	defer b.mtx.Unlock()

	addr, object := route.Prefix, route.Object
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

	if !objects[object] {
		return nil
	}
	if len(objects) == 1 {
		p := b.getPathWithNextHop(ip, b.nextHops[addr])
		if p == nil {
			return nil
		}

		if err := b.s.DeletePath(apiutil.DeletePathRequest{
			Paths: []*apiutil.Path{p},
		}); err != nil {
			return err
		}
		delete(b.tracker, addr)
		delete(b.nextHops, addr)
		log.Debug("[BGP] deleted host", "addr", addr, "cnt", len(objects), "object", object)
	} else {
		delete(objects, object)
		log.Debug("[BGP] deleting from tracker only", "addr", addr, "object", object)
	}

	return nil
}
