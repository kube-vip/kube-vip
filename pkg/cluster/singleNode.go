package cluster

import (
	"context"
	"fmt"
	"time"

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

// StartLoadBalancerService will start a VIP instance and leave it for kube-proxy to handle
func (cluster *Cluster) StartLoadBalancerService(c *kubevip.Config, bgp *bgp.Server) error {
	// Start a kube-vip loadbalancer service
	log.Infof("Starting advertising address [%s] with kube-vip", c.VIP)

	// use a Go context so we can tell the arp loop code when we
	// want to step down
	//nolint
	ctxArp, cancelArp := context.WithCancel(context.Background())
	defer cancelArp()

	cluster.stop = make(chan bool, 1)
	cluster.completed = make(chan bool, 1)

	err := cluster.Network.DeleteIP()
	if err != nil {
		log.Warnf("Attempted to clean existing VIP => %v", err)
	}

	err = cluster.Network.AddIP()
	if err != nil {
		log.Warnf("%v", err)
	}

	if c.EnableARP {
		ctxArp, cancelArp = context.WithCancel(context.Background())

		ipString := cluster.Network.IP()

		var ndp *vip.NdpResponder
		if vip.IsIPv6(ipString) {
			ndp, err = vip.NewNDPResponder(c.Interface)
			if err != nil {
				log.Fatalf("failed to create new NDP Responder")
			}
		}
		go func(ctx context.Context) {
			if ndp != nil {
				defer ndp.Close()
			}

			for {

				select {
				case <-ctx.Done(): // if cancel() execute
					return
				default:
					// Ensure the address exists on the interface before attempting to ARP
					set, err := cluster.Network.IsSet()
					if err != nil {
						log.Warnf("%v", err)
					}
					if !set {
						log.Warnf("Re-applying the VIP configuration [%s] to the interface [%s]", ipString, c.Interface)
						err = cluster.Network.AddIP()
						if err != nil {
							log.Warnf("%v", err)
						}
					}

					if vip.IsIPv4(ipString) {
						// Gratuitous ARP, will broadcast to new MAC <-> IPv4 address
						err := vip.ARPSendGratuitous(ipString, c.Interface)
						if err != nil {
							log.Warnf("%v", err)
						}
					} else {
						// Gratuitous NDP, will broadcast new MAC <-> IPv6 address
						err := ndp.SendGratuitous(ipString)
						if err != nil {
							log.Warnf("%v", err)
						}
					}
				}
				time.Sleep(3 * time.Second)
			}
		}(ctxArp)
	}

	if c.EnableBGP {
		// Lets advertise the VIP over BGP, the host needs to be passed using CIDR notation
		cidrVip := fmt.Sprintf("%s/%s", cluster.Network.IP(), c.VIPCIDR)
		log.Debugf("Attempting to advertise the address [%s] over BGP", cidrVip)
		err = bgp.AddHost(cidrVip)
		if err != nil {
			log.Error(err)
		}
	}

	go func() {
		//nolint
		for {
			select {
			case <-cluster.stop:
				// Stop the Arp context if it is running
				cancelArp()

				log.Info("[LOADBALANCER] Stopping load balancers")
				log.Infof("[VIP] Releasing the Virtual IP [%s]", c.VIP)
				err = cluster.Network.DeleteIP()
				if err != nil {
					log.Warnf("%v", err)
				}

				close(cluster.completed)
				return
			}
		}
	}()
	log.Infoln("Started Load Balancer and Virtual IP")
	return nil
}
