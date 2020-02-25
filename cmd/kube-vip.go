package cmd

import (
	"fmt"
	"os"
	"os/signal"

	"github.com/ghodss/yaml"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/thebsdbox/kube-vip/pkg/cluster"
	"github.com/thebsdbox/kube-vip/pkg/kubevip"
	"github.com/thebsdbox/kube-vip/pkg/service"

	appv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Path to the configuration file
var configPath string

// Disable the Virtual IP (bind to the existing network stack)
var disableVIP bool

// Start as a single node (no cluster), start as a leader in the cluster
var singleNode, startAsLeader *bool

// Run as a load balancer service (within a pod / kubernetes)
//var service bool

// ConfigMap name within a Kubernetes cluster
var configMap string

// Configure the level of loggin
var logLevel uint32

// [sample configuration] - flags
var cliConfig kubevip.Config
var cliConfigLB kubevip.LoadBalancer
var cliLocalPeer string
var cliRemotePeers, cliBackends []string

// Release - this struct contains the release information populated when building kube-vip
var Release struct {
	Version string
	Build   string
}

var kubeVipCmd = &cobra.Command{
	Use:   "kube-vip",
	Short: "This is a server for providing a Virtual IP and load-balancer for the Kubernetes control-plane",
}

func init() {

	// Manage logging
	kubeVipCmd.PersistentFlags().Uint32Var(&logLevel, "log", 4, "Set the level of logging")

	// Get the configuration file
	kubeVipStart.Flags().StringVarP(&configPath, "config", "c", "", "Path to a kube-vip configuration")
	kubeVipStart.Flags().BoolVarP(&disableVIP, "disableVIP", "d", false, "Disable the VIP functionality")

	// Pointers so we can see if they're nil (and not called)
	singleNode = kubeVipStart.Flags().BoolP("singleNode", "s", false, "Start as a single node cluster (raft disabled)")
	startAsLeader = kubeVipStart.Flags().BoolP("startAsLeader", "l", false, "Start this node as the leader")

	// Service flags
	kubeVipService.Flags().StringVarP(&configMap, "configMap", "c", "kube-vip", "The configuration map defined within the cluster")
	kubeVipService.Flags().BoolVar(&service.OutSideCluster, "OutSideCluster", false, "Start Controller outside of cluster")

	kubeVipCmd.AddCommand(kubeVipSample)
	kubeVipCmd.AddCommand(kubeVipService)
	kubeVipCmd.AddCommand(kubeVipStart)
	kubeVipCmd.AddCommand(kubeVipVersion)

	kubeVipSampleConfig.Flags().StringVar(&cliConfig.Interface, "interface", "eth0", "Name of the interface to bind to")
	kubeVipSampleConfig.Flags().StringVar(&cliConfig.VIP, "vip", "192.168.0.1", "The Virtual IP addres")
	kubeVipSampleConfig.Flags().BoolVar(&cliConfig.SingleNode, "singleNode", false, "Start this instance as a single node")
	kubeVipSampleConfig.Flags().BoolVar(&cliConfig.StartAsLeader, "startAsLeader", false, "Start this instance as the cluster leader")
	kubeVipSampleConfig.Flags().BoolVar(&cliConfig.GratuitousARP, "arp", false, "Use ARP broadcasts to improve VIP re-allocations")
	kubeVipSampleConfig.Flags().StringVar(&cliLocalPeer, "localPeer", "server1:192.168.0.1:10000", "Settings for this peer, format: id:address:port")
	kubeVipSampleConfig.Flags().StringSliceVar(&cliRemotePeers, "remotePeers", []string{"server2:192.168.0.2:10000", "server3:192.168.0.3:10000"}, "Comma seperated remotePeers, format: id:address:port")
	// Load Balancer flags
	kubeVipSampleConfig.Flags().BoolVar(&cliConfigLB.BindToVip, "lbBindToVip", false, "Bind example load balancer to VIP")
	kubeVipSampleConfig.Flags().StringVar(&cliConfigLB.Type, "lbType", "tcp", "Type of load balancer instance (tcp/http)")
	kubeVipSampleConfig.Flags().StringVar(&cliConfigLB.Name, "lbName", "Example Load Balancer", "The name of a load balancer instance")
	kubeVipSampleConfig.Flags().IntVar(&cliConfigLB.Port, "lbPort", 8080, "Port that load balander will expose on")
	kubeVipSampleConfig.Flags().StringSliceVar(&cliBackends, "lbBackends", []string{"192.168.0.1:8080", "192.168.0.2:8080"}, "Comma seperated backends, format: address:port")

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

var kubeVipSampleConfig = &cobra.Command{
	Use:   "config",
	Short: "Generate a Sample configuration",
	Run: func(cmd *cobra.Command, args []string) {
		//kubevip.SampleConfig()
		// Parse localPeer
		p, err := kubevip.ParsePeerConfig(cliLocalPeer)
		if err != nil {
			cmd.Help()
			log.Fatalln(err)
		}
		cliConfig.LocalPeer = *p

		// Parse remotePeers
		//Iterate backends
		for i := range cliRemotePeers {
			p, err := kubevip.ParsePeerConfig(cliRemotePeers[i])
			if err != nil {
				cmd.Help()
				log.Fatalln(err)
			}
			cliConfig.RemotePeers = append(cliConfig.RemotePeers, *p)
		}

		//Iterate backends
		for i := range cliBackends {
			b, err := kubevip.ParseBackendConfig(cliBackends[i])
			if err != nil {
				cmd.Help()
				log.Fatalln(err)
			}
			cliConfigLB.Backends = append(cliConfigLB.Backends, *b)
		}
		cliConfig.LoadBalancers = append(cliConfig.LoadBalancers, cliConfigLB)
		cliConfig.PrintConfig()
	},
}

var kubeVipSampleManifest = &cobra.Command{
	Use:   "manifest",
	Short: "Generate a Sample kubernetes manifest",
	Run: func(cmd *cobra.Command, args []string) {
		// Generate the sample manifest specification
		p := &appv1.Pod{
			TypeMeta: metav1.TypeMeta{
				Kind:       "Pod",
				APIVersion: "v1",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      "kube-vip",
				Namespace: "kube-system",
			},
			Spec: appv1.PodSpec{
				Containers: []appv1.Container{
					{
						Name:  "kube-vip",
						Image: fmt.Sprintf("plndr/kube-vip:%s", Release.Version),
						SecurityContext: &appv1.SecurityContext{
							Capabilities: &appv1.Capabilities{
								Add: []appv1.Capability{
									"NET_ADMIN",
									"SYS_TIME",
								},
							},
						},
						Command: []string{
							"/kube-vip",
							"start",
							"-c",
							"/vip.yaml",
						},
						VolumeMounts: []appv1.VolumeMount{
							appv1.VolumeMount{
								Name:      "config",
								MountPath: "/vip.yaml",
							},
						},
					},
				},
				Volumes: []appv1.Volume{
					appv1.Volume{
						Name: "config",
						VolumeSource: appv1.VolumeSource{
							HostPath: &appv1.HostPathVolumeSource{
								Path: "/etc/kube-vip/config.yaml",
							},
						},
					},
				},
				HostNetwork: true,
			},
		}

		b, _ := yaml.Marshal(p)
		fmt.Printf(string(b))
	},
}

var kubeVipStart = &cobra.Command{
	Use:   "start",
	Short: "Start the Virtual IP / Load balancer",
	Run: func(cmd *cobra.Command, args []string) {
		// Set the logging level for all subsequent functions
		log.SetLevel(log.Level(logLevel))

		if configPath == "" {
			cmd.Help()
			log.Fatalln("No Configuration has been specified")
		}

		c, err := kubevip.OpenConfig(configPath)
		if err != nil {
			log.Fatalf("%v", err)
		}

		// Check if these pointers are actually pointing to something (flags have been used)

		// Check if singlenode was called
		if cmd.Flags().Changed("singleNode") {
			c.SingleNode = *singleNode
		}

		// Check if startAsLeader was called
		if cmd.Flags().Changed("startAsLeader") {
			c.StartAsLeader = *startAsLeader
		}

		var newCluster *cluster.Cluster

		if c.SingleNode {
			// If the Virtual IP isn't disabled then create the netlink configuration
			newCluster, err = cluster.InitCluster(c, disableVIP)
			if err != nil {
				log.Fatalf("%v", err)
			}
			// Start a single node cluster
			newCluster.StartSingleNode(c, disableVIP)
		} else {
			if disableVIP {
				log.Fatalln("Cluster mode requires the Virtual IP to be enabled, use single node with no VIP")
			}

			// If the Virtual IP isn't disabled then create the netlink configuration
			newCluster, err = cluster.InitCluster(c, disableVIP)
			if err != nil {
				log.Fatalf("%v", err)
			}

			// Start a multi-node (raft) cluster
			err = newCluster.StartCluster(c)
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

var kubeVipService = &cobra.Command{
	Use:   "service",
	Short: "Start the Virtual IP / Load balancer as a service within a Kubernetes cluster",
	Run: func(cmd *cobra.Command, args []string) {
		// Set the logging level for all subsequent functions
		log.SetLevel(log.Level(logLevel))

		// Define the new service manager
		mgr, err := service.NewManager(configMap)
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
