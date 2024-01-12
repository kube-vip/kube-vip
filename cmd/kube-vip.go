package cmd

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/vishvananda/netlink"

	"github.com/kube-vip/kube-vip/pkg/equinixmetal"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/manager"
	"github.com/kube-vip/kube-vip/pkg/vip"
)

// Path to the configuration file
var configPath string

// Path to the configuration file
// var namespace string

// Disable the Virtual IP (bind to the existing network stack)
var disableVIP bool

// Disable the Virtual IP (bind to the existing network stack)
// var controlPlane bool

// Run as a load balancer service (within a pod / kubernetes)
// var serviceArp bool

// ConfigMap name within a Kubernetes cluster
var configMap string

// Configure the level of logging
var logLevel uint32

// Provider Config
var providerConfig string

// Release - this struct contains the release information populated when building kube-vip
var Release struct {
	Version string
	Build   string
}

// Structs used via the various subcommands
var (
	initConfig       kubevip.Config
	initLoadBalancer kubevip.LoadBalancer
)

// Points to a kubernetes configuration file
var kubeConfigPath string

var kubeVipCmd = &cobra.Command{
	Use:   "kube-vip",
	Short: "This is a server for providing a Virtual IP and load-balancer for the Kubernetes control-plane",
}

func init() {
	// Basic flags
	kubeVipCmd.PersistentFlags().StringVar(&initConfig.Interface, "interface", "", "Name of the interface to bind to")
	kubeVipCmd.PersistentFlags().StringVar(&initConfig.ServicesInterface, "serviceInterface", "", "Name of the interface to bind to (for services)")
	kubeVipCmd.PersistentFlags().StringVar(&initConfig.VIP, "vip", "", "The Virtual IP address")
	kubeVipCmd.PersistentFlags().StringVar(&initConfig.VIPSubnet, "vipSubnet", "", "The Virtual IP address subnet e.g. /32 /24 /8 etc..")

	kubeVipCmd.PersistentFlags().StringVar(&initConfig.VIPCIDR, "cidr", "32", "The CIDR range for the virtual IP address") // todo: deprecate

	kubeVipCmd.PersistentFlags().StringVar(&initConfig.Address, "address", "", "an address (IP or DNS name) to use as a VIP")
	kubeVipCmd.PersistentFlags().IntVar(&initConfig.Port, "port", 6443, "Port for the VIP")
	kubeVipCmd.PersistentFlags().BoolVar(&initConfig.EnableARP, "arp", false, "Enable Arp for VIP changes")
	kubeVipCmd.PersistentFlags().BoolVar(&initConfig.EnableWireguard, "wireguard", false, "Enable Wireguard for services VIPs")
	kubeVipCmd.PersistentFlags().BoolVar(&initConfig.EnableRoutingTable, "table", false, "Enable Routing Table for services VIPs")

	// LoadBalancer flags
	kubeVipCmd.PersistentFlags().BoolVar(&initConfig.EnableLoadBalancer, "enableLoadBalancer", false, "enable loadbalancing on the VIP with IPVS")
	kubeVipCmd.PersistentFlags().IntVar(&initConfig.LoadBalancerPort, "lbPort", 6443, "loadbalancer port for the VIP")
	kubeVipCmd.PersistentFlags().StringVar(&initConfig.LoadBalancerForwardingMethod, "lbForwardingMethod", "local", "loadbalancer forwarding method")
	kubeVipCmd.PersistentFlags().BoolVar(&initConfig.DDNS, "ddns", false, "use Dynamic DNS + DHCP to allocate VIP for address")

	// Clustering type (leaderElection)
	kubeVipCmd.PersistentFlags().BoolVar(&initConfig.EnableLeaderElection, "leaderElection", false, "Use the Kubernetes leader election mechanism for clustering")
	kubeVipCmd.PersistentFlags().StringVar(&initConfig.LeaderElectionType, "leaderElectionType", "kubernetes", "Defines the backend to run the leader election: kubernetes or etcd. Defaults to kubernetes.")
	kubeVipCmd.PersistentFlags().StringVar(&initConfig.LeaseName, "leaseName", "plndr-cp-lock", "Name of the lease that is used for leader election")
	kubeVipCmd.PersistentFlags().IntVar(&initConfig.LeaseDuration, "leaseDuration", 5, "Length of time a Kubernetes leader lease can be held for")
	kubeVipCmd.PersistentFlags().IntVar(&initConfig.RenewDeadline, "leaseRenewDuration", 3, "Length of time a Kubernetes leader can attempt to renew its lease")
	kubeVipCmd.PersistentFlags().IntVar(&initConfig.RetryPeriod, "leaseRetry", 1, "Number of times the host will retry to hold a lease")

	// Equinix Metal flags
	kubeVipCmd.PersistentFlags().BoolVar(&initConfig.EnableMetal, "metal", false, "This will use the Equinix Metal API (requires the token ENV) to update the EIP <-> VIP")
	kubeVipCmd.PersistentFlags().StringVar(&initConfig.MetalAPIKey, "metalKey", "", "The API token for authenticating with the Equinix Metal API")
	kubeVipCmd.PersistentFlags().StringVar(&initConfig.MetalProject, "metalProject", "", "The name of project already created within Equinix Metal")
	kubeVipCmd.PersistentFlags().StringVar(&initConfig.MetalProjectID, "metalProjectID", "", "The ID of project already created within Equinix Metal")
	kubeVipCmd.PersistentFlags().StringVar(&initConfig.ProviderConfig, "provider-config", "", "The path to a provider configuration")

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
	kubeVipCmd.PersistentFlags().StringSliceVar(&initConfig.BGPPeers, "bgppeers", []string{}, "Comma separated BGP Peer, format: address:as:password:multihop")
	kubeVipCmd.PersistentFlags().StringVar(&initConfig.Annotations, "annotations", "", "Set Node annotations prefix for parsing")

	// Namespace for kube-vip
	kubeVipCmd.PersistentFlags().StringVarP(&initConfig.Namespace, "namespace", "n", "kube-system", "The namespace for the configmap defined within the cluster")

	// Manage logging
	kubeVipCmd.PersistentFlags().Uint32Var(&logLevel, "log", 4, "Set the level of logging")

	// Service flags
	kubeVipService.Flags().StringVarP(&configMap, "configMap", "c", "plndr", "The configuration map defined within the cluster")

	// Routing Table flags
	kubeVipCmd.PersistentFlags().IntVar(&initConfig.RoutingTableID, "tableID", 198, "The routing table used for all table entries")
	kubeVipCmd.PersistentFlags().IntVar(&initConfig.RoutingTableType, "tableType", 0, "The type of route that will be added to the routing table")

	// Behaviour flags
	kubeVipCmd.PersistentFlags().BoolVar(&initConfig.EnableControlPlane, "controlplane", false, "Enable HA for control plane")
	kubeVipCmd.PersistentFlags().BoolVar(&initConfig.DetectControlPlane, "autodetectcp", false, "Determine working address for control plane (from loopback)")
	kubeVipCmd.PersistentFlags().BoolVar(&initConfig.EnableServices, "services", false, "Enable Kubernetes services")

	// Extended behaviour flags
	kubeVipCmd.PersistentFlags().BoolVar(&initConfig.EnableServicesElection, "servicesElection", false, "Enable leader election per kubernetes service")
	kubeVipCmd.PersistentFlags().BoolVar(&initConfig.LoadBalancerClassOnly, "lbClassOnly", false, "Enable load balancing only for services with LoadBalancerClass \"kube-vip.io/kube-vip-class\"")
	kubeVipCmd.PersistentFlags().StringVar(&initConfig.LoadBalancerClassName, "lbClassName", "kube-vip.io/kube-vip-class", "Name of load balancer class for kube-VIP, defaults to \"kube-vip.io/kube-vip-class\"")
	kubeVipCmd.PersistentFlags().BoolVar(&initConfig.EnableServiceSecurity, "onlyAllowTrafficServicePorts", false, "Only allow traffic to service ports, others will be dropped, defaults to false")
	kubeVipCmd.PersistentFlags().BoolVar(&initConfig.EnableNodeLabeling, "enableNodeLabeling", false, "Enable leader node labeling with \"kube-vip.io/has-ip=<VIP address>\", defaults to false")
	kubeVipCmd.PersistentFlags().StringVar(&initConfig.ServicesLeaseName, "servicesLeaseName", "plndr-svcs-lock", "Name of the lease that is used for leader election for services (in arp mode)")
	kubeVipCmd.PersistentFlags().BoolVar(&initConfig.EnableEndpointSlices, "enableEndpointSlices", false, "If enabled, kube-vip will only advertise services, but will use EndpointSlices instead of endpoints to get IPs of Pods")

	// Prometheus HTTP Server
	kubeVipCmd.PersistentFlags().StringVar(&initConfig.PrometheusHTTPServer, "prometheusHTTPServer", ":2112", "Host and port used to expose Prometheus metrics via an HTTP server")

	// Etcd
	kubeVipCmd.PersistentFlags().StringVar(&initConfig.Etcd.CAFile, "etcdCACert", "", "Verify certificates of TLS-enabled secure servers using this CA bundle file")
	kubeVipCmd.PersistentFlags().StringVar(&initConfig.Etcd.ClientCertFile, "etcdCert", "", "Identify secure client using this TLS certificate file")
	kubeVipCmd.PersistentFlags().StringVar(&initConfig.Etcd.ClientKeyFile, "etcdKey", "", "Identify secure client using this TLS key file")
	kubeVipCmd.PersistentFlags().StringSliceVar(&initConfig.Etcd.Endpoints, "etcdEndpoints", nil, "Etcd member endpoints")

	// Kubernetes client specific flags

	kubeVipCmd.PersistentFlags().StringVar(&initConfig.K8sConfigFile, "k8sConfigPath", "/etc/kubernetes/admin.conf", "Path to the configuration file used with the Kubernetes client")

	kubeVipCmd.AddCommand(kubeKubeadm)
	kubeVipCmd.AddCommand(kubeManifest)
	kubeVipCmd.AddCommand(kubeVipManager)
	kubeVipCmd.AddCommand(kubeVipSample)
	kubeVipCmd.AddCommand(kubeVipService)
	kubeVipCmd.AddCommand(kubeVipStart)
	kubeVipCmd.AddCommand(kubeVipVersion)
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
		_ = cmd.Help()
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

		if err := initConfig.CheckInterface(); err != nil {
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
		// parse environment variables, these will overwrite anything loaded or flags
		err := kubevip.ParseEnvironment(&initConfig)
		if err != nil {
			log.Fatalln(err)
		}

		// Set the logging level for all subsequent functions
		log.SetLevel(log.Level(initConfig.Logging))

		// Welome messages
		log.Infof("Starting kube-vip.io [%s]", Release.Version)
		log.Debugf("Build kube-vip.io [%s]", Release.Build)

		// start prometheus server
		if initConfig.PrometheusHTTPServer != "" {
			go servePrometheusHTTPServer(cmd.Context(), PrometheusHTTPServerConfig{
				Addr: initConfig.PrometheusHTTPServer,
			})
		}

		// Determine the kube-vip mode
		var mode string
		if initConfig.EnableARP {
			mode = "ARP"
		}

		if initConfig.EnableBGP {
			mode = "BGP"
		}

		if initConfig.EnableWireguard {
			mode = "Wireguard"
		}

		if initConfig.EnableRoutingTable {
			mode = "Routing Table"
		}

		// Provide configuration to output/logging
		log.Infof("namespace [%s], Mode: [%s], Features(s): Control Plane:[%t], Services:[%t]", initConfig.Namespace, mode, initConfig.EnableControlPlane, initConfig.EnableServices)

		// End if nothing is enabled
		if !initConfig.EnableServices && !initConfig.EnableControlPlane {
			log.Fatalln("no features are enabled")
		}

		// If we're using wireguard then all traffic goes through the wg0 interface
		if initConfig.EnableWireguard {
			if initConfig.Interface == "" {
				// Set the vip interface to the wireguard interface
				initConfig.Interface = "wg0"
			}

			log.Infof("configuring Wireguard networking")
			l, err := netlink.LinkByName(initConfig.Interface)
			if err != nil {
				if strings.Contains(err.Error(), "Link not found") {
					log.Warnf("interface \"%s\" doesn't exist, attempting to create wireguard interface", initConfig.Interface)
					err = netlink.LinkAdd(&netlink.Wireguard{LinkAttrs: netlink.LinkAttrs{Name: initConfig.Interface}})
					if err != nil {
						log.Fatalln(err)
					}
					l, err = netlink.LinkByName(initConfig.Interface)
					if err != nil {
						log.Fatalln(err)
					}
				}
			}
			err = netlink.LinkSetUp(l)
			if err != nil {
				log.Fatalln(err)
			}

		} else { // if we're not using Wireguard then we'll need to use an actual interface
			// Check if the interface needs auto-detecting
			if initConfig.Interface == "" {
				log.Infof("No interface is specified for VIP in config, auto-detecting default Interface")
				defaultIF, err := vip.GetDefaultGatewayInterface()
				if err != nil {
					_ = cmd.Help()
					log.Fatalf("unable to detect default interface -> [%v]", err)
				}
				initConfig.Interface = defaultIF.Name
				log.Infof("kube-vip will bind to interface [%s]", initConfig.Interface)

				go func() {
					if err := vip.MonitorDefaultInterface(context.TODO(), defaultIF); err != nil {
						log.Fatalf("crash: %s", err.Error())
					}
				}()
			}
		}
		// Perform a check on th state of the interface
		if err := initConfig.CheckInterface(); err != nil {
			log.Fatalln(err)
		}

		// User Environment variables as an option to make manifest clearer
		envConfigMap := os.Getenv("vip_configmap")
		if envConfigMap != "" {
			configMap = envConfigMap
		}

		// If Equinix Metal is enabled and there is a provider configuration passed
		if initConfig.EnableMetal {
			if providerConfig != "" {
				providerAPI, providerProject, err := equinixmetal.GetPacketConfig(providerConfig)
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

// PrometheusHTTPServerConfig defines the Prometheus server configuration.
type PrometheusHTTPServerConfig struct {
	// Addr sets the http server address used to expose the metric endpoint
	Addr string
}

func servePrometheusHTTPServer(ctx context.Context, config PrometheusHTTPServerConfig) {
	var err error
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html>
			<head><title>kube-vip</title></head>
			<body>
			<h1>kube-vip Metrics</h1>
			<p><a href="` + "/metrics" + `">Metrics</a></p>
			</body>
			</html>`))
	})

	srv := &http.Server{
		Addr:              config.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 2 * time.Second,
	}

	go func() {
		if err = srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen:%+s\n", err)
		}
	}()

	log.Printf("prometheus HTTP server started")

	<-ctx.Done()

	log.Printf("prometheus HTTP server stopped")

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
