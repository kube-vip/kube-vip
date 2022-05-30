package service

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/kube-vip/kube-vip/pkg/bgp"
	"github.com/kube-vip/kube-vip/pkg/equinixmetal"
	"github.com/packethost/packngo"
	log "github.com/sirupsen/logrus"
)

// Start will begin the Manager, which will start services and watch the configmap
func (sm *Manager) startBGP() error {

	// If Packet is enabled then we can begin our preparation work
	var packetClient *packngo.Client
	var err error
	if sm.config.EnableMetal {
		packetClient, err = packngo.NewClient()
		if err != nil {
			log.Error(err)
		}

		// We're using Packet with BGP, popuplate the Peer information from the API
		if sm.config.EnableBGP {
			log.Infoln("Looking up the BGP configuration from packet")
			err = equinixmetal.BGPLookup(packetClient, sm.config)
			if err != nil {
				log.Error(err)
			}
		}
	}

	log.Info("Starting the BGP server to advertise VIP routes to VGP peers")
	sm.bgpServer, err = bgp.NewBGPServer(&sm.config.BGPConfig)
	if err != nil {
		return err
	}

	// Defer a function to check if the bgpServer has been created and if so attempt to close it
	defer func() {
		if sm.bgpServer != nil {
			sm.bgpServer.Close()
		}
	}()

	// use a Go context so we can tell the leaderelection code when we
	// want to step down
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// listen for interrupts or the Linux SIGTERM signal and cancel
	// our context, which the leader election code will observe and
	// step down
	signalChan = make(chan os.Signal, 1)
	// Add Notification for Userland interrupt
	signal.Notify(signalChan, syscall.SIGINT)

	// Add Notification for SIGTERM (sent from Kubernetes)
	signal.Notify(signalChan, syscall.SIGTERM)

	// Add Notification for SIGKILL (sent from Kubernetes)
	//nolint
	signal.Notify(signalChan, syscall.SIGKILL)
	go func() {
		<-signalChan
		log.Info("Received termination, signaling shutdown")
		// Cancel the context, which will in turn cancel the leadership
		cancel()
	}()

	if err := sm.servicesWatcher(ctx); err != nil {
		log.Fatalf("error starting services watcher: %v", err)
	}

	log.Infof("Shutting down Kube-Vip")

	return nil
}
