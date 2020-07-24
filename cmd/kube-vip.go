package cmd

import (
	"fmt"
	"os"
	"strconv"

	"github.com/plunder-app/kube-vip/pkg/service"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

// Path to the configuration file
var configPath string

// Disable the Virtual IP (bind to the existing network stack)
var disableVIP bool

// Run as a load balancer service (within a pod / kubernetes)
var serviceArp bool

// ConfigMap name within a Kubernetes cluster
var configMap string

// Configure the level of loggin
var logLevel uint32

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

	// Service flags
	kubeVipService.Flags().StringVarP(&configMap, "configMap", "c", "kube-vip", "The configuration map defined within the cluster")
	kubeVipService.Flags().StringVarP(&service.Interface, "interface", "i", "eth0", "Name of the interface to bind to")
	kubeVipService.Flags().BoolVar(&service.OutSideCluster, "OutSideCluster", false, "Start Controller outside of cluster")
	kubeVipService.Flags().BoolVar(&service.EnableArp, "arp", false, "Use ARP broadcasts to improve VIP re-allocations")

	kubeVipCmd.AddCommand(kubeKubeadm)
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

		// User Environment variables as an option to make manifest clearer
		envInterface := os.Getenv("vip_interface")
		if envInterface != "" {
			service.Interface = envInterface
		}

		envConfigMap := os.Getenv("vip_configmap")
		if envInterface != "" {
			configMap = envConfigMap
		}

		envLog := os.Getenv("vip_loglevel")
		if envLog != "" {
			logLevel, err := strconv.Atoi(envLog)
			if err != nil {
				panic(fmt.Sprintf("Unable to parse environment variable [vip_loglevel], should be int"))
			}
			log.SetLevel(log.Level(logLevel))
		}

		envArp := os.Getenv("vip_arp")
		if envArp != "" {
			arpBool, err := strconv.ParseBool(envArp)
			if err != nil {
				panic(fmt.Sprintf("Unable to parse environment variable [arp], should be bool (true/false)"))
			}
			service.EnableArp = arpBool
		}

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
