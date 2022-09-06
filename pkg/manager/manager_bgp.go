package manager

import (
	"context"
	"os"
	"syscall"

	"github.com/kube-vip/kube-vip/pkg/bgp"
	"github.com/kube-vip/kube-vip/pkg/cluster"
	"github.com/kube-vip/kube-vip/pkg/equinixmetal"
	"github.com/packethost/packngo"
	log "github.com/sirupsen/logrus"
)

// Start will begin the Manager, which will start services and watch the configmap
func (sm *Manager) startBGP() error {
	var cpCluster *cluster.Cluster
	//var ns string
	var err error

	// If Packet is enabled then we can begin our preparation work
	var packetClient *packngo.Client
	if sm.config.EnableMetal {
		if sm.config.ProviderConfig != "" {
			key, project, err := equinixmetal.GetPacketConfig(sm.config.ProviderConfig)
			if err != nil {
				log.Error(err)
			} else {
				// Set the environment variable with the key for the project
				os.Setenv("PACKET_AUTH_TOKEN", key)
				// Update the configuration with the project key
				sm.config.MetalProjectID = project
			}
		}
		packetClient, err = packngo.NewClient()
		if err != nil {
			log.Error(err)
		}

		// We're using Packet with BGP, populate the Peer information from the API
		if sm.config.EnableBGP {
			log.Infoln("Looking up the BGP configuration from packet")
			err = equinixmetal.BGPLookup(packetClient, sm.config)
			if err != nil {
				log.Error(err)
			}
		}
	}

	log.Info("Starting the BGP server to advertise VIP routes to BGP peers")
	sm.bgpServer, err = bgp.NewBGPServer(&sm.config.BGPConfig)
	if err != nil {
		return err
	}

	// use a Go context so we can tell the leaderelection code when we
	// want to step down
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Defer a function to check if the bgpServer has been created and if so attempt to close it
	defer func() {
		if sm.bgpServer != nil {
			sm.bgpServer.Close()
		}
	}()

	// Shutdown function that will wait on this signal, unless we call it ourselves
	go func() {
		<-sm.signalChan
		log.Info("Received termination, signaling shutdown")
		if sm.config.EnableControlPane {
			if cpCluster != nil {
				cpCluster.Stop()
			}
		}
		// Cancel the context, which will in turn cancel the leadership
		cancel()
	}()

	if sm.config.EnableControlPane {

		cpCluster, err = cluster.InitCluster(sm.config, false)
		if err != nil {
			return err
		}

		clusterManager := &cluster.Manager{
			KubernetesClient: sm.clientSet,
			SignalChan:       sm.signalChan,
		}

		go func() {
			err = cpCluster.StartVipService(sm.config, clusterManager, sm.bgpServer, packetClient)
			if err != nil {
				log.Errorf("Control Pane Error [%v]", err)
				// Trigger the shutdown of this manager instance
				sm.signalChan <- syscall.SIGINT
			}
		}()

		// Check if we're also starting the services, if not we can sit and wait on the closing channel and return here
		if !sm.config.EnableServices {
			<-sm.signalChan
			log.Infof("Shutting down Kube-Vip")

			return nil
		}
	}

	err = sm.servicesWatcher(ctx, sm.syncServices)
	if err != nil {
		return err
	}

	log.Infof("Shutting down Kube-Vip")

	return nil
}
