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
				mode := "ipv4"
				if IsIPv6(d.vip.IP()) {
					mode = "ipv6"
				}

				ip, err := LookupHost(d.vip.DNSName(), mode)
				if err != nil {
					log.Warnf("cannot lookup %s: %v", d.vip.DNSName(), err)
					// fallback to renewing the existing IP
					ip = []string{d.vip.IP()}
				}

				log.Infof("setting %s as an IP", ip)
				if err := d.vip.SetIP(ip[0]); err != nil {
					log.Errorf("setting %s as an IP: %v", ip, err)
				}

				if err := d.vip.AddIP(false); err != nil {
					log.Errorf("error adding virtual IP: %v", err)
				}

			}
			time.Sleep(3 * time.Second)
		}
	}(ctx)
}
