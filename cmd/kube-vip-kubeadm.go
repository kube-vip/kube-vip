package cmd

import (
	"fmt"
	"net"
	"os"
	"strconv"

	"github.com/ghodss/yaml"
	appv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/plunder-app/kube-vip/pkg/kubevip"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

// kubeadm adds two subcommands for managing a vip during a kubeadm init/join
// It is designed to operate "light" and take minimal input to start

var initConfig kubevip.Config
var initLoadBalancer kubevip.LoadBalancer

func init() {

	localpeer, err := autoGenLocalPeer()
	if err != nil {
		log.Fatalln(err)
	}
	initConfig.LocalPeer = *localpeer
	//initConfig.Peers = append(initConfig.Peers, *localpeer)
	kubeKubeadmInit.Flags().StringVar(&initConfig.Interface, "interface", "", "Name of the interface to bind to")
	kubeKubeadmInit.Flags().StringVar(&initConfig.VIP, "vip", "", "The Virtual IP addres")
	kubeKubeadmInit.Flags().BoolVar(&initConfig.StartAsLeader, "startAsLeader", true, "Start this instance as the cluster leader")

	kubeKubeadmInit.Flags().BoolVar(&initConfig.AddPeersAsBackends, "addPeersToLB", true, "The Virtual IP addres")
	kubeKubeadmInit.Flags().BoolVar(&initConfig.GratuitousARP, "arp", true, "Enable Arp for Vip changes")

	// Load Balancer flags
	kubeKubeadmInit.Flags().BoolVar(&initLoadBalancer.BindToVip, "lbBindToVip", true, "Bind example load balancer to VIP")
	kubeKubeadmInit.Flags().StringVar(&initLoadBalancer.Type, "lbType", "tcp", "Type of load balancer instance (tcp/http)")
	kubeKubeadmInit.Flags().StringVar(&initLoadBalancer.Name, "lbName", "Example Load Balancer", "The name of a load balancer instance")
	kubeKubeadmInit.Flags().IntVar(&initLoadBalancer.Port, "lbPort", 6443, "Port that load balander will expose on")
	kubeKubeadmInit.Flags().IntVar(&initLoadBalancer.BackendPort, "lbBackEndPort", 6444, "A port that all backends may be using (optional)")

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
		parseEnvironment(&initConfig)
		// TODO - check for certain things VIP/interfaces
		if initConfig.Interface == "" {
			cmd.Help()
			log.Fatalln("No interface is specified for kube-vip to bind to")
		}

		if initConfig.VIP == "" {
			cmd.Help()
			log.Fatalln("No address is specified for kube-vip to expose services on")
		}
		cfg := generateFromConfig(&initConfig, Release.Version)

		fmt.Println(cfg)
	},
}

var kubeKubeadmJoin = &cobra.Command{
	Use:   "join",
	Short: "kube-vip join",
	Run: func(cmd *cobra.Command, args []string) {
		// Set the logging level for all subsequent functions
		log.SetLevel(log.Level(logLevel))

		// TODO - A load of text detailing what's actually happening
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

// generateFromConfig will take a kube-vip config and generate a manifest
func generateFromConfig(c *kubevip.Config, imageVersion string) string {

	// build environment variables
	newEnvironment := []appv1.EnvVar{
		appv1.EnvVar{
			Name:  vipArp,
			Value: strconv.FormatBool(c.GratuitousARP),
		},
		appv1.EnvVar{
			Name:  vipInterface,
			Value: c.Interface,
		},
		appv1.EnvVar{
			Name:  vipAddress,
			Value: c.VIP,
		},
		appv1.EnvVar{
			Name:  vipStartLeader,
			Value: strconv.FormatBool(c.StartAsLeader),
		},
		appv1.EnvVar{
			Name:  vipAddPeersToLB,
			Value: strconv.FormatBool(c.AddPeersAsBackends),
		},
		appv1.EnvVar{
			Name:  lbBackendPort,
			Value: fmt.Sprintf("%d", c.LoadBalancers[0].Port),
		},
		appv1.EnvVar{
			Name:  lbName,
			Value: c.LoadBalancers[0].Name,
		},
		appv1.EnvVar{
			Name:  lbType,
			Value: c.LoadBalancers[0].Type,
		},
		appv1.EnvVar{
			Name:  lbBindToVip,
			Value: strconv.FormatBool(c.LoadBalancers[0].BindToVip),
		},
	}

	// Parse peers into a comma seperated string
	if len(c.RemotePeers) != 0 {
		var peers string
		for x := range c.RemotePeers {
			if x != 0 {
				peers = fmt.Sprintf("%s,%s:%s:%d", peers, c.RemotePeers[x].ID, c.RemotePeers[x].Address, c.RemotePeers[x].Port)

			} else {
				peers = fmt.Sprintf("%s:%s:%d", c.RemotePeers[x].ID, c.RemotePeers[x].Address, c.RemotePeers[x].Port)

			}
			fmt.Sprintf("%s,%s:%s:%d", peers, c.RemotePeers[x].ID, c.RemotePeers[x].Address, c.RemotePeers[x].Port)
			fmt.Sprintf("", peers)
		}
		peerEnvirontment := appv1.EnvVar{
			Name:  vipPeers,
			Value: peers,
		}
		newEnvironment = append(newEnvironment, peerEnvirontment)
	}

	newManifest := &appv1.Pod{
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
					Name:            "kube-vip",
					Image:           fmt.Sprintf("plndr/kube-vip:%s", imageVersion),
					ImagePullPolicy: appv1.PullAlways,
					SecurityContext: &appv1.SecurityContext{
						Capabilities: &appv1.Capabilities{
							Add: []appv1.Capability{
								"NET_ADMIN",
								"SYS_TIME",
							},
						},
					},
					Args: []string{
						"start",
					},
					Env: newEnvironment,
				},
			},
			HostNetwork: true,
		},
	}
	b, _ := yaml.Marshal(newManifest)
	return string(b)
}
