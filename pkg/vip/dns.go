package vip

import (
	"context"
	"time"

	log "log/slog"
)

// IPUpdater is the interface to plug dns updaters
type IPUpdater interface {
	Run(ctx context.Context)
}

type ipUpdater struct {
	vip Network
}

// NewIPUpdater creates a DNSUpdater
func NewIPUpdater(vip Network) IPUpdater {
	return &ipUpdater{
		vip: vip,
	}
}

// Run runs the IP updater
func (d *ipUpdater) Run(ctx context.Context) {
	go func(ctx context.Context) {
		for {
			select {
			case <-ctx.Done():
				log.Info("stop ipUpdater")
				return
			default:
				mode := "ipv4"
				if IsIPv6(d.vip.IP()) {
					mode = "ipv6"
				}

				ip, err := LookupHost(d.vip.DNSName(), mode)
				if err != nil {
					log.Warn("cannot lookup", "name", d.vip.DNSName(), "err", err)
					// fallback to renewing the existing IP
					ip = []string{d.vip.IP()}
				}

				log.Info("setting IP", "address", ip)
				if err := d.vip.SetIP(ip[0]); err != nil {
					log.Error("setting IP", "address", ip, "err", err)
				}

				if err := d.vip.AddIP(false); err != nil {
					log.Error("error adding virtual IP", "err", err)
				}

			}
			time.Sleep(3 * time.Second)
		}
	}(ctx)
}
