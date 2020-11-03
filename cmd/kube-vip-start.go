package cmd

import (
	"context"
	"os"
	"os/signal"

	"github.com/plunder-app/kube-vip/pkg/cluster"
	"github.com/plunder-app/kube-vip/pkg/kubevip"
	"github.com/plunder-app/kube-vip/pkg/vip"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

// Start as a single node (no cluster), start as a leader in the cluster
var startConfig kubevip.Config
var startConfigLB kubevip.LoadBalancer
var startLocalPeer, startKubeConfigPath string
var startRemotePeers, startBackends []string
var inCluster bool

func init() {
	// Get the configuration file
	kubeVipStart.Flags().StringVarP(&configPath, "config", "c", "", "Path to a kube-vip configuration")
	kubeVipStart.Flags().BoolVarP(&disableVIP, "disableVIP", "d", false, "Disable the VIP functionality")

	// Pointers so we can see if they're nil (and not called)
	kubeVipStart.Flags().StringVar(&startConfig.Interface, "interface", "eth0", "Name of the interface to bind to")
	kubeVipStart.Flags().StringVar(&startConfig.VIP, "vip", "192.168.0.1", "The Virtual IP address")
	kubeVipStart.Flags().StringVar(&startConfig.Address, "address", "", "an address (IP or DNS name) to use as a VIP")
	kubeVipStart.Flags().IntVar(&startConfig.Port, "port", 6443, "listen port for the VIP")
	kubeVipStart.Flags().BoolVar(&startConfig.SingleNode, "singleNode", false, "Start this instance as a single node")
	kubeVipStart.Flags().BoolVar(&startConfig.StartAsLeader, "startAsLeader", false, "Start this instance as the cluster leader")
	kubeVipStart.Flags().BoolVar(&startConfig.GratuitousARP, "arp", false, "Use ARP broadcasts to improve VIP re-allocations")
	kubeVipStart.Flags().StringVar(&startLocalPeer, "localPeer", "server1:192.168.0.1:10000", "Settings for this peer, format: id:address:port")
	kubeVipStart.Flags().StringSliceVar(&startRemotePeers, "remotePeers", []string{"server2:192.168.0.2:10000", "server3:192.168.0.3:10000"}, "Comma seperated remotePeers, format: id:address:port")
	// Load Balancer flags
	kubeVipStart.Flags().BoolVar(&startConfigLB.BindToVip, "lbBindToVip", false, "Bind example load balancer to VIP")
	kubeVipStart.Flags().StringVar(&startConfigLB.Type, "lbType", "tcp", "Type of load balancer instance (TCP/HTTP)")
	kubeVipStart.Flags().StringVar(&startConfigLB.Name, "lbName", "Example Load Balancer", "The name of a load balancer instance")
	kubeVipStart.Flags().IntVar(&startConfigLB.Port, "lbPort", 8080, "Port that load balancer will expose on")
	kubeVipStart.Flags().IntVar(&startConfigLB.BackendPort, "lbBackEndPort", 6443, "A port that all backends may be using (optional)")
	kubeVipStart.Flags().StringSliceVar(&startBackends, "lbBackends", []string{"192.168.0.1:8080", "192.168.0.2:8080"}, "Comma seperated backends, format: address:port")

	// Cluster configuration
	kubeVipStart.Flags().StringVar(&startKubeConfigPath, "kubeConfig", "/etc/kubernetes/admin.conf", "The path of a kubernetes configuration file")
	kubeVipStart.Flags().BoolVar(&inCluster, "inCluster", false, "Use the incluster token to authenticate to Kubernetes")
	kubeVipStart.Flags().BoolVar(&startConfig.EnableLeaderElection, "leaderElection", false, "Use the Kubernetes leader election mechanism for clustering")

}

var kubeVipStart = &cobra.Command{
	Use:   "start",
	Short: "Start the Virtual IP / Load balancer",
	Run: func(cmd *cobra.Command, args []string) {
		// Set the logging level for all subsequent functions
		log.SetLevel(log.Level(logLevel))
		var err error

		// If a configuration file is loaded, then it will overwrite flags

		if configPath != "" {
			c, err := kubevip.OpenConfig(configPath)
			if err != nil {
				log.Fatalf("%v", err)
			}
			startConfig = *c
		}

		// parse environment variables, these will overwrite anything loaded or flags
		err = kubevip.ParseEnvironment(&startConfig)
		if err != nil {
			log.Fatalln(err)
		}

		newCluster, err := cluster.InitCluster(&startConfig, disableVIP)
		if err != nil {
			log.Fatalf("%v", err)
		}

		// start the dns updater if the address flag is used and the address isn't an IP
		if startConfig.Address != "" && !vip.IsIP(startConfig.Address) {
			log.Infof("starting the DNS updater for the address %s", startConfig.Address)

			ipUpdater := vip.NewIPUpdater(startConfig.Address, newCluster.Network)

			ipUpdater.Run(context.Background())
		}

		if startConfig.SingleNode {
			// If the Virtual IP isn't disabled then create the netlink configuration
			// Start a single node cluster
			newCluster.StartSingleNode(&startConfig, disableVIP)
		} else {
			if disableVIP {
				log.Fatalln("Cluster mode requires the Virtual IP to be enabled, use single node with no VIP")
			}

			if startConfig.EnableLeaderElection {
				cm, err := cluster.NewManager(startKubeConfigPath, inCluster, startConfig.Port)
				if err != nil {
					log.Fatalf("%v", err)
				}

				// Leader Cluster will block
				err = newCluster.StartLeaderCluster(&startConfig, cm)
				if err != nil {
					log.Fatalf("%v", err)
				}
			} else {

				// // Start a multi-node (raft) cluster, this doesn't block so will wait on signal
				err = newCluster.StartRaftCluster(&startConfig)
				if err != nil {
					log.Fatalf("%v", err)
				}
				signalChan := make(chan os.Signal, 1)
				signal.Notify(signalChan, os.Interrupt)

				<-signalChan

				newCluster.Stop()
			}

		}

	},
}
