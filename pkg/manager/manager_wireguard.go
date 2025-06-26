package manager

import (
	"context"
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
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	log.Info("reading wireguard peer configuration from Kubernetes secret")
	s, err := sm.clientSet.CoreV1().Secrets(sm.config.Namespace).Get(ctx, "wireguard", metav1.GetOptions{})
	if err != nil {
		return err
	}
	// parse all the details needed for Wireguard
	peerPublicKey := s.Data["peerPublicKey"]
	peerEndpoint := s.Data["peerEndpoint"]
	privateKey := s.Data["privateKey"]

	// Configure the interface to join the Wireguard VPN
	err = wireguard.ConfigureInterface(string(privateKey), string(peerPublicKey), string(peerEndpoint))
	if err != nil {
		return err
	}

	// Shutdown function that will wait on this signal, unless we call it ourselves
	go func() {
		<-sm.signalChan
		log.Info("Received termination, signaling shutdown")

		// Cancel the context, which will in turn cancel the leadership
		cancel()
	}()

	ns, err = returnNameSpace()
	if err != nil {
		log.Warn("unable to auto-detect namespace", "dropping to", sm.config.Namespace)
		ns = sm.config.Namespace
	}

	// Start a services watcher (all kube-vip pods will watch services), upon a new service
	// a lock based upon that service is created that they will all leaderElection on
	if sm.config.EnableServicesElection {
		log.Info("beginning watching services, leaderelection will happen for every service")
		err = sm.startServicesWatchForLeaderElection(ctx)
		if err != nil {
			return err
		}
	} else {

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
		leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
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
					err = sm.servicesWatcher(ctx, sm.syncServices)
					if err != nil {
						log.Error(err.Error())
						panic("")
					}
				},
				OnStoppedLeading: func() {
					// we can do cleanup here
					sm.mutex.Lock()
					defer sm.mutex.Unlock()
					log.Info("leader lost", "id", id)
					for _, instance := range sm.serviceInstances {
						for _, cluster := range instance.Clusters {
							cluster.Stop()
						}
					}

					log.Error("lost leadership, restarting kube-vip")
					panic("")
				},
				OnNewLeader: func(identity string) {
					// we're notified when new leader elected
					if identity == id {
						// I just got the lock
						return
					}
					log.Info("new leader elected", "id", identity)
				},
			},
		})
	}
	return nil
}
