package cmd

import (
	"fmt"

	"github.com/plunder-app/kube-vip/pkg/kubevip"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

// manifests will eventually deprecate the kubeadm set of subcommands
// manifests will be used to generate:
// - Pod spec manifest, mainly used for a static pod (kubeadm)
// - Daemonset manifest, mainly used to run kube-vip as a deamonset within Kubernetes (k3s/rke)

// var initConfig kubevip.Config
// var initLoadBalancer kubevip.LoadBalancer

// // Points to a kubernetes configuration file
// var kubeConfigPath string

func init() {

	// localpeer, err := autoGenLocalPeer()
	// if err != nil {
	// 	log.Fatalln(err)
	// }
	// initConfig.LocalPeer = *localpeer
	// //initConfig.Peers = append(initConfig.Peers, *localpeer)
	// kubeKubeadm.PersistentFlags().StringVar(&initConfig.Interface, "interface", "", "Name of the interface to bind to")
	// kubeKubeadm.PersistentFlags().StringVar(&initConfig.VIP, "vip", "", "The Virtual IP address")
	// kubeKubeadm.PersistentFlags().StringVar(&initConfig.VIPCIDR, "cidr", "", "The CIDR range for the virtual IP address")
	// kubeKubeadm.PersistentFlags().BoolVar(&initConfig.GratuitousARP, "arp", true, "Enable Arp for Vip changes")

	// // Clustering type (leaderElection)
	// kubeKubeadm.PersistentFlags().BoolVar(&initConfig.EnableLeaderElection, "leaderElection", false, "Use the Kubernetes leader election mechanism for clustering")
	// kubeKubeadm.PersistentFlags().IntVar(&initConfig.LeaseDuration, "leaseDuration", 5, "Length of time a Kubernetes leader lease can be held for")
	// kubeKubeadm.PersistentFlags().IntVar(&initConfig.RenewDeadline, "leaseRenewDuration", 3, "Length of time a Kubernetes leader can attempt to renew its lease")
	// kubeKubeadm.PersistentFlags().IntVar(&initConfig.RetryPeriod, "leaseRetry", 1, "Number of times the host will retry to hold a lease")

	// // Clustering type (raft)
	// kubeKubeadm.PersistentFlags().BoolVar(&initConfig.StartAsLeader, "startAsLeader", false, "Start this instance as the cluster leader")
	// kubeKubeadm.PersistentFlags().BoolVar(&initConfig.AddPeersAsBackends, "addPeersToLB", true, "Add raft peers to the load-balancer")

	// // Packet flags
	// kubeKubeadm.PersistentFlags().BoolVar(&initConfig.EnablePacket, "packet", false, "This will use the Packet API (requires the token ENV) to update the EIP <-> VIP")
	// kubeKubeadm.PersistentFlags().StringVar(&initConfig.PacketAPIKey, "packetKey", "", "The API token for authenticating with the Packet API")
	// kubeKubeadm.PersistentFlags().StringVar(&initConfig.PacketProject, "packetProject", "", "The name of project already created within Packet")

	// // Load Balancer flags
	// kubeKubeadm.PersistentFlags().BoolVar(&initConfig.EnableLoadBalancer, "lbEnable", false, "Enable a load-balancer on the VIP")
	// kubeKubeadm.PersistentFlags().BoolVar(&initLoadBalancer.BindToVip, "lbBindToVip", true, "Bind example load balancer to VIP")
	// kubeKubeadm.PersistentFlags().StringVar(&initLoadBalancer.Type, "lbType", "tcp", "Type of load balancer instance (TCP/HTTP)")
	// kubeKubeadm.PersistentFlags().StringVar(&initLoadBalancer.Name, "lbName", "Kubeadm Load Balancer", "The name of a load balancer instance")
	// kubeKubeadm.PersistentFlags().IntVar(&initLoadBalancer.Port, "lbPort", 6443, "Port that load balancer will expose on")
	// kubeKubeadm.PersistentFlags().IntVar(&initLoadBalancer.BackendPort, "lbBackEndPort", 6444, "A port that all backends may be using (optional)")

	// // BGP flags
	// kubeKubeadm.PersistentFlags().BoolVar(&initConfig.EnableBGP, "bgp", false, "This will enable BGP support within kube-vip")
	// kubeKubeadm.PersistentFlags().StringVar(&initConfig.BGPConfig.RouterID, "bgpRouterID", "", "The routerID for the bgp server")
	// kubeKubeadm.PersistentFlags().Uint32Var(&initConfig.BGPConfig.AS, "localAS", 65000, "The local AS number for the bgp server")
	// kubeKubeadm.PersistentFlags().StringVar(&initConfig.BGPPeerConfig.Address, "peerAddress", "", "The address of a BGP peer")
	// kubeKubeadm.PersistentFlags().Uint32Var(&initConfig.BGPPeerConfig.AS, "peerAS", 65000, "The AS number for a BGP peer")

	// kubeKubeadmJoin.Flags().StringVar(&kubeConfigPath, "config", "/etc/kubernetes/admin.conf", "The path of a Kubernetes configuration file")

	kubeManifest.AddCommand(kubeManifestPod)
	kubeManifest.AddCommand(kubeManifestDaemon)
}

var kubeManifest = &cobra.Command{
	Use:   "manifest",
	Short: "Manifest functions",
	Run: func(cmd *cobra.Command, args []string) {
		cmd.Help()
		// TODO - A load of text detailing what's actually happening
	},
}

var kubeManifestPod = &cobra.Command{
	Use:   "pod",
	Short: "Generate a Pod Manifest",
	Run: func(cmd *cobra.Command, args []string) {
		// Set the logging level for all subsequent functions
		log.SetLevel(log.Level(logLevel))
		initConfig.LoadBalancers = append(initConfig.LoadBalancers, initLoadBalancer)
		// TODO - A load of text detailing what's actually happening
		kubevip.ParseEnvironment(&initConfig)
		// TODO - check for certain things VIP/interfaces
		if initConfig.Interface == "" {
			cmd.Help()
			log.Fatalln("No interface is specified for kube-vip to bind to")
		}

		if initConfig.VIP == "" {
			cmd.Help()
			log.Fatalln("No address is specified for kube-vip to expose services on")
		}
		cfg := kubevip.GeneratePodManifestFromConfig(&initConfig, Release.Version)

		fmt.Println(cfg)
	},
}

var kubeManifestDaemon = &cobra.Command{
	Use:   "daemonset",
	Short: "Generate a Daemonset Manifest",
	Run: func(cmd *cobra.Command, args []string) {
		// Set the logging level for all subsequent functions
		log.SetLevel(log.Level(logLevel))
		initConfig.LoadBalancers = append(initConfig.LoadBalancers, initLoadBalancer)
		// TODO - A load of text detailing what's actually happening
		kubevip.ParseEnvironment(&initConfig)
		// TODO - check for certain things VIP/interfaces
		if initConfig.Interface == "" {
			cmd.Help()
			log.Fatalln("No interface is specified for kube-vip to bind to")
		}

		if initConfig.VIP == "" {
			cmd.Help()
			log.Fatalln("No address is specified for kube-vip to expose services on")
		}
		cfg := kubevip.GenerateDeamonsetManifestFromConfig(&initConfig, Release.Version)

		fmt.Println(cfg)
	},
}
