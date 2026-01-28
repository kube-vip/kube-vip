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

	"github.com/kube-vip/kube-vip/pkg/nftables"
	"github.com/kube-vip/kube-vip/pkg/sysctl"
	"github.com/kube-vip/kube-vip/pkg/wireguard"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// Start will begin the Manager, which will start services and watch the configmap
func (sm *Manager) startWireguard(ctx context.Context, id string) error {
	var ns string
	var err error

	// use a Go context so we can tell the leaderelection code when we
	// want to step down
	wgCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	log.Info("reading wireguard peer configuration from Kubernetes secret")
	s, err := sm.clientSet.CoreV1().Secrets(sm.config.Namespace).Get(wgCtx, "wireguard", metav1.GetOptions{})
	if err != nil {
		return err
	}
	// parse all the details needed for Wireguard
	peerPublicKey := string(s.Data["peerPublicKey"])
	peerEndpoint := string(s.Data["peerEndpoint"])
	privateKey := string(s.Data["privateKey"])
	allowedIPs := string(s.Data["allowedIPs"])
	listenPort := string(s.Data["listenPort"])
	if listenPort == "" {
		listenPort = "51820"
	}
	port, err := strconv.Atoi(listenPort)
	if err != nil {
		return fmt.Errorf("failed to convert listenPort to integer: %w", err)
	}
	IPs := make([]string, 0)
	for ip := range strings.SplitSeq(allowedIPs, ",") {
		IPs = append(IPs, strings.TrimSpace(ip))
	}
	cfg := wireguard.WGConfig{
		PrivateKey:    privateKey,
		PeerPublicKey: peerPublicKey,
		PeerEndpoint:  peerEndpoint,
		InterfaceName: "wg0",
		Address:       sm.config.VIP,
		KeepAlive:     time.Duration(5) * time.Second,
		AllowedIPs:    IPs,
		ListenPort:    port,
	}
	wg := wireguard.NewWireGuard(cfg)
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
	// Start a services watcher (all kube-vip pods will watch services), upon a new service
	// a lock based upon that service is created that they will all leaderElection on
	if sm.config.EnableControlPlane {
		// Get Kubernetes service IP and port from environment
		kubeAPIHost := os.Getenv("KUBERNETES_SERVICE_HOST")
		kubeAPIPort := os.Getenv("KUBERNETES_SERVICE_PORT_HTTPS")
		if kubeAPIHost == "" || kubeAPIPort == "" {
			return fmt.Errorf("KUBERNETES_SERVICE_HOST or KUBERNETES_SERVICE_PORT_HTTPS not set")
		}

		log.Info("beginning services leadership", "namespace", ns, "lock name", plunderLock, "id", id)
		// we use the Lease lock type since edits to Leases are less common
		// and fewer objects in the cluster watch "all Leases".
		lock := &resourcelock.LeaseLock{
			LeaseMeta: metav1.ObjectMeta{
				Name:      plunderLock,
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
					err = wg.Up()
					if err != nil {
						log.Error("could not start wireguard", "err", err)
						_ = wg.Down()
						if !closing.Load() {
							sm.signalChan <- syscall.SIGINT
						}
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

					// Apply nftables DNAT rule to route traffic from wg0:6443 to Kubernetes API service
					log.Info("applying nftables DNAT rule", "interface", "wg0", "vip", vipIP, "sourcePort", 6443, "kubeAPIHost", kubeAPIHost, "kubeAPIPort", kubeAPIPort)
					err = nftables.ApplyAPIServerDNAT("wg0", vipIP, kubeAPIHost, 6443, uint16(kubeAPIPortInt), "controlplane", false)
					if err != nil {
						log.Error("could not apply nftables DNAT rule", "err", err)
						_ = wg.Down()
						panic("could not apply nftables DNAT rule")
					}
					log.Info("nftables DNAT rule applied successfully")

				},
				OnStoppedLeading: func() {
					// we can do cleanup here
					sm.mutex.Lock()
					defer sm.mutex.Unlock()
					log.Info("leader lost", "id", id)

					log.Info("deleting nftables DNAT chains")
					err = nftables.DeleteIngressChains(false, "controlplane")
					if err != nil {
						log.Error("could not delete DNAT ingress chains", "err", err)
					} else {
						log.Info("nftables DNAT chains deleted successfully")
					}

					err = wg.Down()
					if err != nil {
						log.Error(err.Error(), "id", id)
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
					// safety check
					_ = wg.Down()
					log.Info("new leader elected", "id", identity)
				},
			},
		})
	}

	<-sm.shutdownChan
	log.Info("Shutting down Kube-Vip")

	return nil
}
