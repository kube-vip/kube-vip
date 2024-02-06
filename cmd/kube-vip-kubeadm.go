package cmd

import (
	"os"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	log "github.com/sirupsen/logrus"
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
	Run: func(cmd *cobra.Command, args []string) {
		_ = cmd.Help()
		// TODO - A load of text detailing what's actually happening
	},
}

var kubeKubeadmInit = &cobra.Command{
	Use:   "init",
	Short: "kube-vip init",
	Long:  "The \"init\" subcommand will generate the Kubernetes manifest that will be started by kubeadm through the kubeadm init process",
	Run: func(cmd *cobra.Command, args []string) {
		// Set the logging level for all subsequent functions
		log.SetLevel(log.Level(logLevel))
		initConfig.LoadBalancers = append(initConfig.LoadBalancers, initLoadBalancer)
		// TODO - A load of text detailing what's actually happening
		err := kubevip.ParseEnvironment(&initConfig)
		if err != nil {
			log.Fatalf("Error parsing environment from config: %v", err)
		}

		// TODO - check for certain things VIP/interfaces
		if initConfig.Interface == "" {
			_ = cmd.Help()
			log.Fatalln("No interface is specified for kube-vip to bind to")
		}

		if initConfig.VIP == "" && initConfig.Address == "" {
			_ = cmd.Help()
			log.Fatalln("No address is specified for kube-vip to expose services on")
		}
		cfg := kubevip.GeneratePodManifestFromConfig(&initConfig, Release.Version, inCluster)

		log.Info(cfg)
	},
}

var kubeKubeadmJoin = &cobra.Command{
	Use:   "join",
	Short: "kube-vip join",
	Run: func(cmd *cobra.Command, args []string) {
		// Set the logging level for all subsequent functions
		log.SetLevel(log.Level(logLevel))

		initConfig.LoadBalancers = append(initConfig.LoadBalancers, initLoadBalancer)
		// TODO - A load of text detailing what's actually happening
		err := kubevip.ParseEnvironment(&initConfig)
		if err != nil {
			log.Fatalf("Error parsing environment from config: %v", err)
		}

		// TODO - check for certain things VIP/interfaces
		if initConfig.Interface == "" {
			_ = cmd.Help()
			log.Fatalln("No interface is specified for kube-vip to bind to")
		}

		if initConfig.VIP == "" && initConfig.Address == "" {
			_ = cmd.Help()
			log.Fatalln("No address is specified for kube-vip to expose services on")
		}

		if _, err := os.Stat(kubeConfigPath); os.IsNotExist(err) {
			log.Fatalf("Unable to find file [%s]", kubeConfigPath)
		}

		// Generate manifest and print
		cfg := kubevip.GeneratePodManifestFromConfig(&initConfig, Release.Version, inCluster)
		log.Info(cfg)
	},
}
