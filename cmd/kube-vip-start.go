package cmd

import (
	"github.com/kube-vip/kube-vip/pkg/bgp"
	"github.com/kube-vip/kube-vip/pkg/cluster"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

// Start as a single node (no cluster), start as a leader in the cluster
var startConfig kubevip.Config
var startConfigLB kubevip.LoadBalancer
var startLocalPeer, startKubeConfigPath string
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
	kubeVipStart.Flags().BoolVar(&startConfig.DDNS, "ddns", false, "use Dynamic DNS + DHCP to allocate VIP for address")
	kubeVipStart.Flags().BoolVar(&startConfig.SingleNode, "singleNode", false, "Start this instance as a single node")
	kubeVipStart.Flags().BoolVar(&startConfig.StartAsLeader, "startAsLeader", false, "Start this instance as the cluster leader")
	kubeVipStart.Flags().BoolVar(&startConfig.EnableARP, "arp", false, "Use ARP broadcasts to improve VIP re-allocations")
	kubeVipStart.Flags().StringVar(&startLocalPeer, "localPeer", "server1:192.168.0.1:10000", "Settings for this peer, format: id:address:port")

	// Load Balancer flags
	kubeVipStart.Flags().BoolVar(&startConfigLB.BindToVip, "lbBindToVip", false, "Bind example load balancer to VIP")
	kubeVipStart.Flags().StringVar(&startConfigLB.Type, "lbType", "tcp", "Type of load balancer instance (TCP/HTTP)")
	kubeVipStart.Flags().StringVar(&startConfigLB.Name, "lbName", "Example Load Balancer", "The name of a load balancer instance")
	kubeVipStart.Flags().IntVar(&startConfigLB.Port, "lbPort", 8080, "Port that load balancer will expose on")
	kubeVipStart.Flags().StringVar(&startConfigLB.ForwardingMethod, "lbForwardingMethod", "local", "The forwarding method of a load balancer instance")

	// Cluster configuration
	kubeVipStart.Flags().StringVar(&startKubeConfigPath, "kubeConfig", "/etc/kubernetes/admin.conf", "The path of a kubernetes configuration file")
	kubeVipStart.Flags().BoolVar(&inCluster, "inCluster", false, "Use the incluster token to authenticate to Kubernetes")
	kubeVipStart.Flags().BoolVar(&startConfig.EnableLeaderElection, "leaderElection", false, "Use the Kubernetes leader election mechanism for clustering")

	// This sets the namespace that the lock should exist in
	kubeVipStart.Flags().StringVarP(&startConfig.Namespace, "namespace", "n", "kube-system", "The configuration map defined within the cluster")
}

var kubeVipStart = &cobra.Command{
	Use:   "start",
	Short: "Start the Virtual IP / Load balancer",
	Run: func(cmd *cobra.Command, args []string) {
		// Set the logging level for all subsequent functions
		log.SetLevel(log.Level(logLevel))
		var err error

		// If a configuration file is loaded, then it will overwrite flags

		// parse environment variables, these will overwrite anything loaded or flags
		err = kubevip.ParseEnvironment(&startConfig)
		if err != nil {
			log.Fatalln(err)
		}

		if startConfig.LeaderElectionType == "etcd" {
			log.Fatalln("Leader election with etcd not supported in start command, use manager")
		}

		newCluster, err := cluster.InitCluster(&startConfig, disableVIP)
		if err != nil {
			log.Fatalf("%v", err)
		}
		var bgpServer *bgp.Server
		if startConfig.SingleNode {
			// If the Virtual IP isn't disabled then create the netlink configuration
			// Start a single node cluster
			if err := newCluster.StartSingleNode(&startConfig, disableVIP); err != nil {
				log.Errorf("error starting single node: %v", err)
			}
		} else {
			if disableVIP {
				log.Fatalln("Cluster mode requires the Virtual IP to be enabled, use single node with no VIP")
			}

			if startConfig.EnableLeaderElection {
				cm, err := cluster.NewManager(startKubeConfigPath, inCluster, startConfig.Port)
				if err != nil {
					log.Fatalf("%v", err)
				}

				if startConfig.EnableBGP {
					log.Info("Starting the BGP server to advertise VIP routes to VGP peers")
					bgpServer, err = bgp.NewBGPServer(&startConfig.BGPConfig, nil)
					if err != nil {
						log.Fatalf("%v", err)
					}

					// Defer a function to check if the bgpServer has been created and if so attempt to close it
					defer func() {
						if bgpServer != nil {
							bgpServer.Close()
						}
					}()
				}

				// Leader Cluster will block
				err = newCluster.StartCluster(&startConfig, cm, bgpServer)
				if err != nil {
					log.Fatalf("%v", err)
				}
			}
		}
	},
}
