package cluster

import (
	log "github.com/sirupsen/logrus"

	"github.com/thebsdbox/kube-vip/pkg/kubevip"
	"github.com/thebsdbox/kube-vip/pkg/loadbalancer"
	"github.com/thebsdbox/kube-vip/pkg/vip"
)

// StartSingleNode will start a single node cluster
func (cluster *Cluster) StartSingleNode(c *kubevip.Config, disableVIP bool) error {
	// Start kube-vip as a single node server

	// TODO - Split all this code out as a seperate function
	log.Infoln("Starting kube-vip as a single node cluster")

	log.Info("This node is assuming leadership of the cluster")

	cluster.stop = make(chan bool, 1)
	cluster.completed = make(chan bool, 1)

	if !disableVIP {
		err := cluster.network.AddIP()
		if err != nil {
			log.Warnf("%v", err)
		}
	}
	// Once we have the VIP running, start the load balancer(s) that bind to the VIP

	// Iterate through all Configurations
	for x := range c.LoadBalancers {
		// If the load balancer doesn't bind to the VIP
		if c.LoadBalancers[x].BindToVip == true {
			// DO IT
			if c.LoadBalancers[x].Type == "tcp" {
				loadbalancer.StartTCP(&c.LoadBalancers[x], c.VIP)
			} else if c.LoadBalancers[x].Type == "http" {
				loadbalancer.StartHTTP(&c.LoadBalancers[x], c.VIP)
			} else {
				// If the type isn't one of above then we don't understand it
				log.Warnf("Load Balancer [%s] uses unknown type [%s]", c.LoadBalancers[x].Name, c.LoadBalancers[x].Type)
			}
		}
	}

	if c.GratuitousARP == true {
		// Gratuitous ARP, will broadcast to new MAC <-> IP
		err := vip.ARPSendGratuitous(c.VIP, c.Interface)
		if err != nil {
			log.Warnf("%v", err)
		}
	}

	go func() {
		for {
			select {
			case <-cluster.stop:
				log.Info("Stopping this node")
				close(cluster.completed)
				return
			}
		}
	}()

	return nil
}
