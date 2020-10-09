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
	dnsName string
	vip     Network
}

// NewIPUpdater creates a DNSUpdater
func NewIPUpdater(dnsName string, vip Network) IPUpdater {
	return &ipUpdater{
		dnsName: dnsName,
		vip:     vip,
	}
}

// Run runs the IP updater
func (d *ipUpdater) Run(ctx context.Context) {
	go func(ctx context.Context) {
		for {
			select {
			case <-ctx.Done():
				return
			default:
				ip, err := lookupHost(d.dnsName)
				if err != nil {
					log.Warnf("cannot lookup %s: %v", d.dnsName, err)
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
