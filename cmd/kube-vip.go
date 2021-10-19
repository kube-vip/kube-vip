package cmd

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/manager"
	"github.com/kube-vip/kube-vip/pkg/packet"
	"github.com/kube-vip/kube-vip/pkg/vip"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

// Path to the configuration file
var configPath string

// Path to the configuration file
var namespace string

// Disable the Virtual IP (bind to the existing network stack)
var disableVIP bool

// Disable the Virtual IP (bind to the existing network stack)
var controlPlane bool

// Run as a load balancer service (within a pod / kubernetes)
var serviceArp bool

// ConfigMap name within a Kubernetes cluster
var configMap string

// Configure the level of loggin
var logLevel uint32

// Provider Config
var providerConfig string

// Release - this struct contains the release information populated when building kube-vip
var Release struct {
	Version string
	Build   string
}

// Structs used via the various subcommands
var initConfig kubevip.Config
var initLoadBalancer kubevip.LoadBalancer

// Points to a kubernetes configuration file
var kubeConfigPath string

var kubeVipCmd = &cobra.Command{
	Use:   "kube-vip",
	Short: "This is a server for providing a Virtual IP and load-balancer for the Kubernetes control-plane",
}

func init() {

	localpeer, err := autoGenLocalPeer()
	if err != nil {
		log.Fatalln(err)
	}
	initConfig.LocalPeer = *localpeer
	//initConfig.Peers = append(initConfig.Peers, *localpeer)
	kubeVipCmd.PersistentFlags().StringVar(&initConfig.Interface, "interface", "", "Name of the interface to bind to")
	kubeVipCmd.PersistentFlags().BoolVar(&initConfig.AutoInterface, "autoInterface", false, "Name of the interface to bind to")

	kubeVipCmd.PersistentFlags().StringVar(&initConfig.ServicesInterface, "serviceInterface", "", "Name of the interface to bind to (for services)")

	kubeVipCmd.PersistentFlags().StringVar(&initConfig.VIP, "vip", "", "The Virtual IP address")
	kubeVipCmd.PersistentFlags().StringVar(&initConfig.Address, "address", "", "an address (IP or DNS name) to use as a VIP")
	kubeVipCmd.PersistentFlags().IntVar(&initConfig.Port, "port", 6443, "listen port for the VIP")
	kubeVipCmd.PersistentFlags().StringVar(&initConfig.VIPCIDR, "cidr", "32", "The CIDR range for the virtual IP address")
	kubeVipCmd.PersistentFlags().BoolVar(&initConfig.EnableARP, "arp", false, "Enable Arp for Vip changes")
	kubeVipCmd.PersistentFlags().StringVar(&initConfig.Annotations, "annotations", "", "Set Node annotations prefix for parsing")
	kubeVipCmd.PersistentFlags().BoolVar(&initConfig.DDNS, "ddns", false, "use Dynamic DNS + DHCP to allocate VIP for address")

	// Clustering type (leaderElection)
	kubeVipCmd.PersistentFlags().BoolVar(&initConfig.EnableLeaderElection, "leaderElection", false, "Use the Kubernetes leader election mechanism for clustering")
	kubeVipCmd.PersistentFlags().IntVar(&initConfig.LeaseDuration, "leaseDuration", 5, "Length of time a Kubernetes leader lease can be held for")
	kubeVipCmd.PersistentFlags().IntVar(&initConfig.RenewDeadline, "leaseRenewDuration", 3, "Length of time a Kubernetes leader can attempt to renew its lease")
	kubeVipCmd.PersistentFlags().IntVar(&initConfig.RetryPeriod, "leaseRetry", 1, "Number of times the host will retry to hold a lease")

	// Clustering type (raft)
	kubeVipCmd.PersistentFlags().BoolVar(&initConfig.StartAsLeader, "startAsLeader", false, "Start this instance as the cluster leader")
	kubeVipCmd.PersistentFlags().BoolVar(&initConfig.AddPeersAsBackends, "addPeersToLB", true, "Add raft peers to the load-balancer")

	// Packet flags
	kubeVipCmd.PersistentFlags().BoolVar(&initConfig.EnableMetal, "metal", false, "This will use the Equinix Metal API (requires the token ENV) to update the EIP <-> VIP")
	kubeVipCmd.PersistentFlags().StringVar(&initConfig.MetalAPIKey, "metalKey", "", "The API token for authenticating with the Equinix Metal API")
	kubeVipCmd.PersistentFlags().StringVar(&initConfig.MetalProject, "metalProject", "", "The name of project already created within Equinix Metal")
	kubeVipCmd.PersistentFlags().StringVar(&initConfig.MetalProjectID, "metalProjectID", "", "The ID of project already created within Equinix Metal")
	kubeVipCmd.PersistentFlags().StringVar(&initConfig.ProviderConfig, "provider-config", "", "The path to a provider configuration")

	// Load Balancer flags
	kubeVipCmd.PersistentFlags().BoolVar(&initConfig.EnableLoadBalancer, "lbEnable", false, "Enable a load-balancer on the VIP")
	kubeVipCmd.PersistentFlags().BoolVar(&initLoadBalancer.BindToVip, "lbBindToVip", true, "Bind example load balancer to VIP")
	kubeVipCmd.PersistentFlags().StringVar(&initLoadBalancer.Type, "lbType", "tcp", "Type of load balancer instance (TCP/HTTP)")
	kubeVipCmd.PersistentFlags().StringVar(&initLoadBalancer.Name, "lbName", "Kubeadm Load Balancer", "The name of a load balancer instance")
	kubeVipCmd.PersistentFlags().IntVar(&initLoadBalancer.Port, "lbPort", 6443, "Port that load balancer will expose on")
	kubeVipCmd.PersistentFlags().IntVar(&initLoadBalancer.BackendPort, "lbBackEndPort", 6444, "A port that all backends may be using (optional)")

	// BGP flags
	kubeVipCmd.PersistentFlags().BoolVar(&initConfig.EnableBGP, "bgp", false, "This will enable BGP support within kube-vip")
	kubeVipCmd.PersistentFlags().StringVar(&initConfig.BGPConfig.RouterID, "bgpRouterID", "", "The routerID for the bgp server")
	kubeVipCmd.PersistentFlags().StringVar(&initConfig.BGPConfig.SourceIF, "sourceIF", "", "The source interface for bgp peering (not to be used with sourceIP)")
	kubeVipCmd.PersistentFlags().StringVar(&initConfig.BGPConfig.SourceIP, "sourceIP", "", "The source address for bgp peering (not to be used with sourceIF)")
	kubeVipCmd.PersistentFlags().Uint32Var(&initConfig.BGPConfig.AS, "localAS", 65000, "The local AS number for the bgp server")
	kubeVipCmd.PersistentFlags().StringVar(&initConfig.BGPPeerConfig.Address, "peerAddress", "", "The address of a BGP peer")
	kubeVipCmd.PersistentFlags().Uint32Var(&initConfig.BGPPeerConfig.AS, "peerAS", 65000, "The AS number for a BGP peer")
	kubeVipCmd.PersistentFlags().StringVar(&initConfig.BGPPeerConfig.Password, "peerPass", "", "The md5 password for a BGP peer")
	kubeVipCmd.PersistentFlags().BoolVar(&initConfig.BGPPeerConfig.MultiHop, "multihop", false, "This will enable BGP multihop support")
	kubeVipCmd.PersistentFlags().StringSliceVar(&initConfig.BGPPeers, "bgppeers", []string{}, "Comma seperated BGP Peer, format: address:as:password:multihop")

	// Control plane specific flags
	kubeVipCmd.PersistentFlags().StringVarP(&initConfig.Namespace, "namespace", "n", "kube-system", "The configuration map defined within the cluster")

	// Manage logging
	kubeVipCmd.PersistentFlags().Uint32Var(&logLevel, "log", 4, "Set the level of logging")

	// Service flags
	kubeVipService.Flags().StringVarP(&configMap, "configMap", "c", "plndr", "The configuration map defined within the cluster")

	kubeVipCmd.PersistentFlags().BoolVar(&initConfig.EnableControlPane, "controlplane", false, "Enable HA for control plane, hybrid mode")
	kubeVipCmd.PersistentFlags().BoolVar(&initConfig.EnableServices, "services", false, "Enable Kubernetes services, hybrid mode")

	// Prometheus HTTP Server
	kubeVipCmd.PersistentFlags().StringVar(&initConfig.PrometheusHTTPServer, "promethuesHTTPServer", ":2112", "Host and port used to expose Promethues metrics via an HTTP server")

	kubeVipCmd.AddCommand(kubeKubeadm)
	kubeVipCmd.AddCommand(kubeManifest)
	kubeVipCmd.AddCommand(kubeVipManager)
	kubeVipCmd.AddCommand(kubeVipSample)
	kubeVipCmd.AddCommand(kubeVipService)
	kubeVipCmd.AddCommand(kubeVipStart)
	kubeVipCmd.AddCommand(kubeVipVersion)

	// Sample commands
	kubeVipSample.AddCommand(kubeVipSampleConfig)
	kubeVipSample.AddCommand(kubeVipSampleManifest)

}

