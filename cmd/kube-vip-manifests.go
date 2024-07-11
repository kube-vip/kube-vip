package cmd

import (
	"fmt"
	"net"
	"strings"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v2"
)

// manifests will eventually deprecate the kubeadm set of subcommands
// manifests will be used to generate:
// - Pod spec manifest, mainly used for a static pod (kubeadm)
// - Daemonset manifest, mainly used to run kube-vip as a deamonset within Kubernetes (k3s/rke)

// var inCluster bool
var taint bool

func init() {
	kubeManifest.PersistentFlags().BoolVar(&inCluster, "inCluster", false, "Use the incluster token to authenticate to Kubernetes")
	kubeManifestDaemon.PersistentFlags().BoolVar(&taint, "taint", false, "Taint the manifest for only running on control planes")

	kubeManifest.AddCommand(kubeManifestPod)
	kubeManifest.AddCommand(kubeManifestDaemon)
	kubeManifest.AddCommand(kubeManifestRbac)
}

var kubeManifest = &cobra.Command{
	Use:   "manifest",
	Short: "Manifest functions",
	Run: func(cmd *cobra.Command, args []string) {
		_ = cmd.Help()
		// TODO - A load of text detailing what's actually happening
	},
}

var kubeManifestPod = &cobra.Command{
	Use:   "pod",
	Short: "Generate a Pod Manifest",
	Run: func(cmd *cobra.Command, args []string) {
		var err error

		// Set the logging level for all subsequent functions
		log.SetLevel(log.Level(logLevel))
		initConfig.LoadBalancers = append(initConfig.LoadBalancers, initLoadBalancer)
		// TODO - A load of text detailing what's actually happening
		if err := kubevip.ParseEnvironment(&initConfig); err != nil {
			log.Fatalf("Error parsing environment from config: %v", err)
		}

		// The control plane has a requirement for a VIP being specified
		if initConfig.EnableControlPlane && (initConfig.VIP == "" && initConfig.Address == "" && !initConfig.DDNS) {
			_ = cmd.Help()
			log.Fatalln("No address is specified for kube-vip to expose services on")
		}

		// Ensure there is an address to generate the CIDR from
		if initConfig.VIPCIDR == "" && initConfig.Address != "" {
			initConfig.VIPCIDR, err = generateCidrRange(initConfig.Address)
			if err != nil {
				log.Fatalln(err)
			}
		}

		cfg := kubevip.GeneratePodManifestFromConfig(&initConfig, Release.Version, inCluster)
		fmt.Println(cfg) // output manifest to stdout
	},
}

var kubeManifestDaemon = &cobra.Command{
	Use:   "daemonset",
	Short: "Generate a Daemonset Manifest",
	Run: func(cmd *cobra.Command, args []string) {
		var err error

		// Set the logging level for all subsequent functions
		log.SetLevel(log.Level(logLevel))
		initConfig.LoadBalancers = append(initConfig.LoadBalancers, initLoadBalancer)
		// TODO - A load of text detailing what's actually happening
		if err := kubevip.ParseEnvironment(&initConfig); err != nil {
			log.Fatalf("error parsing environment config: %v", err)
		}

		// TODO - check for certain things VIP/interfaces

		// The control plane has a requirement for a VIP being specified
		if initConfig.EnableControlPlane && (initConfig.VIP == "" && initConfig.Address == "" && !initConfig.DDNS) {
			_ = cmd.Help()
			log.Fatalln("No address is specified for kube-vip to expose services on")
		}

		// Ensure there is an address to generate the CIDR from
		if initConfig.VIPCIDR == "" && initConfig.Address != "" {
			initConfig.VIPCIDR, err = generateCidrRange(initConfig.Address)
			if err != nil {
				log.Fatalln(err)
			}
		}

		cfg := kubevip.GenerateDaemonsetManifestFromConfig(&initConfig, Release.Version, inCluster, taint)
		fmt.Println(cfg) // output manifest to stdout
	},
}

var kubeManifestRbac = &cobra.Command{
	Use:   "rbac",
	Short: "Generate an RBAC Manifest",
	Run: func(cmd *cobra.Command, args []string) {
		var err error

		// Set the logging level for all subsequent functions
		log.SetLevel(log.Level(logLevel))
		initConfig.LoadBalancers = append(initConfig.LoadBalancers, initLoadBalancer)
		// TODO - A load of text detailing what's actually happening
		if err := kubevip.ParseEnvironment(&initConfig); err != nil {
			log.Fatalf("Error parsing environment from config: %v", err)
		}

		// The control plane has a requirement for a VIP being specified
		if initConfig.EnableControlPlane && (initConfig.VIP == "" && initConfig.Address == "" && !initConfig.DDNS) {
			_ = cmd.Help()
			log.Fatalln("No address is specified for kube-vip to expose services on")
		}

		// Ensure there is an address to generate the CIDR from
		if initConfig.VIPCIDR == "" && initConfig.Address != "" {
			initConfig.VIPCIDR, err = generateCidrRange(initConfig.Address)
			if err != nil {
				log.Fatalln(err)
			}
		}

		cfg := kubevip.GenerateSA()
		b, _ := yaml.Marshal(cfg)
		fmt.Println(string(b)) // output manifest to stdout
	},
}

func generateCidrRange(address string) (string, error) {
	var cidrs []string

	addresses := strings.Split(address, ",")
	for _, a := range addresses {
		ip := net.ParseIP(a)

		if ip == nil {
			return "", fmt.Errorf("invalid IP address: %s from [%s]", a, address)
		}

		if ip.To4() != nil {
			cidrs = append(cidrs, "32")
		} else {
			cidrs = append(cidrs, "128")
		}
	}

	return strings.Join(cidrs, ","), nil
}
