package vip

import (
	"context"
	"time"

	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/metrics"
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
	mode := utils.IPv4Family
	if utils.IsIPv6(d.vip.IP()) {
		mode = utils.IPv6Family
	}

	dnsName := d.vip.DNSName()

	t := time.NewTicker(defaultInterval * time.Second)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info("stop ipUpdater")
			return
		case <-t.C:
			previousIP := d.vip.IP()
			ip, err := utils.LookupHost(dnsName, mode, true)
			if err != nil {
				metrics.DNSResolutionsTotal.WithLabelValues("error").Inc()
				log.Warn("cannot lookup", "name", dnsName, "err", err)
				// fallback to renewing the existing IP
				ip = []string{d.vip.IP()}
			} else {
				metrics.DNSResolutionsTotal.WithLabelValues("ok").Inc()
			}

			setIPErr := d.vip.SetIP(ip[0])
			if setIPErr != nil {
				log.Error("setting IP", "address", ip, "err", setIPErr)
			} else if previousIP != ip[0] {
				metrics.DNSIPChangesTotal.Inc()
			}

			// Normal VIP addition for DNS, use skipDAD=false for normal DAD process
			if _, err := d.vip.AddIP(true, false, defaultInterval); err != nil {
				log.Error("error adding virtual IP", "err", err)
			}
		}
	}
}
