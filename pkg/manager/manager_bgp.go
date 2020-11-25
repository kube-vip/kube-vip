package manager

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/packethost/packngo"
	"github.com/plunder-app/kube-vip/pkg/bgp"
	"github.com/plunder-app/kube-vip/pkg/cluster"
	"github.com/plunder-app/kube-vip/pkg/packet"
	log "github.com/sirupsen/logrus"
)

// Start will begin the Manager, which will start services and watch the configmap
func (sm *Manager) startBGP() error {
	var cpCluster *cluster.Cluster
	//var ns string
	var err error

	// If Packet is enabled then we can begin our preperation work
	var packetClient *packngo.Client
	if sm.config.EnablePacket {
		packetClient, err = packngo.NewClient()
		if err != nil {
			log.Error(err)
		}

		// We're using Packet with BGP, popuplate the Peer information from the API
		if sm.config.EnableBGP {
			log.Infoln("Looking up the BGP configuration from packet")
			err = packet.BGPLookup(packetClient, sm.config)
			if err != nil {
				log.Error(err)
			}
		}
	}

	log.Info("Starting the BGP server to adverise VIP routes to VGP peers")
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

	if sm.config.EnableControlPane {
		cpCluster, err = cluster.InitCluster(sm.config, false)
		if err != nil {
			return err
		}
		err = cpCluster.StartLoadBalancerService(sm.config, sm.bgpServer)
		if err != nil {
			return err
		}
		// 	ns = sm.config.Namespace
		// } else {
		// 	ns, err = returnNameSpace()
		// 	if err != nil {
		// 		return err
		// 	}
	}

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
	signal.Notify(signalChan, syscall.SIGKILL)
	go func() {
		<-signalChan
		log.Info("Received termination, signaling shutdown")
		// Cancel the context, which will in turn cancel the leadership
		cancel()
	}()

	sm.servicesWatcher(ctx)

	log.Infof("Shutting down Kube-Vip")

	return nil
}
