package manager

import (
	"context"
	"fmt"
	"syscall"

	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/bgp"
	"github.com/kube-vip/kube-vip/pkg/cluster"
	api "github.com/osrg/gobgp/v3/api"
	"github.com/prometheus/client_golang/prometheus"
)

// Start will begin the Manager, which will start services and watch the configmap
func (sm *Manager) startBGP() error {
	var cpCluster *cluster.Cluster
	// var ns string
	var err error

	if sm.bgpServer == nil {
		sm.bgpServer, err = bgp.NewBGPServer(sm.config.BGPConfig)
		if err != nil {
			return fmt.Errorf("creating BGP server: %w", err)
		}
	}

	log.Info("Starting the BGP server to advertise VIP routes to BGP peers")
	if err := sm.bgpServer.Start(func(p *api.WatchEventResponse_PeerEvent) {
		ipaddr := p.GetPeer().GetState().GetNeighborAddress()
		port := uint64(179)
		peerDescription := fmt.Sprintf("%s:%d", ipaddr, port)

		for stateName, stateValue := range api.PeerState_SessionState_value {
			metricValue := 0.0
			if stateValue == int32(p.GetPeer().GetState().GetSessionState().Number()) {
				metricValue = 1
			}

			sm.bgpSessionInfoGauge.With(prometheus.Labels{
				"state": stateName,
				"peer":  peerDescription,
			}).Set(metricValue)
		}
	}); err != nil {
		return fmt.Errorf("starting BGP server: %w", err)
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
		for {
			sig := <-sm.signalChan
			switch sig {
			case syscall.SIGUSR1:
				log.Info("Received SIGUSR1, dumping configuration")
				sm.dumpConfiguration()
			case syscall.SIGINT, syscall.SIGTERM:
				log.Info("Received termination, signaling shutdown")
				if sm.config.EnableControlPlane {
					if cpCluster != nil {
						cpCluster.Stop()
					}
				}
				// Cancel the context, which will in turn cancel the leadership
				cancel()
				return
			}
		}
	}()

	if sm.config.EnableControlPlane {
		cpCluster, err = cluster.InitCluster(sm.config, false, sm.intfMgr, sm.arpMgr)
		if err != nil {
			return err
		}

		clusterManager, err := initClusterManager(sm)
		if err != nil {
			return err
		}

		go func() {
			if sm.config.EnableLeaderElection {
				err = cpCluster.StartCluster(sm.config, clusterManager, sm.bgpServer)
			} else {
				err = cpCluster.StartVipService(sm.config, clusterManager, sm.bgpServer)
			}
			if err != nil {
				log.Error("Control Plane", "err", err)
				// Trigger the shutdown of this manager instance
				sm.signalChan <- syscall.SIGINT
			}
		}()

		// Check if we're also starting the services, if not we can sit and wait on the closing channel and return here
		if !sm.config.EnableServices {
			<-sm.signalChan
			log.Info("Shutting down Kube-Vip")

			return nil
		}
	}

	if sm.config.EnableServicesElection {
		log.Info("beginning watching services, leaderelection will happen for every service")
		err = sm.svcProcessor.StartServicesWatchForLeaderElection(ctx)
		if err != nil {
			return err
		}
	} else {
		log.Info("beginning watching services without leader election")
		err = sm.svcProcessor.ServicesWatcher(ctx, sm.svcProcessor.SyncServices)
		if err != nil {
			return err
		}
	}

	log.Info("Shutting down Kube-Vip")

	return nil
}