// Execute - starts the command parsing process
func Execute() {
	if err := kubeVipCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

var kubeVipVersion = &cobra.Command{
	Use:   "version",
	Short: "Version and Release information about the Kubernetes Virtual IP Server",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("Kube-VIP Release Information\n")
		fmt.Printf("Version:  %s\n", Release.Version)
		fmt.Printf("Build:    %s\n", Release.Build)
	},
}

var kubeVipSample = &cobra.Command{
	Use:   "sample",
	Short: "Generate a Sample configuration",
	Run: func(cmd *cobra.Command, args []string) {
		cmd.Help()
	},
}

var kubeVipService = &cobra.Command{
	Use:   "service",
	Short: "Start the Virtual IP / Load balancer as a service within a Kubernetes cluster",
	Run: func(cmd *cobra.Command, args []string) {
		// Set the logging level for all subsequent functions
		log.SetLevel(log.Level(logLevel))

		// parse environment variables, these will overwrite anything loaded or flags
		err := kubevip.ParseEnvironment(&initConfig)
		if err != nil {
			log.Fatalln(err)
		}

		// User Environment variables as an option to make manifest clearer
		envConfigMap := os.Getenv("vip_configmap")
		if envConfigMap != "" {
			configMap = envConfigMap
		}

		// Define the new service manager
		mgr, err := manager.New(configMap, &initConfig)
		if err != nil {
			log.Fatalf("%v", err)
		}

		// Start the service manager, this will watch the config Map and construct kube-vip services for it
		err = mgr.Start()
		if err != nil {
			log.Fatalf("%v", err)
		}
	},
}

