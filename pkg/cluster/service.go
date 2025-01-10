package cluster

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kube-vip/kube-vip/pkg/backend"
	"github.com/kube-vip/kube-vip/pkg/bgp"
	"github.com/kube-vip/kube-vip/pkg/equinixmetal"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/loadbalancer"
	"github.com/kube-vip/kube-vip/pkg/vip"
	"github.com/packethost/packngo"
	log "github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

func (cluster *Cluster) vipService(ctxArp, ctxDNS context.Context, c *kubevip.Config, sm *Manager, bgpServer *bgp.Server, packetClient *packngo.Client) error {
	var err error

	// listen for interrupts or the Linux SIGTERM signal and cancel
	// our context, which the leader election code will observe and
	// step down
	signalChan := make(chan os.Signal, 1)
	// Add Notification for Userland interrupt
	signal.Notify(signalChan, syscall.SIGINT)

	// Add Notification for SIGTERM (sent from Kubernetes)
	signal.Notify(signalChan, syscall.SIGTERM)

	for i := range cluster.Network {
		if cluster.Network[i].IsDDNS() {
			if err := cluster.StartDDNS(ctxDNS); err != nil {
				log.Error(err)
			}
		}

		// start the dns updater if address is dns
		if cluster.Network[i].IsDNS() {
			log.Infof("starting the DNS updater for the address %s", cluster.Network[i].DNSName())
			ipUpdater := vip.NewIPUpdater(cluster.Network[i])
			ipUpdater.Run(ctxDNS)
		}

		if !c.EnableRoutingTable {
			if c.EnableARP {
				subnets := vip.Split(c.VIPCIDR)
				subnet := ""
				if len(subnets) > 0 {
					subnet = subnets[0]
				}
				if vip.IsIPv6(cluster.Network[i].IP()) && len(subnets) > 1 {
					subnet = subnets[1]
				}
				if subnet == "" {
					log.Fatalf("no subnet provided for IP %s", cluster.Network[i].IP())
				}
				if err = cluster.Network[i].SetMask(subnet); err != nil {
					log.Fatalf("failed to set mask %s: %s", subnet, err.Error())
				}
			}
			if err = cluster.Network[i].AddIP(false); err != nil {
				log.Fatalf("%v", err)
			}
		}

		if c.EnableMetal {
			// We're not using Equinix Metal with BGP
			if !c.EnableBGP {
				// Attempt to attach the EIP in the standard manner
				log.Debugf("Attaching the Equinix Metal EIP through the API to this host")
				err = equinixmetal.AttachEIP(packetClient, c, c.NodeName)
				if err != nil {
					log.Error(err)
				}
			}
		}

		if c.EnableBGP {
			// Lets advertise the VIP over BGP, the host needs to be passed using CIDR notation
			cidrVip := fmt.Sprintf("%s/%s", cluster.Network[i].IP(), c.VIPCIDR)
			log.Debugf("Attempting to advertise the address [%s] over BGP", cidrVip)

			err = bgpServer.AddHost(cidrVip)
			if err != nil {
				log.Error(err)
			}
		}

		if c.EnableLoadBalancer {
			log.Infof("Starting IPVS LoadBalancer")

			lb, err := loadbalancer.NewIPVSLB(cluster.Network[i].IP(), c.LoadBalancerPort, c.LoadBalancerForwardingMethod, c.BackendHealthCheckInterval)
			if err != nil {
				log.Errorf("Error creating IPVS LoadBalancer [%s]", err)
			}

			go func() {
				err = sm.NodeWatcher(lb, c.Port)
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
			go func(ctx context.Context, i int) {
				ipString := cluster.Network[i].IP()
				isIPv6 := vip.IsIPv6(ipString)

				var ndp *vip.NdpResponder
				if isIPv6 {
					ndp, err = vip.NewNDPResponder(cluster.Network[i].Interface())
					if err != nil {
						log.Fatalf("failed to create new NDP Responder")
					}
				}

				if ndp != nil {
					defer ndp.Close()
				}
				log.Infof("Gratuitous Arp broadcast will repeat every 3 seconds for [%s/%s]", ipString, cluster.Network[i].Interface())
				for {
					select {
					case <-ctx.Done(): // if cancel() execute
						return
					default:
						cluster.ensureIPAndSendGratuitous(i, ndp)
					}
					time.Sleep(3 * time.Second)
				}
			}(ctxArp, i)
		}
	}

	if c.EnableRoutingTable {
		backendMapV4 := backend.Map{}
		backendMapV6 := backend.Map{}
		// only check localhost

		nodename := ""
		if c.NodeName != "" {
			nodename = c.NodeName
		} else {
			nodename = os.Getenv("HOSTNAME")
		}

		ips := []string{}
		if nodename != "" {
			if ips, err = getNodeIPs(ctxArp, nodename, sm.KubernetesClient); err != nil && !apierrors.IsNotFound(err) {
				log.Error("failed to get IP of control-plane node: %w", err)
			}
		}

		if len(ips) == 0 {
			isV6, err := isV6(cluster.Network[0].IP())
			if err != nil {
				return fmt.Errorf("failed to parse IP '%s'", cluster.Network[0].IP())
			}
			if !isV6 {
				ips = append(ips, "127.0.0.1")
			} else {
				ips = append(ips, "::1")
			}

			log.Infof("no IP address found for node - will fallback to use localhost address: %v", ips)
		}

		for _, ip := range ips {
			entry := backend.Entry{Addr: ip, Port: c.Port}
			ipv6, err := isV6(ip)
			if err != nil {
				log.Error("failed to check IP type", "IP", ip, "error", err)
			}
			if !ipv6 {
				backendMapV4[entry] = false
			} else {
				backendMapV6[entry] = false
			}
		}

		stop := make(chan struct{})

		// will wait for system interrupt and will send stop signal to backend watch
		go func() {
			<-signalChan
			stop <- struct{}{}
		}()

		backend.Watch(func() {
			for i := range cluster.Network {
				networkIP := cluster.Network[i].IP()
				isNetworkV6, err := isV6(networkIP)
				if err != nil {
					log.Error("failed to check IP type", "IP", networkIP, "error", err)
					continue
				}

				backendMap := &backendMapV4
				if isNetworkV6 {
					backendMap = &backendMapV6
				}

				for entry := range *backendMap {
					if entry.Check() {
						err = cluster.Network[i].AddIP(true)
						if err != nil {
							log.Fatalf("error adding IP: %v", err)
						}
						if !(*backendMap)[entry] {
							log.Infof("added IP: %s", cluster.Network[i].IP())
						}

						err = cluster.Network[i].AddRoute(true)
						if err != nil && !errors.Is(err, fs.ErrExist) && !errors.Is(err, syscall.ESRCH) {
							log.Warnf("%v", err)
						} else if err == nil && !(*backendMap)[entry] {
							log.Infof("added route: %s", cluster.Network[i].PrepareRoute().String())
						}

						(*backendMap)[entry] = true
						break
					}
					(*backendMap)[entry] = false
				}

				deleteAddress := true
				for entry := range *backendMap {
					if (*backendMap)[entry] {
						deleteAddress = false
						break
					}
				}

				if deleteAddress {
					err = cluster.Network[i].DeleteRoute()
					if err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ESRCH) {
						log.Warnf("error while deleting route: %v", err)
					} else if err == nil {
						log.Infof("deleted route: %s", cluster.Network[i].PrepareRoute().String())
					}

					isSet, err := cluster.Network[i].IsSet()
					if err != nil {
						log.Error("failed to check IP address", "error", err)
					}
					if isSet {
						err = cluster.Network[i].DeleteIP()
						if err != nil {
							log.Fatalf("error deleting IP: %v", err)
						}
						log.Infof("deleted IP: %s", cluster.Network[i].IP())
					}
				}
			}
		}, c.BackendHealthCheckInterval, stop)
	}

	return nil
}

