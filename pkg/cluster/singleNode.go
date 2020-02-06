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

	// Managers for Vip load balancers and none-vip loadbalancers
	nonVipLB := loadbalancer.LBManager{}
	VipLB := loadbalancer.LBManager{}

	// Iterate through all Configurations
	for x := range c.LoadBalancers {
		// If the load balancer doesn't bind to the VIP
		if c.LoadBalancers[x].BindToVip == false {
			err := nonVipLB.Add("", &c.LoadBalancers[x])
			if err != nil {
				log.Warnf("Error creating loadbalancer [%s] type [%s] -> error [%s]", c.LoadBalancers[x].Name, c.LoadBalancers[x].Type, err)
			}

		}
	}

	if !disableVIP {
		err := cluster.network.AddIP()
		if err != nil {
			log.Warnf("%v", err)
		}

		// Once we have the VIP running, start the load balancer(s) that bind to the VIP
		for x := range c.LoadBalancers {

			if c.LoadBalancers[x].BindToVip == true {
				err = VipLB.Add(c.VIP, &c.LoadBalancers[x])
				if err != nil {
					log.Warnf("Error creating loadbalancer [%s] type [%s] -> error [%s]", c.LoadBalancers[x].Name, c.LoadBalancers[x].Type, err)
				}
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
