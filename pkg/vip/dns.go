package vip

import (
	"context"
	"time"

	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/utils"
)

const (
	defaultInterval = 3
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
				if utils.IsIPv6(d.vip.IP()) {
					mode = "ipv6"
				}

				ip, err := utils.LookupHost(d.vip.DNSName(), mode, true)
				if err != nil {
					log.Warn("cannot lookup", "name", d.vip.DNSName(), "err", err)
					// fallback to renewing the existing IP
					ip = []string{d.vip.IP()}
				}

				if err := d.vip.SetIP(ip[0]); err != nil {
					log.Error("setting IP", "address", ip, "err", err)
				}

				// Normal VIP addition for DNS, use skipDAD=false for normal DAD process
				if _, err := d.vip.AddIP(true, false, defaultInterval); err != nil {
					log.Error("error adding virtual IP", "err", err)
				}

			}
			time.Sleep(defaultInterval * time.Second)
		}
	}(ctx)
}
