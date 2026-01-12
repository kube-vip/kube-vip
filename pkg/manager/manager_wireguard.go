package manager

import (
	"context"
	"sync/atomic"
	"strings"
	"syscall"
	"time"

	log "log/slog"

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
	routes := string(s.Data["routes"])
	wg := wireguard.NewWireGuard(wireguard.WGConfig{
		PrivateKey:    privateKey,
		PeerPublicKey: peerPublicKey,
		PeerEndpoint:  peerEndpoint,
		InterfaceName: "wg0",
		Address:       sm.config.VIP,
		KeepAlive:     time.Duration(5) * time.Second,
		AllowedIPs:    strings.Split(allowedIPs, ","),
		Routes:        strings.Split(routes, ","),
	})

	var closing atomic.Bool

	// Shutdown function that will wait on this signal, unless we call it ourselves
	go sm.waitForShutdown(wgCtx, cancel, nil)

	ns, err = returnNameSpace()
	if err != nil {
		log.Warn("unable to auto-detect namespace", "dropping to", sm.config.Namespace)
		ns = sm.config.Namespace
	}

	// Start a services watcher (all kube-vip pods will watch services), upon a new service
	// a lock based upon that service is created that they will all leaderElection on
	if sm.config.EnableControlPlane {

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
				},
				OnStoppedLeading: func() {
					// we can do cleanup here
					sm.mutex.Lock()
					defer sm.mutex.Unlock()
					log.Info("leader lost", "id", id)
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
