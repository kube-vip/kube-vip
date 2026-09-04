package bgp

import (
	"context"
	"fmt"
	log "log/slog"
	"net"

	"github.com/kube-vip/kube-vip/pkg/metrics"
	"github.com/kube-vip/kube-vip/pkg/utils"
	"github.com/osrg/gobgp/v4/pkg/apiutil"
)

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
			metrics.BGPRouteOperationsTotal.WithLabelValues("add", "error").Inc()
			return err
		}

		p := b.getPath(ip)
		if p == nil {
			metrics.BGPRouteOperationsTotal.WithLabelValues("add", "error").Inc()
			return fmt.Errorf("failed to get path for %v", ip)
		}

		if _, err := b.s.AddPath(apiutil.AddPathRequest{
			Paths: []*apiutil.Path{p},
		}); err != nil {
			metrics.BGPRouteOperationsTotal.WithLabelValues("add", "error").Inc()
			return err
		}
		family := utils.IPv4Family
		if ip.To4() == nil {
			family = utils.IPv6Family
		}
		metrics.BGPRoutesAdvertised.WithLabelValues(family).Inc()
		log.Debug("[BGP] added host", "addr", addr, "cnt", len(objects)+1, "object", object)
	}

	objects[object] = true

	metrics.BGPRouteOperationsTotal.WithLabelValues("add", "ok").Inc()
	return nil
}

// DelHost will inform peers to remove a host
func (b *Server) DelHost(ctx context.Context, addr string, object string) error {
	b.mtx.Lock()
	defer b.mtx.Unlock()

	objects, exists := b.tracker[addr]
	if !exists {
		log.Debug("[BGP] deleting host - nothing to delete", "addr", addr, "object", object)
		metrics.BGPRouteOperationsTotal.WithLabelValues("delete", "ok").Inc()
		return nil
	}

	ip, _, err := net.ParseCIDR(addr)
	if err != nil {
		metrics.BGPRouteOperationsTotal.WithLabelValues("delete", "error").Inc()
		return err
	}

	delete(objects, object)

	if len(objects) == 0 {
		p := b.getPath(ip)
		if p == nil {
			metrics.BGPRouteOperationsTotal.WithLabelValues("delete", "ok").Inc()
			return nil
		}

		if err := b.s.DeletePath(apiutil.DeletePathRequest{
			Paths: []*apiutil.Path{p},
		}); err != nil {
			metrics.BGPRouteOperationsTotal.WithLabelValues("delete", "error").Inc()
			return err
		}
		family := utils.IPv4Family
		if ip.To4() == nil {
			family = utils.IPv6Family
		}
		metrics.BGPRoutesAdvertised.WithLabelValues(family).Dec()
		delete(b.tracker, addr)
		log.Debug("[BGP] deleted host", "addr", addr, "cnt", len(objects), "object", object)
	} else {
		log.Debug("[BGP] deleting from tracker only", "addr", addr, "object", object)
	}

	metrics.BGPRouteOperationsTotal.WithLabelValues("delete", "ok").Inc()
	return nil
}