func isV6(ip string) (bool, error) {
	ipaddr := net.ParseIP(ip)
	if ipaddr == nil {
		return false, fmt.Errorf("failed to parse IP '%s'", ip)
	}
	return ipaddr.To4() == nil, nil
}

func getNodeIPs(ctx context.Context, nodename string, client *kubernetes.Clientset) ([]string, error) {
	node, err := client.CoreV1().Nodes().Get(ctx, nodename, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return []string{}, fmt.Errorf("failed to get data about '%s' node: %w", nodename, err)
	}
	ips := []string{}
	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeInternalIP {
			ips = append(ips, addr.Address)
		}
	}
	return ips, nil
}

// StartLoadBalancerService will start a VIP instance and leave it for kube-proxy to handle
func (cluster *Cluster) StartLoadBalancerService(c *kubevip.Config, bgp *bgp.Server) {
	// use a Go context so we can tell the arp loop code when we
	// want to step down
	//nolint
	ctxArp, cancelArp := context.WithCancel(context.Background())

	cluster.stop = make(chan bool, 1)
	cluster.completed = make(chan bool, 1)

	for i := range cluster.Network {
		network := cluster.Network[i]

		err := network.DeleteIP()
		if err != nil {
			log.Warnf("Attempted to clean existing VIP => %v", err)
		}
		if c.EnableRoutingTable && (c.EnableLeaderElection || c.EnableServicesElection) {
			err = network.AddRoute(false)
			if err != nil {
				log.Warnf("%v", err)
			}
		} else if !c.EnableRoutingTable {
			if c.EnableARP {
				subnets := vip.Split(c.VIPCIDR)
				subnet := ""
				if len(subnets) > 0 {
					subnet = subnets[0]
				}
				if vip.IsIPv6(cluster.Network[i].IP()) && len(subnets) > 1 {
					subnet = subnets[1]
				}
				if subnet == "" {
					log.Fatalf("no subnet provided for IP %s", cluster.Network[i].IP())
				}
				if err = network.SetMask(subnet); err != nil {
					log.Fatalf("failed to set mask %s: %s", subnet, err.Error())
				}
			}
			if err = network.AddIP(false); err != nil {
				log.Warnf("%v", err)
			}
		}

		if c.EnableARP {
			ipString := network.IP()
			var ndp *vip.NdpResponder
			if vip.IsIPv6(ipString) {
				ndp, err = vip.NewNDPResponder(network.Interface())
				if err != nil {
					log.Fatalf("failed to create new NDP Responder")
				}
			}
			go func(ctx context.Context) {
				if ndp != nil {
					defer ndp.Close()
				}
				log.Debugf("(svcs) broadcasting ARP update for %s via %s, every %dms", ipString, network.Interface(), c.ArpBroadcastRate)

				for {
					select {
					case <-ctx.Done(): // if cancel() execute
						log.Debugf("(svcs) ending ARP update for %s via %s, every %dms", ipString, network.Interface(), c.ArpBroadcastRate)
						return
					default:
						cluster.ensureIPAndSendGratuitous(i, ndp)
					}
					if c.ArpBroadcastRate < 500 {
						log.Errorf("arp broadcast rate is [%d], this shouldn't be lower that 300ms (defaulting to 3000)", c.ArpBroadcastRate)
						c.ArpBroadcastRate = 3000
					}
					time.Sleep(time.Duration(c.ArpBroadcastRate) * time.Millisecond)
				}
			}(ctxArp)
		}

		if c.EnableBGP && (c.EnableLeaderElection || c.EnableServicesElection) {
			// Lets advertise the VIP over BGP, the host needs to be passed using CIDR notation
			cidrVip := fmt.Sprintf("%s/%s", network.IP(), c.VIPCIDR)
			log.Debugf("(svcs) attempting to advertise the address [%s] over BGP", cidrVip)
			err = bgp.AddHost(cidrVip)
			if err != nil {
				log.Error(err)
			}
		}
	}

	go func() {
		<-cluster.stop
		// Stop the Arp context if it is running
		cancelArp()

		log.Info("[LOADBALANCER] Stopping load balancers")

		if c.EnableRoutingTable {
			for i := range cluster.Network {
				log.Infof("[VIP] Deleting Route for Virtual IP [%s]", cluster.Network[i].IP())
				if err := cluster.Network[i].DeleteRoute(); err != nil {
					log.Warnf("%v", err)
				}
			}

			close(cluster.completed)
			return
		}
		for i := range cluster.Network {
			log.Infof("[VIP] Releasing the Virtual IP [%s]", cluster.Network[i].IP())
			if err := cluster.Network[i].DeleteIP(); err != nil {
				log.Warnf("%v", err)
			}
		}

		close(cluster.completed)
	}()
}

