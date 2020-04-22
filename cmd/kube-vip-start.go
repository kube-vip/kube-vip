package cmd

import (
	"os"
	"os/signal"

	"github.com/plunder-app/kube-vip/pkg/cluster"
	"github.com/plunder-app/kube-vip/pkg/kubevip"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

// Start as a single node (no cluster), start as a leader in the cluster
var startConfig kubevip.Config
var startConfigLB kubevip.LoadBalancer
var startLocalPeer string
var startRemotePeers, startBackends []string

func init() {
	// Get the configuration file
	kubeVipStart.Flags().StringVarP(&configPath, "config", "c", "", "Path to a kube-vip configuration")
	kubeVipStart.Flags().BoolVarP(&disableVIP, "disableVIP", "d", false, "Disable the VIP functionality")

	// Pointers so we can see if they're nil (and not called)
	kubeVipStart.Flags().StringVar(&startConfig.Interface, "interface", "eth0", "Name of the interface to bind to")
	kubeVipStart.Flags().StringVar(&startConfig.VIP, "vip", "192.168.0.1", "The Virtual IP addres")
	kubeVipStart.Flags().BoolVar(&startConfig.SingleNode, "singleNode", false, "Start this instance as a single node")
	kubeVipStart.Flags().BoolVar(&startConfig.StartAsLeader, "startAsLeader", false, "Start this instance as the cluster leader")
	kubeVipStart.Flags().BoolVar(&startConfig.GratuitousARP, "arp", false, "Use ARP broadcasts to improve VIP re-allocations")
	kubeVipStart.Flags().StringVar(&startLocalPeer, "localPeer", "server1:192.168.0.1:10000", "Settings for this peer, format: id:address:port")
	kubeVipStart.Flags().StringSliceVar(&startRemotePeers, "remotePeers", []string{"server2:192.168.0.2:10000", "server3:192.168.0.3:10000"}, "Comma seperated remotePeers, format: id:address:port")
	// Load Balancer flags
	kubeVipStart.Flags().BoolVar(&startConfigLB.BindToVip, "lbBindToVip", false, "Bind example load balancer to VIP")
	kubeVipStart.Flags().StringVar(&startConfigLB.Type, "lbType", "tcp", "Type of load balancer instance (tcp/http)")
	kubeVipStart.Flags().StringVar(&startConfigLB.Name, "lbName", "Example Load Balancer", "The name of a load balancer instance")
	kubeVipStart.Flags().IntVar(&startConfigLB.Port, "lbPort", 8080, "Port that load balander will expose on")
	kubeVipStart.Flags().IntVar(&startConfigLB.BackendPort, "lbBackEndPort", 6443, "A port that all backends may be using (optional)")
	kubeVipStart.Flags().StringSliceVar(&startBackends, "lbBackends", []string{"192.168.0.1:8080", "192.168.0.2:8080"}, "Comma seperated backends, format: address:port")
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

		var newCluster *cluster.Cluster

		if startConfig.SingleNode {
			// If the Virtual IP isn't disabled then create the netlink configuration
			newCluster, err = cluster.InitCluster(&startConfig, disableVIP)
			if err != nil {
				log.Fatalf("%v", err)
			}
			// Start a single node cluster
			newCluster.StartSingleNode(&startConfig, disableVIP)
		} else {
			if disableVIP {
				log.Fatalln("Cluster mode requires the Virtual IP to be enabled, use single node with no VIP")
			}

			// If the Virtual IP isn't disabled then create the netlink configuration
			newCluster, err = cluster.InitCluster(&startConfig, disableVIP)
			if err != nil {
				log.Fatalf("%v", err)
			}

			// Start a multi-node (raft) cluster
			err = newCluster.StartCluster(&startConfig)
			if err != nil {
				log.Fatalf("%v", err)
			}
		}

		signalChan := make(chan os.Signal, 1)
		signal.Notify(signalChan, os.Interrupt)

		<-signalChan

		newCluster.Stop()

	},
}
