package cmd

import (
	"fmt"

	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
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
	Run: func(cmd *cobra.Command, args []string) { //nolint TODO
		_ = cmd.Help()
		// TODO - A load of text detailing what's actually happening
	},
}

var kubeManifestPod = &cobra.Command{
	Use:   "pod",
	Short: "Generate a Pod Manifest",
	Run: func(cmd *cobra.Command, args []string) { //nolint TODO
		var err error

		initConfig.LoadBalancers = append(initConfig.LoadBalancers, initLoadBalancer)
		// TODO - A load of text detailing what's actually happening
		if err := kubevip.ParseEnvironment(&initConfig); err != nil {
			log.Error("parsing environment", "err", err)
			return
		}

		// The control plane has a requirement for a VIP being specified
		if initConfig.EnableControlPlane && (initConfig.VIP == "" && initConfig.Address == "" && !initConfig.DDNS) {
			_ = cmd.Help()
			log.Error("No address is specified for kube-vip to expose services on")
			return
		}

		// Ensure there is an address to generate the CIDR from
		if initConfig.VIPSubnet == "" && initConfig.Address != "" {
			initConfig.VIPSubnet, err = GenerateCidrRange(initConfig.Address)
			if err != nil {
				log.Error("config parse", "err", err)
				return
			}
		}

		cfg := kubevip.GeneratePodManifestFromConfig(&initConfig, Release.Version, inCluster)
		fmt.Println(cfg) // output manifest to stdout
	},
}

var kubeManifestDaemon = &cobra.Command{
	Use:   "daemonset",
	Short: "Generate a Daemonset Manifest",
	Run: func(cmd *cobra.Command, args []string) { //nolint TODO
		var err error

		initConfig.LoadBalancers = append(initConfig.LoadBalancers, initLoadBalancer)
		// TODO - A load of text detailing what's actually happening
		if err := kubevip.ParseEnvironment(&initConfig); err != nil {
			log.Error("parsing environment", "err", err)
			return
		}
		// The control plane has a requirement for a VIP being specified
		if initConfig.EnableControlPlane && (initConfig.VIP == "" && initConfig.Address == "" && !initConfig.DDNS) {
			_ = cmd.Help()
			log.Error("No address is specified for kube-vip to expose services on")
			return
		}

		// Ensure there is an address to generate the CIDR from
		if initConfig.VIPSubnet == "" && initConfig.Address != "" {
			initConfig.VIPSubnet, err = GenerateCidrRange(initConfig.Address)
			if err != nil {
				log.Error("config parse", "err", err)
				return
			}
		}

		cfg := kubevip.GenerateDaemonsetManifestFromConfig(&initConfig, Release.Version, inCluster, taint)
		fmt.Println(cfg) // output manifest to stdout
	},
}

var kubeManifestRbac = &cobra.Command{
	Use:   "rbac",
	Short: "Generate an RBAC Manifest",
	Run: func(cmd *cobra.Command, args []string) { //nolint TODO
		var err error

		initConfig.LoadBalancers = append(initConfig.LoadBalancers, initLoadBalancer)
		// TODO - A load of text detailing what's actually happening
		if err := kubevip.ParseEnvironment(&initConfig); err != nil {
			log.Error("parsing environment", "err", err)
			return
		}

		// The control plane has a requirement for a VIP being specified
		if initConfig.EnableControlPlane && (initConfig.VIP == "" && initConfig.Address == "" && !initConfig.DDNS) {
			_ = cmd.Help()
			log.Error("No address is specified for kube-vip to expose services on")
			return
		}

		// Ensure there is an address to generate the CIDR from
		if initConfig.VIPSubnet == "" && initConfig.Address != "" {
			initConfig.VIPSubnet, err = GenerateCidrRange(initConfig.Address)
			if err != nil {
				log.Error("generating VIPSubnet", "err", err)
				return
			}
		}

		cfg := kubevip.GenerateSA()
		b, _ := yaml.Marshal(cfg)
		fmt.Println(string(b)) // output manifest to stdout
	},
}
