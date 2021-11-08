package cluster

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kube-vip/kube-vip/pkg/bgp"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/loadbalancer"
	"github.com/kube-vip/kube-vip/pkg/packet"
	"github.com/kube-vip/kube-vip/pkg/vip"
	"github.com/packethost/packngo"
	log "github.com/sirupsen/logrus"
)

func (cluster *Cluster) vipService(c *kubevip.Config, sm *Manager, bgpServer *bgp.Server, ctxArp, ctxDNS context.Context, packetClient *packngo.Client) error {
	id, err := os.Hostname()
	if err != nil {
		return err
	}

	// listen for interrupts or the Linux SIGTERM signal and cancel
	// our context, which the leader election code will observe and
	// step down
	signalChan := make(chan os.Signal, 1)
	// Add Notification for Userland interrupt
	signal.Notify(signalChan, syscall.SIGINT)

	// Add Notification for SIGTERM (sent from Kubernetes)
	signal.Notify(signalChan, syscall.SIGTERM)

	if cluster.Network.IsDDNS() {
		if err := cluster.StartDDNS(ctxDNS); err != nil {
			log.Error(err)
		}
	}

	// start the dns updater if address is dns
	if cluster.Network.IsDNS() {
		log.Infof("starting the DNS updater for the address %s", cluster.Network.DNSName())
		ipUpdater := vip.NewIPUpdater(cluster.Network)
		ipUpdater.Run(ctxDNS)
	}

	err = cluster.Network.AddIP()
	if err != nil {
		log.Warnf("%v", err)
	}

	if c.EnableMetal {
		// We're not using Packet with BGP
		if !c.EnableBGP {
			// Attempt to attach the EIP in the standard manner
			log.Debugf("Attaching the Packet EIP through the API to this host")
			err = packet.AttachEIP(packetClient, c, id)
			if err != nil {
				log.Error(err)
			}
		}
	}

	if c.EnableBGP {
		// Lets advertise the VIP over BGP, the host needs to be passed using CIDR notation
		cidrVip := fmt.Sprintf("%s/%s", cluster.Network.IP(), c.VIPCIDR)
		log.Debugf("Attempting to advertise the address [%s] over BGP", cidrVip)

		err = bgpServer.AddHost(cidrVip)
		if err != nil {
			log.Error(err)
		}
	}

	if c.EnableLoadBalancer {

		log.Infof("Starting IPVS LoadBalancer")

		lb, err := loadbalancer.NewIPVSLB(c.VIP, c.LoadBalancerPort)
		if err != nil {
			log.Errorf("Error creating IPVS LoadBalancer [%s]", err)
		}

		go func() {
			err = sm.NodeWatcher(lb)
			if err != nil {
				log.Errorf("Error watching node labels [%s]", err)
			}
		}()
		// Shutdown function that will wait on this signal, unless we call it ourselves
		go func() {
			<-signalChan
			err = lb.RemoveIPVSLB()
			if err != nil {
				log.Errorf("Error stopping IPVS LoadBalancer [%s]", err)
			}
			log.Info("Stopping IPVS LoadBalancer")
		}()
	}

	if c.EnableARP {
		//ctxArp, cancelArp = context.WithCancel(context.Background())

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
		//ctxArp, cancelArp = context.WithCancel(context.Background())

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