var kubeVipManager = &cobra.Command{
	Use:   "manager",
	Short: "Start the kube-vip manager",
	Run: func(cmd *cobra.Command, args []string) {
		// Set the logging level for all subsequent functions
		log.SetLevel(log.Level(logLevel))

		if initConfig.AutoInterface {
			defaultIF, err := vip.GetDefaultGatewayInterface()
			if err != nil {
				log.Fatalf("unable to set default interface -> [%v]", err)
			}
			initConfig.Interface = defaultIF.Name
		}

		if initConfig.Interface == "" {
			cmd.Help()
			log.Fatalln("No interface is specified for kube-vip to bind to")
		}

		go servePrometheusHTTPServer(cmd.Context(), PrometheusHTTPServerConfig{
			Addr: initConfig.PrometheusHTTPServer,
		})

		// parse environment variables, these will overwrite anything loaded or flags
		err := kubevip.ParseEnvironment(&initConfig)
		if err != nil {
			log.Fatalln(err)
		}

		// User Environment variables as an option to make manifest clearer
		envConfigMap := os.Getenv("vip_configmap")
		if envConfigMap != "" {
			configMap = envConfigMap
		}

		// If Packet is enabled and there is a provider configuration passed
		if initConfig.EnableMetal {
			if providerConfig != "" {
				providerAPI, providerProject, err := packet.GetPacketConfig(providerConfig)
				if err != nil {
					log.Fatalf("%v", err)
				}
				initConfig.MetalAPIKey = providerAPI
				initConfig.MetalProject = providerProject
			}
		}

		// Define the new service manager
		mgr, err := manager.New(configMap, &initConfig)
		if err != nil {
			log.Fatalf("configuring new Manager error -> %v", err)
		}

		prometheus.MustRegister(mgr.PrometheusCollector()...)

		// Start the service manager, this will watch the config Map and construct kube-vip services for it
		err = mgr.Start()
		if err != nil {
			log.Fatalf("starting new Manager error -> %v", err)
		}
	},
}

type PrometheusHTTPServerConfig struct {
	// Addr sets the http server address used to expose the metric endpoint
	Addr string
}

func servePrometheusHTTPServer(ctx context.Context, config PrometheusHTTPServerConfig) {
	var err error
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())

	srv := &http.Server{
		Addr:    config.Addr,
		Handler: mux,
	}

	go func() {
		if err = srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen:%+s\n", err)
		}
	}()

	log.Printf("server started")

	<-ctx.Done()

	log.Printf("server stopped")

	ctxShutDown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer func() {
		cancel()
	}()

	if err = srv.Shutdown(ctxShutDown); err != nil {
		log.Fatalf("server Shutdown Failed:%+s", err)
	}

	if err == http.ErrServerClosed {
		err = nil
	}

}
