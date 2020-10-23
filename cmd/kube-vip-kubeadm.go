package cmd

import (
	"context"
	"fmt"
	"net"
	"os"

	"github.com/plunder-app/kube-vip/pkg/kubevip"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
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
		cmd.Help()
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
		kubevip.ParseEnvironment(&initConfig)
		// TODO - check for certain things VIP/interfaces
		if initConfig.Interface == "" {
			cmd.Help()
			log.Fatalln("No interface is specified for kube-vip to bind to")
		}

		if initConfig.VIP == "" && initConfig.Address == "" {
			cmd.Help()
			log.Fatalln("No address is specified for kube-vip to expose services on")
		}
		cfg := kubevip.GeneratePodManifestFromConfig(&initConfig, Release.Version)

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
		kubevip.ParseEnvironment(&initConfig)
		// TODO - check for certain things VIP/interfaces
		if initConfig.Interface == "" {
			cmd.Help()
			log.Fatalln("No interface is specified for kube-vip to bind to")
		}

		if initConfig.VIP == "" && initConfig.Address == "" {
			cmd.Help()
			log.Fatalln("No address is specified for kube-vip to expose services on")
		}

		if _, err := os.Stat(kubeConfigPath); os.IsNotExist(err) {
			log.Fatalf("Unable to find file [%s]", kubeConfigPath)
		}

		// We will use kubeconfig in order to find all the master nodes
		// use the current context in kubeconfig
		config, err := clientcmd.BuildConfigFromFlags("", kubeConfigPath)
		if err != nil {
			log.Fatal(err.Error())
		}

		// create the clientset
		clientset, err := kubernetes.NewForConfig(config)
		if err != nil {
			log.Fatal(err.Error())
		}

		opts := metav1.ListOptions{}
		opts.LabelSelector = "node-role.kubernetes.io/master"
		nodes, err := clientset.CoreV1().Nodes().List(context.TODO(), opts)

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
		cfg := kubevip.GeneratePodManifestFromConfig(&initConfig, Release.Version)
		fmt.Println(cfg)
	},
}

func autoGenLocalPeer() (*kubevip.RaftPeer, error) {
	// hostname // address // defaultport
	h, err := os.Hostname()
	if err != nil {
		return nil, err
	}

	var a string
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	for _, address := range addrs {
		// check the address type and if it is not a loopback the display it
		if ipnet, ok := address.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() != nil {
				a = ipnet.IP.String()
				break
			}
		}
	}
	if a == "" {
		return nil, fmt.Errorf("Unable to find local address")
	}
	return &kubevip.RaftPeer{
		ID:      h,
		Address: a,
		Port:    10000,
	}, nil

}
