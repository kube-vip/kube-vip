package service

import (
	"context"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/kamhlos/upnp"
	log "github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// Start will begin the Manager, which will start services and watch the configmap
func (sm *Manager) startARP() error {
	ns, err := returnNameSpace()
	if err != nil {
		return err
	}

	id, err := os.Hostname()
	if err != nil {
		return err
	}

	// Before starting the leader Election enable any additional functionality
	upnpEnabled, _ := strconv.ParseBool(os.Getenv("enableUPNP"))

	if upnpEnabled {
		sm.upnp = new(upnp.Upnp)
		err := sm.upnp.ExternalIPAddr()
		if err != nil {
			log.Errorf("Error Enabling UPNP %s", err.Error())
			// Set the struct to nil so nothing should use it in future
			sm.upnp = nil
		} else {
			log.Infof("Succesfully enabled UPNP, Gateway address [%s]", sm.upnp.GatewayOutsideIP)
		}
	}

	// // If BGP is enabled then we start the server that will broadcast VIPs
	// if sm.config.EnableBGP {
	// 	// Lets start BGP
	// 	log.Info("Starting the BGP server to adverise VIP routes to VGP peers")
	// 	sm.bgpServer, err = bgp.NewBGPServer(&sm.config.BGPConfig)
	// 	if err != nil {
	// 		log.Error(err)
	// 	}
	// }

	// // Defer a function to check if the bgpServer has been created and if so attempt to close it
	// defer func() {
	// 	if sm.bgpServer != nil {
	// 		sm.bgpServer.Close()
	// 	}
	// }()

	log.Infof("Beginning cluster membership, namespace [%s], lock name [%s], id [%s]", ns, plunderLock, id)
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

	// use a Go context so we can tell the leaderelection code when we
	// want to step down
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// listen for interrupts or the Linux SIGTERM signal and cancel
	// our context, which the leader election code will observe and
	// step down
	signalChan := make(chan os.Signal, 1)
	// Add Notification for Userland interrupt
	signal.Notify(signalChan, syscall.SIGINT)

	// Add Notification for SIGTERM (sent from Kubernetes)
	signal.Notify(signalChan, syscall.SIGTERM)

	// Add Notification for SIGKILL (sent from Kubernetes)
	signal.Notify(signalChan, syscall.SIGKILL)
	go func() {
		<-signalChan
		log.Info("Received termination, signaling shutdown")
		// Cancel the context, which will in turn cancel the leadership
		cancel()
	}()

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
		LeaseDuration:   10 * time.Second,
		RenewDeadline:   5 * time.Second,
		RetryPeriod:     1 * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				sm.watcher(ctx, ns)
			},
			OnStoppedLeading: func() {
				// we can do cleanup here
				log.Infof("leader lost: %s", id)
				for x := range sm.serviceInstances {
					sm.serviceInstances[x].cluster.Stop()
				}
			},
			OnNewLeader: func(identity string) {
				// we're notified when new leader elected
				if identity == id {
					// I just got the lock
					return
				}
				log.Infof("new leader elected: %s", identity)
			},
		},
	})

	//<-signalChan
	log.Infof("Shutting down Kube-Vip")

	return nil
}
