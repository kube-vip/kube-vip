package cmd

import (
	"fmt"
	"os"

	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/spf13/cobra"
)

// kubeadm adds two subcommands for managing a vip during a kubeadm init/join
// It is designed to operate "light" and take minimal input to start

func init() {
	kubeKubeadmJoin.Flags().StringVar(&kubeConfigPath, "config", "/etc/kubernetes/admin.conf", "The path of a Kubernetes configuration file")

	kubeKubeadm.AddCommand(kubeKubeadmInit)
	kubeKubeadm.AddCommand(kubeKubeadmJoin)
}

var kubeKubeadm = &cobra.Command{
	Use:   "kubeadm",
	Short: "Kubeadm functions",
	Run: func(cmd *cobra.Command, args []string) { //nolint TODO
		_ = cmd.Help()
		// TODO - A load of text detailing what's actually happening
	},
}

var kubeKubeadmInit = &cobra.Command{
	Use:   "init",
	Short: "kube-vip init",
	Long:  "The \"init\" subcommand will generate the Kubernetes manifest that will be started by kubeadm through the kubeadm init process",
	Run: func(cmd *cobra.Command, args []string) { //nolint TODO

		initConfig.LoadBalancers = append(initConfig.LoadBalancers, initLoadBalancer)
		// TODO - A load of text detailing what's actually happening
		err := kubevip.ParseEnvironment(&initConfig)
		if err != nil {
			log.Error("parsing environment", "err", err)
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
			initConfig.VIPSubnet, err = GenerateCidrRange(initConfig.Address)
			if err != nil {
				log.Error("generating VIPSubnet", "err", err)
				return
			}
		}

		cfg := kubevip.GeneratePodManifestFromConfig(&initConfig, image, Release.Version, inCluster)
		fmt.Println(cfg) // output manifest to stdout
	},
}

var kubeKubeadmJoin = &cobra.Command{
	Use:   "join",
	Short: "kube-vip join",
	Run: func(cmd *cobra.Command, args []string) { //nolint TODO

		initConfig.LoadBalancers = append(initConfig.LoadBalancers, initLoadBalancer)
		// TODO - A load of text detailing what's actually happening
		err := kubevip.ParseEnvironment(&initConfig)
		if err != nil {
			log.Error("parsing environment", "err", err)
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

		if _, err := os.Stat(kubeConfigPath); os.IsNotExist(err) {
			log.Error("kubeConfig not found", "Path", kubeConfigPath)
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

		cfg := kubevip.GeneratePodManifestFromConfig(&initConfig, image, Release.Version, inCluster)
		fmt.Println(cfg) // output manifest to stdout
	},
}
