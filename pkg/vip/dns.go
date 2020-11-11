package vip

import (
	"context"
	"time"

	log "github.com/sirupsen/logrus"
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
				log.Infof("stop ipUpdater")
				return
			default:
				ip, err := lookupHost(d.vip.DNSName())
				if err != nil {
					log.Warnf("cannot lookup %s: %v", d.vip.DNSName(), err)
					// fallback to renewing the existing IP
					ip = d.vip.IP()
				}

				log.Infof("setting %s as an IP", ip)
				d.vip.SetIP(ip)
				d.vip.AddIP()

			}
			time.Sleep(3 * time.Second)
		}
	}(ctx)
}
