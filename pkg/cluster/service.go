package cluster

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
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
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// BGPRouteManager allows to manage the routes announced by the BGP server.
type BGPRouteManager interface {
	AddHost(ctx context.Context, addr string, object string) error
	DelHost(ctx context.Context, addr string, object string) error
}

func (cluster *Cluster) StartVipService(ctx context.Context, c *kubevip.Config, em *election.Manager,
	bgpServer BGPRouteManager, killFunc func()) error {

	var err error

	var wg sync.WaitGroup
	defer wg.Wait()

	wg.Go(func() {
		<-ctx.Done()
		killFunc()
	})

	loadbalancers := []*loadbalancer.IPVSLoadBalancer{}

	for i := range cluster.Network {
		network := cluster.Network[i]

		if network.IsDDNS() {
			if err := cluster.StartDDNS(ctx, cluster.Network[i], c.DHCPBackoffAttempts, &wg); err != nil {
				log.Error("failed to start DDNS", "err", err)
			}
		}

		if err := network.SetMask(c.VIPSubnet); err != nil {
			killFunc()
			return fmt.Errorf("failed to set mask for subnet %q: %w", c.VIPSubnet, err)
		}

		// start the dns updater if address is dns
		if network.IsDNS() {
			log.Info("starting the DNS updater", "address", network.DNSName())
			ipUpdater := vip.NewIPUpdater(network)
			wg.Go(func() {
				ipUpdater.Run(ctx)
			})
		}

		if !c.EnableRoutingTable {
			// Normal VIP addition, use skipDAD=false for normal DAD process
			if _, err = network.AddIP(false, false); err != nil {
				log.Error("failed to add IP", "address", network.IP(), "error", err)
			}
		}

		if c.EnableBGP {
			if c.ControlPlaneHealthCheck.Address != "" {
				// The health check loop owns route advertisement/withdrawal when configured.
				wg.Go(func() {
					cluster.bgpHealthCheckLoop(ctx, c, bgpServer, network.CIDR())
				})
			} else {
				// Lets advertise the VIP over BGP, the host needs to be passed using CIDR notation.
				log.Debug("Attempting to advertise over BGP", "address", network.CIDR())
				err = bgpServer.AddHost(ctx, network.CIDR(), c.NodeName)
				if err != nil {
					log.Error(err.Error())
				}
			}
		}

		if c.EnableLoadBalancer {
			lb, err := loadbalancer.NewIPVSLB(ctx, network.IP(), c.LoadBalancerPort, c.LoadBalancerForwardingMethod,
				c.BackendHealthCheckInterval, killFunc, &wg)
			if err != nil {
				killFunc()
				return fmt.Errorf("creating IPVS LoadBalance: %w", err)
			}

			wg.Go(func() {
				for {
					select {
					case <-ctx.Done():
						return
					default:
						err = em.NodeWatcher(ctx, lb, c.Port)
						if err != nil {
							log.Error("Error watching node labels", "err", err)
							if errors.Is(err, &utils.PanicError{}) {
								killFunc()
								return
							}
						}
					}
				}
			})

			loadbalancers = append(loadbalancers, lb)
		}

		if c.EnableARP {
			wg.Go(func() {
				cluster.layer2Update(ctx, network, c)
			})
		}
	}

	if c.EnableLoadBalancer {
		// Shutdown function that will wait on this signal, unless we call it ourselves
		<-ctx.Done()
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

		ips := []string{}
		if c.NodeName != "" {
			if ips, err = getNodeIPs(ctx, c.NodeName, em.KubernetesClient); err != nil && !apierrors.IsNotFound(err) {
				log.Error("failed to get IP of control-plane node", "err", err)
			}
		}

		if len(ips) == 0 {
			if !utils.IsIPv6(cluster.Network[0].IP()) {
				ips = append(ips, "127.0.0.1")
			} else {
				ips = append(ips, "::1")
			}

			log.Info("no IP address found for node - will fallback to use localhost address", "addresses", ips)
		}

		for _, ip := range ips {
			entry := backend.Entry{Addr: ip, Port: c.Port}
			if !utils.IsIPv6(ip) {
				backendMapV4[entry] = false
			} else {
				backendMapV6[entry] = false
			}
		}

		backend.Watch(ctx, c.BackendHealthCheckInterval, func() {
			for i := range cluster.Network {
				network := cluster.Network[i]
				networkIP := network.IP()
				isNetworkV6 := utils.IsIPv6(networkIP)
				log.Debug("current ip to process", "ip", networkIP)

				backendMap := &backendMapV4
				if isNetworkV6 {
					backendMap = &backendMapV6
				}

				for entry := range *backendMap {
					log.Debug("entry.Check() for entry", "entry", entry)
					var healthy bool
					if c.ControlPlaneHealthCheck.Address != "" {
						req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, c.ControlPlaneHealthCheck.Address, nil)
						if reqErr != nil {
							log.Error("create health check request", "err", reqErr)
						} else if resp, doErr := cluster.healthCheckHTTPClient.Do(req); doErr != nil {
							log.Error("health check request failed", "url", c.ControlPlaneHealthCheck.Address, "err", doErr)
						} else {
							resp.Body.Close()
							healthy = resp.StatusCode == http.StatusOK
							if !healthy {
								log.Warn("health check returned non-200 status", "url", c.ControlPlaneHealthCheck.Address, "status", resp.StatusCode)
							}
						}
					} else {
						healthy = entry.Check()
					}
					if healthy {
						log.Debug("entry.Check() true")
						// Normal VIP addition with precheck, use skipDAD=false for normal DAD process
						_, err = network.AddIP(true, false)
						if err != nil {
							log.Error("error adding address", "err", err)
						}
						if !(*backendMap)[entry] {
							log.Info("added backend", "ip", network.IP())
						}

						err = cluster.routeMgr.Add(c.NodeName, network, true, false)
						if err != nil && !errors.Is(err, fs.ErrExist) && !errors.Is(err, syscall.ESRCH) {
							log.Warn(err.Error())
						} else if err == nil && !(*backendMap)[entry] {
							log.Info("added route", "route", network.PrepareRoute())
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
					err = cluster.routeMgr.Delete(c.NodeName, network)
					if err != nil {
						log.Warn("deleting route", "err", err)
					}

					deleted, err := network.DeleteIP()
					if err != nil {
						log.Error("error deleting IP", "err", err)
						killFunc()
						return
					}
					if deleted {
						log.Info("deleted address", "IP", network.IP(), "interface", network.Interface())
					}
				}
			}
		})
	}

	if c.EnableBGP {
		<-ctx.Done()
	}

	return nil
}

func (cluster *Cluster) bgpHealthCheckLoop(ctx context.Context, c *kubevip.Config, bgpServer BGPRouteManager, vipCIDR string) {
	period := time.Duration(c.ControlPlaneHealthCheck.PeriodSeconds) * time.Second

	consecutiveFailures := 0
	routeAnnounced := false
	ticker := time.NewTicker(period)
	defer ticker.Stop()

	log.Info("Starting BGP health check",
		"address", c.ControlPlaneHealthCheck.Address,
		"cidr", vipCIDR,
		"period", period,
		"timeout", cluster.healthCheckHTTPClient.Timeout,
		"threshold", c.ControlPlaneHealthCheck.FailureThreshold,
	)

	for {
		statusCode := 0
		var healthErr error

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.ControlPlaneHealthCheck.Address, nil)
		if err != nil {
			healthErr = err
		} else {
			resp, err := cluster.healthCheckHTTPClient.Do(req)
			if err != nil {
				healthErr = err
			} else {
				defer resp.Body.Close()
				statusCode = resp.StatusCode
			}
		}

		healthy := healthErr == nil && statusCode == http.StatusOK

		if healthy {
			consecutiveFailures = 0
			if !routeAnnounced {
				log.Info("BGP health check passed, announcing route", "cidr", vipCIDR)
				if err := bgpServer.AddHost(ctx, vipCIDR, c.NodeName); err != nil {
					log.Error("BGP health check: failed to announce route", "cidr", vipCIDR, "err", err)
				} else {
					routeAnnounced = true
				}
			}
		} else {
			consecutiveFailures++
			if healthErr != nil {
				log.Warn("BGP health check failed", "address", c.ControlPlaneHealthCheck.Address, "consecutive", consecutiveFailures, "err", healthErr)
			} else {
				log.Warn("BGP health check failed", "address", c.ControlPlaneHealthCheck.Address, "consecutive", consecutiveFailures, "status", statusCode)
			}

			if consecutiveFailures >= c.ControlPlaneHealthCheck.FailureThreshold && routeAnnounced {
				log.Warn("BGP health check threshold reached, withdrawing route", "failureThreshold", c.ControlPlaneHealthCheck.FailureThreshold, "cidr", vipCIDR)
				if err := bgpServer.DelHost(ctx, vipCIDR, c.NodeName); err != nil {
					log.Error("BGP health check: failed to withdraw route", "cidr", vipCIDR, "err", err)
				} else {
					routeAnnounced = false
				}
			}
		}

		select {
		case <-ctx.Done():
			if routeAnnounced {
				if err := bgpServer.DelHost(ctx, vipCIDR, c.NodeName); err != nil {
					log.Error("BGP health check: failed to withdraw route", "cidr", vipCIDR, "err", err)
				}
			}
			return
		case <-ticker.C:
		}
	}
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
func (cluster *Cluster) StartLoadBalancerService(ctx context.Context, c *kubevip.Config, bgp *bgp.Server, name string, wg *sync.WaitGroup) error {
	// use a Go context so we can tell the arp loop code when we
	// want to step down
	//nolint
	lbCtx, lbCancel := context.WithCancel(ctx)

	var lbWg sync.WaitGroup

	for i := range cluster.Network {
		network := cluster.Network[i]

		if network.IsDDNS() {
			ddnsReady := make(chan struct{})
			lbWg.Go(func() {
				// start the DDNS if requested
				log.Debug("(svcs) start DDNS", "name", network.DNSName())
				if err := cluster.StartDDNS(lbCtx, cluster.Network[i], c.DHCPBackoffAttempts, &lbWg); err != nil {
					log.Error("failed to start DDNS", "err", err)
				}

				close(ddnsReady)
				<-lbCtx.Done()
			})
			<-ddnsReady
		}

		log.Debug("current ip to process", "ip", network.IP(), "mask", c.VIPSubnet)
		if err := network.SetMask(c.VIPSubnet); err != nil {
			log.Error("failed to set mask", "subnet", c.VIPSubnet, "err", err)
			lbCancel()
			return utils.NewPanicError(fmt.Sprintf("failed to set mask for subnet %q: %s", c.VIPSubnet, err.Error()))
		}
		_, err := network.DeleteIP()
		if err != nil {
			log.Warn("attempted to clean existing VIP", "err", err)
		}
		log.Debug("config flags", "enable_routing_table", c.EnableRoutingTable, "enable_leader_election", c.EnableLeaderElection, "enable_services_election", c.EnableServicesElection)

		if c.EnableRoutingTable && (c.EnableLeaderElection || c.EnableServicesElection) {
			err = cluster.routeMgr.Add(name, network, false, false)
			if err != nil {
				log.Warn(err.Error())
			} else {
				log.Info("successful add Route")
			}
		}

		if !c.EnableRoutingTable && !c.EnableBGP && !c.EnableWireguard {
			// Normal VIP addition, use skipDAD=false for normal DAD process
			// Note: When WireGuard is enabled, the VIP is added to the tunnel interface
			// instead of lo, so we skip adding it here.
			if _, err = network.AddIP(false, false); err != nil {
				log.Warn(err.Error())
			} else {
				log.Info("successful add IP")
			}
		}

		if c.EnableARP {
			lbWg.Go(func() {
				cluster.layer2Update(lbCtx, network, c)
			})
		}

		if c.EnableBGP && (c.EnableLeaderElection || c.EnableServicesElection) {
			// Lets advertise the VIP over BGP, the host needs to be passed using CIDR notation
			log.Debug("(svcs) attempting to advertise over BGP", "address", network.CIDR())
			err = bgp.AddHost(lbCtx, network.CIDR(), name)
			if err != nil {
				log.Error(err.Error())
			}
		}
	}

	wg.Go(func() {
		for i := range cluster.Network {
			network := cluster.Network[i]

			// start the dns updater if address is dns
			if network.IsDNS() {
				log.Info("(svcs) starting the DNS updater", "address", network.DNSName(), "ip", network.IP())
				ipUpdater := vip.NewIPUpdater(network)
				wg.Go(func() {
					ipUpdater.Run(lbCtx)
				})
			}
		}

		select {
		case <-cluster.stop:
		case <-ctx.Done():
		}

		// Stop the loadbalancer context if it is running
		lbCancel()

		lbWg.Wait() // wait for all cluster ARP/NDP to be finished

		log.Info("[LOADBALANCER] Stopping load balancers", "name", name)

		if c.EnableRoutingTable {
			for i := range cluster.Network {
				if err := cluster.routeMgr.Delete(name, cluster.Network[i]); err != nil {
					log.Warn(err.Error())
				}
			}

			return
		}

		cluster.cleanupVIPs(c)
	})

	return nil
}

// Layer2Update, handles the creation of the
func (cluster *Cluster) layer2Update(ctx context.Context, network vip.Network, c *kubevip.Config) {
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
