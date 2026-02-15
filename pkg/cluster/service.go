package cluster

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/arp"
	"github.com/kube-vip/kube-vip/pkg/backend"
	"github.com/kube-vip/kube-vip/pkg/bgp"
	"github.com/kube-vip/kube-vip/pkg/election"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/loadbalancer"
	"github.com/kube-vip/kube-vip/pkg/utils"
	"github.com/kube-vip/kube-vip/pkg/vip"
	"github.com/vishvananda/netlink"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

func (cluster *Cluster) vipService(ctx context.Context, c *kubevip.Config, sm *election.Manager, bgpServer *bgp.Server, cancelLeaderElection context.CancelFunc) error {
	var err error

	// listen for interrupts or the Linux SIGTERM signal and cancel
	// our context, which the leader election code will observe and
	// step down
	signalChan := make(chan os.Signal, 1)
	// Add Notification for Userland interrupt
	signal.Notify(signalChan, syscall.SIGINT)

	// Add Notification for SIGTERM (sent from Kubernetes)
	signal.Notify(signalChan, syscall.SIGTERM)

	shouldClose := false
	if cluster.completed == nil {
		cluster.completed = make(chan bool, 1)
		shouldClose = true
	}

	go func() {
		<-ctx.Done()
		signalChan <- syscall.SIGINT
		if shouldClose {
			defer close(cluster.completed)
		}
	}()

	loadbalancers := []*loadbalancer.IPVSLoadBalancer{}

	var arpWG sync.WaitGroup

	for i := range cluster.Network {
		network := cluster.Network[i]

		if network.IsDDNS() {
			if err := cluster.StartDDNS(ctx, cluster.Network[i], c.DHCPBackoffAttempts); err != nil {
				log.Error("failed to start DDNS", "err", err)
			}
		}

		if err := network.SetMask(c.VIPSubnet); err != nil {
			return fmt.Errorf("failed to set mask for subnet %q: %w", c.VIPSubnet, err)
		}

		// start the dns updater if address is dns
		if network.IsDNS() {
			log.Info("starting the DNS updater", "address", network.DNSName())
			ipUpdater := vip.NewIPUpdater(network)
			ipUpdater.Run(ctx)
		}

		if !c.EnableRoutingTable {
			// Normal VIP addition, use skipDAD=false for normal DAD process
			if _, err = network.AddIP(false, false); err != nil {
				return fmt.Errorf("failed to add IP address %s: %w", network.IP(), err)
			}
		}

		if c.EnableBGP {
			// Lets advertise the VIP over BGP, the host needs to be passed using CIDR notation
			log.Debug("Attempting to advertise over BGP", "address", network.CIDR())
			err = bgpServer.AddHost(ctx, network.CIDR())
			if err != nil {
				log.Error(err.Error())
			}
		}

		if c.EnableLoadBalancer {
			lb, err := loadbalancer.NewIPVSLB(network.IP(), c.LoadBalancerPort, c.LoadBalancerForwardingMethod, c.BackendHealthCheckInterval, c.Interface, cancelLeaderElection, signalChan)
			if err != nil {
				return fmt.Errorf("creating IPVS LoadBalance: %w", err)
			}

			go func() {
				err = sm.NodeWatcher(ctx, lb, c.Port)
				if err != nil {
					log.Error("Error watching node labels", "err", err)
					if errors.Is(err, &utils.PanicError{}) {
						signalChan <- syscall.SIGINT
					}
				}
			}()

			loadbalancers = append(loadbalancers, lb)
		}

		if c.EnableARP {
			arpWG.Add(1)
			go cluster.layer2Update(ctx, network, c, &arpWG)
		}
	}

	if c.EnableLoadBalancer {
		// Shutdown function that will wait on this signal, unless we call it ourselves
		<-signalChan
		for _, lb := range loadbalancers {
			err = lb.RemoveIPVSLB()
			if err != nil {
				log.Error("Error stopping IPVS LoadBalancer", "err", err)
			}
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
			if ips, err = getNodeIPs(ctx, nodename, sm.KubernetesClient); err != nil && !apierrors.IsNotFound(err) {
				log.Error("failed to get IP of control-plane node", "err", err)
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

			log.Info("no IP address found for node - will fallback to use localhost address", "addresses", ips)
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
				network := cluster.Network[i]
				networkIP := network.IP()
				isNetworkV6, err := isV6(networkIP)
				if err != nil {
					log.Error("failed to check IP type", "IP", networkIP, "error", err)
					continue
				}
				log.Debug("current ip to process", "ip", networkIP)

				backendMap := &backendMapV4
				if isNetworkV6 {
					backendMap = &backendMapV6
				}

				for entry := range *backendMap {
					log.Debug("entry.Check() for entry", "entry", entry)
					if entry.Check() {
						log.Debug("entry.Check() true")
						// Normal VIP addition with precheck, use skipDAD=false for normal DAD process
						_, err = network.AddIP(true, false)
						if err != nil {
							log.Error("error adding address", "err", err)
						}
						if !(*backendMap)[entry] {
							log.Info("added backend", "ip", network.IP())
						}

						err = network.AddRoute(true)
						if err != nil && !errors.Is(err, fs.ErrExist) && !errors.Is(err, syscall.ESRCH) {
							log.Warn(err.Error())
						} else if err == nil && !(*backendMap)[entry] {
							log.Info("added route", "route", network.PrepareRoute().String())
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
					err = network.DeleteRoute()
					if err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ESRCH) {
						log.Warn("deleting route", "err", err)
					} else if err == nil {
						log.Info("deleted route", "route", network.PrepareRoute().String())
					}

					deleted, err := network.DeleteIP()
					if err != nil {
						log.Error("error deleting IP", "err", err)
						signalChan <- syscall.SIGINT
						return
					}
					if deleted {
						log.Info("deleted address", "IP", network.IP(), "interface", network.Interface())
					}
				}
			}
		}, c.BackendHealthCheckInterval, stop)
	}

	if c.EnableARP {
		arpWG.Wait()
	}

	if c.EnableBGP {
		<-signalChan
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
func (cluster *Cluster) StartLoadBalancerService(ctx context.Context, c *kubevip.Config, bgp *bgp.Server, name string, CountRouteReferences func(*netlink.Route) int) error {
	// use a Go context so we can tell the arp loop code when we
	// want to step down
	//nolint
	ctxArp, cancelArp := context.WithCancel(ctx)

	cluster.stop = make(chan bool, 1)
	cluster.completed = make(chan bool, 1)

	var arpWG sync.WaitGroup

	log.Debug("StartLoadBalancerService", "networks", len(cluster.Network))
	for i := range cluster.Network {
		network := cluster.Network[i]

		if network.IsDDNS() {
			ddnsReady := make(chan struct{})
			go func() {
				ctxDDNS, ddnsCancel := context.WithCancel(ctx)
				defer ddnsCancel()

				// start the DDNS if requested
				log.Debug("(svcs) start DDNS", "name", network.DNSName())
				if err := cluster.StartDDNS(ctxDDNS, cluster.Network[i], c.DHCPBackoffAttempts); err != nil {
					log.Error("failed to start DDNS", "err", err)
				}

				close(ddnsReady)
				<-cluster.stop
			}()
			<-ddnsReady
		}

		log.Debug("current ip to process", "ip", network.IP(), "mask", c.VIPSubnet)
		if err := network.SetMask(c.VIPSubnet); err != nil {
			log.Error("failed to set mask", "subnet", c.VIPSubnet, "err", err)
			cancelArp()
			return utils.NewPanicError(fmt.Sprintf("failed to set mask for subnet %q: %s", c.VIPSubnet, err.Error()))
		}
		_, err := network.DeleteIP()
		if err != nil {
			log.Warn("attempted to clean existing VIP", "err", err)
		}
		log.Debug("config flags", "enable_routing_table", c.EnableRoutingTable, "enable_leader_election", c.EnableLeaderElection, "enable_services_election", c.EnableServicesElection)

		if c.EnableRoutingTable && (c.EnableLeaderElection || c.EnableServicesElection) {
			err = network.AddRoute(false)
			if err != nil {
				log.Warn(err.Error())
			} else {
				log.Info("successful add Route")
			}
		}

		// Normal VIP addition, use skipDAD=false for normal DAD process
		if _, err = network.AddIP(false, false); err != nil {
			log.Warn(err.Error())
		} else {
			log.Info("successful add IP")
		}

		if c.EnableARP {
			arpWG.Add(1)
			go cluster.layer2Update(ctxArp, network, c, &arpWG)
		}

		if c.EnableBGP && (c.EnableLeaderElection || c.EnableServicesElection) {
			// Lets advertise the VIP over BGP, the host needs to be passed using CIDR notation
			log.Debug("(svcs) attempting to advertise over BGP", "address", network.CIDR())
			err = bgp.AddHost(ctx, network.CIDR())
			if err != nil {
				log.Error(err.Error())
			}
		}
	}

	go func() {
		for i := range cluster.Network {
			network := cluster.Network[i]

			ctxDNS, dnsCancel := context.WithCancel(ctx)
			defer dnsCancel()

			// start the dns updater if address is dns
			if network.IsDNS() {
				log.Info("(svcs) starting the DNS updater", "address", network.DNSName(), "ip", network.IP())
				ipUpdater := vip.NewIPUpdater(network)
				ipUpdater.Run(ctxDNS)
			}
		}

		<-cluster.stop
		// Stop the Arp context if it is running
		cancelArp()

		arpWG.Wait() // wait for all cluster ARP/NDP to be finished

		log.Info("[LOADBALANCER] Stopping load balancers", "name", name)

		if c.EnableRoutingTable {
			for i := range cluster.Network {
				// chek if route is not  referenced by another service
				r := cluster.Network[i].PrepareRoute()
				if CountRouteReferences(r) < 1 {
					log.Info("[VIP] Deleting Route for VIP", "IP", cluster.Network[i].IP())
					if err := cluster.Network[i].DeleteRoute(); err != nil {
						log.Warn(err.Error())
					}
				}
			}

			close(cluster.completed)
			return
		}
		for i := range cluster.Network {
			if c.EnableARP && cluster.arpMgr.Count(cluster.Network[i].ARPName()) > 1 {
				continue
			}

			// Handle VIP cleanup based on configuration
			if c.PreserveVIPOnLeadershipLoss {
				// For IPv6, we must remove VIPs immediately to avoid DAD failures on the new leader
				// IPv6 Duplicate Address Detection will fail if the new leader tries to add an IP
				// that is still present on this node's interface
				if utils.IsIPv6(cluster.Network[i].IP()) {
					log.Info("[VIP] Removing IPv6 VIP immediately (required to prevent DAD failures on new leader)", "ip", cluster.Network[i].IP())
					deleted, err := cluster.Network[i].DeleteIP()
					if err != nil {
						log.Warn(err.Error())
					}
					if deleted {
						log.Info("deleted address", "IP", cluster.Network[i].IP(), "interface", cluster.Network[i].Interface())
					}
				} else {
					log.Info("[VIP] Preserving IPv4 VIP address on interface, only stopped ARP broadcasting", "ip", cluster.Network[i].IP())
				}
			} else {
				// Legacy behavior: delete VIP addresses on leadership loss
				log.Info("[VIP] Deleting VIP", "ip", cluster.Network[i].IP())
				deleted, err := cluster.Network[i].DeleteIP()
				if err != nil {
					log.Warn(err.Error())
				}
				if deleted {
					log.Info("deleted address", "IP", cluster.Network[i].IP(), "interface", cluster.Network[i].Interface())
				}
			}
		}

		close(cluster.completed)
	}()

	return nil
}

// Layer2Update, handles the creation of the
func (cluster *Cluster) layer2Update(ctx context.Context, network vip.Network, c *kubevip.Config, arpWG *sync.WaitGroup) {
	defer arpWG.Done()
	var ndp *vip.NdpResponder
	var err error
	ipString := network.IP()
	if utils.IsIPv6(ipString) {
		if network.IPisLinkLocal() {
			log.Error("layer2 is link-local can't use NDP", "address", ipString)
		} else {
			ndp, err = waitNDPResponder(ctx, network.Interface())
			if err != nil {
				log.Error("failed to create new NDP Responder", "error", err)
			} else {
				if ndp != nil {
					defer ndp.Close()
				}
			}
		}
	}

	log.Info("layer 2 broadcaster starting", "IP", network.IP(), "device", network.Interface())
	log.Debug("layer 2 update", "ip", ipString, "interface", network.Interface(), "ms", c.ArpBroadcastRate)

	arpInstance := arp.NewInstance(network, ndp)
	cluster.arpMgr.Insert(arpInstance)

	<-ctx.Done() // if cancel() execute
	log.Debug("ending layer 2 update", "ip", ipString, "interface", network.Interface(), "ms", c.ArpBroadcastRate)
	cluster.arpMgr.RemoveOnLeadershipLoss(arpInstance)
}

func waitNDPResponder(ctx context.Context, ifaceName string) (*vip.NdpResponder, error) {
	ndp, err := vip.NewNDPResponder(ifaceName)
	if err != nil && strings.Contains(err.Error(), "no such device") {
		log.Warn("unable to create NDP responder at first try", "interface", ifaceName, "err", err)
		ndpCreateCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		ticker := time.NewTicker(time.Second)

		for {
			select {
			case <-ndpCreateCtx.Done():
				return nil, fmt.Errorf("failed to create NDP responder for interface %q: %w", ifaceName, ndpCreateCtx.Err())
			case <-ticker.C:
				ndp, err = vip.NewNDPResponder(ifaceName)
				if err != nil {
					log.Warn("unable to create NDP responder on retry", "interface", ifaceName, "err", err)
				} else {
					return ndp, nil
				}
			}
		}
	} else if err != nil {
		return nil, fmt.Errorf("unable to create NDP responder for interface %q: %w", ifaceName, err)
	}
	return ndp, nil
}
