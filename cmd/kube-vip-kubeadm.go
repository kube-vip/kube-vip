package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/kube-vip/kube-vip/pkg/k8s"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

		fmt.Println(cfg)
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

		// We will use kubeconfig in order to find all the master nodes
		// use the current context in kubeconfig
		clientset, err := k8s.NewClientset(kubeConfigPath, false, "")
		if err != nil {
			log.Fatal(err.Error())
		}

		opts := metav1.ListOptions{}
		opts.LabelSelector = "node-role.kubernetes.io/master"
		nodes, err := clientset.CoreV1().Nodes().List(context.TODO(), opts)
		if err != nil {
			log.Fatal(err.Error())
		}
		// Iterate over all nodes that are masters and find the details to build a peer list
		for x := range nodes.Items {
			// Get hostname and address
			var nodeAddress, nodeHostname string
			for y := range nodes.Items[x].Status.Addresses {
				switch nodes.Items[x].Status.Addresses[y].Type {
				case corev1.NodeHostName:
					nodeHostname = nodes.Items[x].Status.Addresses[y].Address
				case corev1.NodeInternalIP:
					nodeAddress = nodes.Items[x].Status.Addresses[y].Address
				}
			}

			newPeer, err := kubevip.ParsePeerConfig(fmt.Sprintf("%s:%s:%d", nodeHostname, nodeAddress, 10000))
			if err != nil {
				panic(err.Error())
			}
			initConfig.RemotePeers = append(initConfig.RemotePeers, *newPeer)

		}
		// Generate manifest and print
		cfg := kubevip.GeneratePodManifestFromConfig(&initConfig, Release.Version, inCluster)
		fmt.Println(cfg)
	},
}
