package cmd

import (
	"fmt"
	"os"

	log "github.com/gookit/slog"
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
		// Set the logging level for all subsequent functions
		log.SetLogLevel(log.Level(logLevel))
		log.SetExitFunc(os.Exit)

		initConfig.LoadBalancers = append(initConfig.LoadBalancers, initLoadBalancer)
		// TODO - A load of text detailing what's actually happening
		err := kubevip.ParseEnvironment(&initConfig)
		if err != nil {
			log.Fatalf("Error parsing environment from config: %v", err)
		}

		// TODO - check for certain things VIP/interfaces
		if initConfig.Interface == "" {
			_ = cmd.Help()
			log.Fatal("No interface is specified for kube-vip to bind to")
		}

		if initConfig.VIP == "" && initConfig.Address == "" {
			_ = cmd.Help()
			log.Fatal("No address is specified for kube-vip to expose services on")
		}

		// Ensure there is an address to generate the CIDR from
		if initConfig.VIPCIDR == "" && initConfig.Address != "" {
			initConfig.VIPCIDR, err = GenerateCidrRange(initConfig.Address)
			if err != nil {
				log.Fatal(err)
			}
		}

		cfg := kubevip.GeneratePodManifestFromConfig(&initConfig, Release.Version, inCluster)
		fmt.Println(cfg) // output manifest to stdout
	},
}

var kubeKubeadmJoin = &cobra.Command{
	Use:   "join",
	Short: "kube-vip join",
	Run: func(cmd *cobra.Command, args []string) { //nolint TODO
		// Set the logging level for all subsequent functions
		log.SetLogLevel(log.Level(logLevel))

		initConfig.LoadBalancers = append(initConfig.LoadBalancers, initLoadBalancer)
		// TODO - A load of text detailing what's actually happening
		err := kubevip.ParseEnvironment(&initConfig)
		if err != nil {
			log.Fatalf("Error parsing environment from config: %v", err)
		}

		// TODO - check for certain things VIP/interfaces
		if initConfig.Interface == "" {
			_ = cmd.Help()
			log.Fatal("No interface is specified for kube-vip to bind to")
		}

		if initConfig.VIP == "" && initConfig.Address == "" {
			_ = cmd.Help()
			log.Fatal("No address is specified for kube-vip to expose services on")
		}

		if _, err := os.Stat(kubeConfigPath); os.IsNotExist(err) {
			log.Fatalf("Unable to find file [%s]", kubeConfigPath)
		}

		// Ensure there is an address to generate the CIDR from
		if initConfig.VIPCIDR == "" && initConfig.Address != "" {
			initConfig.VIPCIDR, err = GenerateCidrRange(initConfig.Address)
			if err != nil {
				log.Fatal(err)
			}
		}

		cfg := kubevip.GeneratePodManifestFromConfig(&initConfig, Release.Version, inCluster)
		fmt.Println(cfg) // output manifest to stdout
	},
}
