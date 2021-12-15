package cluster

import (
	"context"

	"github.com/packethost/packngo"
	log "github.com/sirupsen/logrus"

	"github.com/kube-vip/kube-vip/pkg/bgp"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/vip"
)

// StartSingleNode will start a single node cluster
func (cluster *Cluster) StartSingleNode(c *kubevip.Config, disableVIP bool) error {
	// Start kube-vip as a single node server

	// TODO - Split all this code out as a separate function
	log.Infoln("Starting kube-vip as a single node cluster")

	log.Info("This node is assuming leadership of the cluster")

	cluster.stop = make(chan bool, 1)
	cluster.completed = make(chan bool, 1)

	if !disableVIP {
		err := cluster.Network.DeleteIP()
		if err != nil {
			log.Warnf("Attempted to clean existing VIP => %v", err)
		}

		err = cluster.Network.AddIP()
		if err != nil {
			log.Warnf("%v", err)
		}

	}

	if c.EnableARP {
		// Gratuitous ARP, will broadcast to new MAC <-> IP
		err := vip.ARPSendGratuitous(cluster.Network.IP(), c.Interface)
		if err != nil {
			log.Warnf("%v", err)
		}
	}

	go func() {
		//nolint
		for {
			select {
			case <-cluster.stop:

				if !disableVIP {

					log.Info("[VIP] Releasing the Virtual IP")
					err := cluster.Network.DeleteIP()
					if err != nil {
						log.Warnf("%v", err)
					}
				}
				close(cluster.completed)
				return
			}
		}
	}()
	log.Infoln("Started Load Balancer and Virtual IP")
	return nil
}

func (cluster *Cluster) StartVipService(c *kubevip.Config, sm *Manager, bgp *bgp.Server, packetClient *packngo.Client) error {
	// use a Go context so we can tell the arp loop code when we
	// want to step down
	ctxArp, cancelArp := context.WithCancel(context.Background())
	defer cancelArp()

	// use a Go context so we can tell the dns loop code when we
	// want to step down
	ctxDNS, cancelDNS := context.WithCancel(context.Background())
	defer cancelDNS()

	// use a Go context so we can tell the forwarder loop code when we
	// want to step down
	ctxFwd, cancelFwd := context.WithCancel(context.Background())
	defer cancelFwd()

	return cluster.vipService(ctxArp, ctxDNS, ctxFwd, c, sm, bgp, packetClient)
}
