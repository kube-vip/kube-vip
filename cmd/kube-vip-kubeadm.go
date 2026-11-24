package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/spf13/cobra"
)

// kubeadm adds two subcommands for managing a vip during a kubeadm init/join
// It is designed to operate "light" and take minimal input to start

func init() {
	kubeKubeadmJoin.Flags().StringVar(&kubeConfigPath, "config", "/etc/kubernetes/admin.conf", "Host kubeconfig path for the generated Pod (overrides --k8sConfigPath)")

	kubeKubeadm.AddCommand(kubeKubeadmInit)
	kubeKubeadm.AddCommand(kubeKubeadmJoin)
}

var kubeKubeadm = &cobra.Command{
	Use:   "kubeadm",
	Short: "Kubeadm functions",
	Long: `This command group provides utilities for generating static Pod manifests specifically tailored for the kubeadm bootstrapping process.
	It contains two subcommands:
  		- init:  Generates a manifest to be used during 'kubeadm init' on the first control-plane node.
  		- join:  Generates a manifest to be used during 'kubeadm join' for additional control-plane nodes.

The generated YAML manifest should be saved to the kubeadm static Pod directory (typically /etc/kubernetes/manifests/) so that kubeadm launches the kube-vip static Pod automatically.`,
	Run: func(cmd *cobra.Command, _ []string) {
		_ = cmd.Help()
	},
}

var kubeKubeadmInit = &cobra.Command{
	Use:   "init",
	Short: "kube-vip init",
	Long: `The 'init' subcommand generates a Kubernetes Pod manifest that kubeadm will start as a static Pod during the cluster initialisation phase.

This manifest runs kube-vip on the first control-plane node to advertise the Virtual IP (VIP) for the API server. The VIP is typically configured using ARP (Layer 2) or BGP (dynamic routing).

Required flags for this command:
  --interface   : The network interface to bind the VIP to (e.g., eth0).
  --vip or --address : The Virtual IP address or DNS name to use.

Example:
  kube-vip kubeadm init --interface eth0 --vip 192.168.1.100 --controlplane

The output YAML should be written to the kubeadm manifests directory, e.g.:
  kube-vip kubeadm init ... > /etc/kubernetes/manifests/kube-vip.yaml`,
	Run: func(cmd *cobra.Command, _ []string) {

		initConfig.LoadBalancers = append(initConfig.LoadBalancers, initLoadBalancer)
		err := kubevip.ParseEnvironment(&initConfig)
		if err != nil {
			log.Error("parsing environment", "err", err)
			return
		}
		if err := initConfig.Validate(); err != nil {
			log.Error("validating configuration", "err", err)
			return
		}

		// TODO - check for certain things VIP/interfaces
		if initConfig.Interface == "" {
			_ = cmd.Help()
			log.Error("No interface is specified for kube-vip to bind to")
			return
		}

		if initConfig.VIP == "" && initConfig.Address == "" {
			_ = cmd.Help()
			log.Error("No address is specified for kube-vip to expose services on")
			return
		}

		// Ensure there is an address to generate the CIDR from
		if initConfig.VIPSubnet == "" && initConfig.Address != "" {
			initConfig.VIPSubnet, err = GenerateCidrRange(initConfig.Address, initConfig.DNSMode)
			if err != nil {
				log.Error("generating VIPSubnet", "err", err)
				return
			}
		}

		cfg, err := kubevip.GeneratePodManifestFromConfig(&initConfig, image, Release.Version, inCluster)
		if err != nil {
			log.Error("unable to create manifest", "err", err)
			return
		}
		fmt.Println(cfg) // output manifest to stdout
	},
}

var kubeKubeadmJoin = &cobra.Command{
	Use:   "join",
	Short: "kube-vip join",
	Long: `The 'join' subcommand generates a Kubernetes Pod manifest for additional control-plane nodes joining an existing cluster via 'kubeadm join'.

The command checks that the kubeconfig file exists on the joining node and uses that file in the generated Pod manifest. An explicit --config takes precedence over --k8sConfigPath and the k8s_config_file environment variable. The default path is /etc/kubernetes/admin.conf.

Required flags for this command:
  --interface   : The network interface to bind the VIP to.
  --vip or --address : The Virtual IP address or DNS name (must match the VIP used during 'init').

Example:
  kube-vip kubeadm join --interface eth0 --vip 192.168.1.100

The output YAML should be saved to the kubeadm manifests directory on the joining node.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		cmd.SilenceUsage = true
		config := initConfig
		config.LoadBalancers = append(config.LoadBalancers, initLoadBalancer)
		if err := kubevip.ParseEnvironment(&config); err != nil {
			return fmt.Errorf("parsing environment: %w", err)
		}
		if err := config.Validate(); err != nil {
			return fmt.Errorf("validating configuration: %w", err)
		}

		if config.Interface == "" {
			return fmt.Errorf("no interface is specified for kube-vip to bind to")
		}

		if config.VIP == "" && config.Address == "" {
			return fmt.Errorf("no address is specified for kube-vip to expose services on")
		}

		if cmd.Flags().Changed("config") {
			config.K8sConfigFile = kubeConfigPath
		}
		if config.K8sConfigFile == "" {
			return fmt.Errorf("kubeconfig path is empty")
		}
		hostPath, err := filepath.Abs(config.K8sConfigFile)
		if err != nil {
			return fmt.Errorf("resolving kubeconfig path %q: %w", config.K8sConfigFile, err)
		}
		file, err := os.Stat(hostPath)
		if err != nil {
			return fmt.Errorf("checking kubeconfig %q: %w", hostPath, err)
		}
		if !file.Mode().IsRegular() {
			return fmt.Errorf("kubeconfig %q must be a regular file", hostPath)
		}
		config.K8sConfigFile = hostPath

		// Ensure there is an address to generate the CIDR from
		if config.VIPSubnet == "" && config.Address != "" {
			config.VIPSubnet, err = GenerateCidrRange(config.Address, config.DNSMode)
			if err != nil {
				return fmt.Errorf("generating VIPSubnet: %w", err)
			}
		}

		manifest, err := kubevip.GeneratePodManifestFromConfig(&config, image, Release.Version, inCluster)
		if err != nil {
			return fmt.Errorf("unable to create manifest: %w", err)
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), manifest)
		return err
	},
}
