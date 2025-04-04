package cluster

import (
	"context"

	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/bgp"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/vip"
)

// StartSingleNode will start a single node cluster
func (cluster *Cluster) StartSingleNode(c *kubevip.Config, disableVIP bool) error {
	// Start kube-vip as a single node server

	// TODO - Split all this code out as a separate function
	log.Info("Starting kube-vip as a single node cluster")

	log.Info("This node is assuming leadership of the cluster")

	cluster.stop = make(chan bool, 1)
	cluster.completed = make(chan bool, 1)

	for i := range cluster.Network {
		if !disableVIP {
			err := cluster.Network[i].DeleteIP()
			if err != nil {
				log.Warn("Attempted to clean existing VIP", "err", err)
			}

			err = cluster.Network[i].AddIP(false)
			if err != nil {
				log.Warn(err.Error())
			}

		}

		if c.EnableARP {
			// Gratuitous ARP, will broadcast to new MAC <-> IP
			err := vip.ARPSendGratuitous(cluster.Network[i].IP(), c.Interface)
			if err != nil {
				log.Warn(err.Error())
			}
		}
	}

	go func() {
		<-cluster.stop

		if !disableVIP {
			for i := range cluster.Network {
				log.Info("[VIP] Releasing the VIP", "address", cluster.Network[i].IP())
				err := cluster.Network[i].DeleteIP()
				if err != nil {
					log.Warn(err.Error())
				}
			}
		}
		close(cluster.completed)
	}()
	log.Info("Started Load Balancer and Virtual IP")
	return nil
}

func (cluster *Cluster) StartVipService(c *kubevip.Config, sm *Manager, bgp *bgp.Server) error {
	// use a Go context so we can tell the arp loop code when we
	// want to step down
	ctxArp, cancelArp := context.WithCancel(context.Background())
	defer cancelArp()

	// use a Go context so we can tell the dns loop code when we
	// want to step down
	ctxDNS, cancelDNS := context.WithCancel(context.Background())
	defer cancelDNS()

	return cluster.vipService(ctxArp, ctxDNS, c, sm, bgp, nil)
}
