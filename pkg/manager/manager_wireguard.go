package manager

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/iptables"
	"github.com/kube-vip/kube-vip/pkg/nftables"
	"github.com/kube-vip/kube-vip/pkg/sysctl"
	"github.com/kube-vip/kube-vip/pkg/vip"
	"github.com/kube-vip/kube-vip/pkg/wireguard"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

const controlPlaneLock = "plndr-cp-lock"

// Start will begin the Manager, which will start services and watch the configmap
func (sm *Manager) startWireguard(ctx context.Context, id string) error {
	var ns string
	var err error

	// use a Go context so we can tell the leaderelection code when we
	// want to step down
	wgCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Load WireGuard tunnel configurations from secret
	log.Info("reading wireguard tunnel configurations from Kubernetes secret")
	tunnelMgr := wireguard.NewTunnelManager()
	err = tunnelMgr.LoadConfigurationsFromSecret(wgCtx, sm.clientSet, sm.config.Namespace, "wireguard")
	if err != nil {
		return fmt.Errorf("failed to load WireGuard tunnel configurations: %w", err)
	}

	configuredVIPs := tunnelMgr.ListConfiguredTunnels()
	log.Info("loaded WireGuard tunnel configurations", "vips", configuredVIPs)

	// For control plane, we need a single tunnel with the configured VIP
	var wg *wireguard.WireGuard
	if sm.config.EnableControlPlane {
		// Check if we have a tunnel config for the control plane VIP
		if !tunnelMgr.HasConfigForVIP(sm.config.VIP) {
			return fmt.Errorf("no WireGuard tunnel configuration found for control plane VIP %s", sm.config.VIP)
		}
		// We'll bring up the tunnel when we become leader
	}

	// For services, pass the tunnel manager to the service processor
	if sm.config.EnableServices && sm.svcProcessor != nil {
		sm.svcProcessor.TunnelMgr = tunnelMgr
		log.Info("WireGuard tunnel manager configured for services")
	}
	signalChan := make(chan os.Signal, 1)

	// Add Notification for Userland interrupt
	signal.Notify(signalChan, syscall.SIGINT)
	// Add Notification for SIGTERM (sent from Kubernetes)
	signal.Notify(signalChan, syscall.SIGTERM)

	var closing atomic.Bool

	// Shutdown function that will wait on this signal, unless we call it ourselves
	go sm.waitForShutdown(wgCtx, cancel, nil)

	ns, err = returnNameSpace()
	if err != nil {
		log.Warn("unable to auto-detect namespace", "dropping to", sm.config.Namespace)
		ns = sm.config.Namespace
	}

	if _, err := sysctl.EnableProcSys("/proc/sys/net/ipv4/conf/all/src_valid_mark"); err != nil {
		return fmt.Errorf("net.ipv4.conf.all.src_valid_mark is disabled and could not be enabled %w", err)
	}
	if _, err := sysctl.EnableProcSys("/proc/sys/net/ipv4/conf/all/route_localnet"); err != nil {
		return fmt.Errorf("net.ipv4.conf.all.route_localnet is disabled and could not be enabled %w", err)
	}

	if sm.config.EnableControlPlane {
		// Get Kubernetes service IP and port from environment
		kubeAPIHost := os.Getenv("KUBERNETES_SERVICE_HOST")
		kubeAPIPort := os.Getenv("KUBERNETES_SERVICE_PORT_HTTPS")
		if kubeAPIHost == "" || kubeAPIPort == "" {
			return fmt.Errorf("KUBERNETES_SERVICE_HOST or KUBERNETES_SERVICE_PORT_HTTPS not set")
		}

		log.Info("beginning controlplane leadership", "namespace", ns, "lock name", controlPlaneLock, "id", id)
		// we use the Lease lock type since edits to Leases are less common
		// and fewer objects in the cluster watch "all Leases".
		lock := &resourcelock.LeaseLock{
			LeaseMeta: metav1.ObjectMeta{
				Name:      controlPlaneLock,
				Namespace: ns,
			},
			Client: sm.clientSet.CoordinationV1(),
			LockConfig: resourcelock.ResourceLockConfig{
				Identity: id,
			},
		}

		// start the leader election code loop
		leaderelection.RunOrDie(wgCtx, leaderelection.LeaderElectionConfig{
			Lock: lock,
			// IMPORTANT: you MUST ensure that any code you have that
			// is protected by the lease must terminate **before**
			// you call cancel. Otherwise, you could have a background
			// loop still running and another process could
			// get elected before your background loop finished, violating
			// the stated goal of the lease.
			ReleaseOnCancel: true,
			LeaseDuration:   time.Duration(sm.config.LeaseDuration) * time.Second,
			RenewDeadline:   time.Duration(sm.config.RenewDeadline) * time.Second,
			RetryPeriod:     time.Duration(sm.config.RetryPeriod) * time.Second,
			Callbacks: leaderelection.LeaderCallbacks{
				OnStartedLeading: func(ctx context.Context) {
					log.Info("started leading", "id", id)

					// Bring up the WireGuard tunnel for control plane VIP
					err = tunnelMgr.BringUpTunnelForVIP(sm.config.VIP)
					if err != nil {
						log.Error("could not start wireguard tunnel for control plane", "vip", sm.config.VIP, "err", err)
						_ = tunnelMgr.TearDownTunnelForVIP(sm.config.VIP)
						if !closing.Load() {
							sm.signalChan <- syscall.SIGINT
						}
						return
					}

					// Get the tunnel to access its configuration
					wg = tunnelMgr.GetTunnelForVIP(sm.config.VIP)
					if wg == nil {
						log.Error("failed to get wireguard tunnel after bringing up", "vip", sm.config.VIP)
						if !closing.Load() {
							sm.signalChan <- syscall.SIGINT
						}
						return
					}

					tunnelConfig := tunnelMgr.GetConfigForVIP(sm.config.VIP)
					if tunnelConfig == nil {
						log.Error("failed to get tunnel configuration", "vip", sm.config.VIP)
						_ = tunnelMgr.TearDownTunnelForVIP(sm.config.VIP)
						if !closing.Load() {
							sm.signalChan <- syscall.SIGINT
						}
						return
					}

					// Strip CIDR notation from VIP if present
					vipIP := sm.config.VIP
					if strings.Contains(vipIP, "/") {
						ip, _, err := net.ParseCIDR(vipIP)
						if err != nil {
							log.Error("could not parse VIP CIDR", "err", err, "vip", vipIP)
							_ = wg.Down()
							panic("could not parse VIP CIDR")
						}
						vipIP = ip.String()
					}

					// Parse Kubernetes API port
					kubeAPIPortInt, err := strconv.ParseUint(kubeAPIPort, 10, 16)
					if err != nil {
						log.Error("could not parse KUBERNETES_SERVICE_PORT_HTTPS", "err", err, "port", kubeAPIPort)
						_ = wg.Down()
						panic("could not parse KUBERNETES_SERVICE_PORT_HTTPS")
					}

					// Apply nftables DNAT rule to route traffic from wireguard interface:6443 to Kubernetes API service
					log.Info("applying nftables DNAT rule",
						"interface", tunnelConfig.InterfaceName,
						"vip", vipIP,
						"sourcePort", 6443,
						"kubeAPIHost", kubeAPIHost,
						"kubeAPIPort", kubeAPIPort)
					err = nftables.ApplyAPIServerDNAT(tunnelConfig.InterfaceName, vipIP, kubeAPIHost, 6443, uint16(kubeAPIPortInt), "controlplane", false)
					if err != nil {
						log.Error("could not apply nftables DNAT rule", "err", err)
						_ = tunnelMgr.TearDownTunnelForVIP(sm.config.VIP)
						panic("could not apply nftables DNAT rule")
					}
					log.Info("nftables DNAT rule applied successfully")

					// Start services watcher if services are enabled
					if sm.config.EnableServices {
						log.Info("starting services watcher for control plane leader")
						if sm.config.EnableServicesElection {
							err = sm.svcProcessor.StartServicesWatchForLeaderElection(ctx)
						} else {
							err = sm.svcProcessor.ServicesWatcher(ctx, sm.svcProcessor.SyncServices)
						}
						if err != nil {
							log.Error("service watcher", "err", err)
							_ = tunnelMgr.TearDownAllTunnels()
							if !closing.Load() {
								sm.signalChan <- syscall.SIGINT
							}
						}
					}

				},
				OnStoppedLeading: func() {
					// we can do cleanup here
					sm.mutex.Lock()
					defer sm.mutex.Unlock()
					log.Info("control plane leader lost", "id", id)

					// Stop services if running
					if sm.config.EnableServices {
						sm.svcProcessor.Stop()
					}

					log.Info("deleting nftables DNAT chains")
					err = nftables.DeleteIngressChains(false, "controlplane")
					if err != nil {
						log.Error("could not delete DNAT ingress chains", "err", err)
					} else {
						log.Info("nftables DNAT chains deleted successfully")
					}

					// Tear down all tunnels (control plane + services)
					err = tunnelMgr.TearDownAllTunnels()
					if err != nil {
						log.Error("failed to tear down tunnels", "err", err)
					}

					log.Error("lost control plane leadership, restarting kube-vip")
					if !closing.Load() {
						sm.signalChan <- syscall.SIGINT
					}
				},
				OnNewLeader: func(identity string) {
					// we're notified when new leader elected
					if identity == id {
						// I just got the lock
						return
					}
					// safety check - tear down tunnel if we're not the leader
					_ = tunnelMgr.TearDownTunnelForVIP(sm.config.VIP)
					log.Info("new leader elected", "id", identity)
				},
			},
		})
	} else if sm.config.EnableServices {
		// This will tidy any dangling kube-vip iptables rules
		if sm.config.EgressClean {
			vip.ClearIPTables(sm.config.EgressWithNftables, sm.config.ServiceNamespace, iptables.ProtocolIPv4)
		}

		// Start a services watcher (all kube-vip pods will watch services), upon a new service
		// a lock based upon that service is created that they will all leaderElection on
		if sm.config.EnableServicesElection {
			log.Info("beginning watching services, leaderelection will happen for every service")
			err = sm.svcProcessor.StartServicesWatchForLeaderElection(wgCtx)
			if err != nil {
				return err
			}
		} else {
			ns, err := returnNameSpace()
			if err != nil {
				log.Warn("unable to auto-detect namespace, dropping to config", "namespace", sm.config.Namespace)
				ns = sm.config.Namespace
			}

			log.Info("beginning services leadership", "namespace", ns, "lock name", sm.config.ServicesLeaseName, "id", id)
			// we use the Lease lock type since edits to Leases are less common
			// and fewer objects in the cluster watch "all Leases".
			lock := &resourcelock.LeaseLock{
				LeaseMeta: metav1.ObjectMeta{
					Name:      sm.config.ServicesLeaseName,
					Namespace: ns,
				},
				Client: sm.clientSet.CoordinationV1(),
				LockConfig: resourcelock.ResourceLockConfig{
					Identity: id,
				},
			}

			// start the leader election code loop
			leaderelection.RunOrDie(wgCtx, leaderelection.LeaderElectionConfig{
				Lock: lock,
				// IMPORTANT: you MUST ensure that any code you have that
				// is protected by the lease must terminate **before**
				// you call cancel. Otherwise, you could have a background
				// loop still running and another process could
				// get elected before your background loop finished, violating
				// the stated goal of the lease.
				ReleaseOnCancel: true,
				LeaseDuration:   time.Duration(sm.config.LeaseDuration) * time.Second,
				RenewDeadline:   time.Duration(sm.config.RenewDeadline) * time.Second,
				RetryPeriod:     time.Duration(sm.config.RetryPeriod) * time.Second,
				Callbacks: leaderelection.LeaderCallbacks{
					OnStartedLeading: func(ctx context.Context) {
						// Note: WireGuard tunnels for services are brought up on-demand
						// when each service is added (one tunnel per service VIP)

						err = sm.svcProcessor.ServicesWatcher(ctx, sm.svcProcessor.SyncServices)
						if err != nil {
							log.Error("service watcher", "err", err)
							// Tear down all service tunnels
							_ = tunnelMgr.TearDownAllTunnels()
							if !closing.Load() {
								sm.signalChan <- syscall.SIGINT
							}
						}
					},
					OnStoppedLeading: func() {
						// we can do cleanup here
						sm.mutex.Lock()
						defer sm.mutex.Unlock()
						log.Info("services leader lost", "id", id)
						sm.svcProcessor.Stop()

						// Tear down all service WireGuard tunnels
						err = tunnelMgr.TearDownAllTunnels()
						if err != nil {
							log.Error("could not tear down all wireguard tunnels", "err", err)
						}

						log.Error("lost services leadership, restarting kube-vip")
						if !closing.Load() {
							sm.signalChan <- syscall.SIGINT
						}
					},
					OnNewLeader: func(identity string) {
						// we're notified when new leader elected
						if identity == id {
							// I just got the lock
							return
						}
						log.Info("new services leader elected", "new leader", identity)
					},
				},
			})
		}
	}

	<-sm.shutdownChan
	log.Info("Shutting down Kube-Vip")

	return nil
}