// ensureIPAndSendGratuitous - adds IP to the interface if missing, and send
// either a gratuitous ARP or gratuitous NDP. Re-adds the interface if it is IPv6
// and in a dadfailed state.
func (cluster *Cluster) ensureIPAndSendGratuitous(index int, ndp *vip.NdpResponder) {
	iface := cluster.Network[index].Interface()
	ipString := cluster.Network[index].IP()

	// Check if IP is dadfailed
	if cluster.Network[index].IsDADFAIL() {
		log.Warnf("IP address is in dadfailed state, removing [%s] from interface [%s]", ipString, iface)
		err := cluster.Network[index].DeleteIP()
		if err != nil {
			log.Warnf("%v", err)
		}
	}

	// Ensure the address exists on the interface before attempting to ARP
	set, err := cluster.Network[index].IsSet()
	if err != nil {
		log.Warnf("%v", err)
	}
	if !set {
		log.Warnf("Re-applying the VIP configuration [%s] to the interface [%s]", ipString, iface)
		err = cluster.Network[index].AddIP(false)
		if err != nil {
			log.Warnf("%v", err)
		}
	}

	if vip.IsIPv6(ipString) {
		// Gratuitous NDP, will broadcast new MAC <-> IPv6 address
		if ndp == nil {
			log.Error("NDP responder was not created")
		} else {
			err := ndp.SendGratuitous(ipString)
			if err != nil {
				log.Warnf("%v", err)
			}
		}

	} else {
		// Gratuitous ARP, will broadcast to new MAC <-> IPv4 address
		err := vip.ARPSendGratuitous(ipString, iface)
		if err != nil {
			log.Warnf("%v", err)
		}
	}

}
