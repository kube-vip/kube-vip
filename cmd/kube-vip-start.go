package cmd

import (
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"

	"github.com/plunder-app/kube-vip/pkg/cluster"
	"github.com/plunder-app/kube-vip/pkg/kubevip"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

// Start as a single node (no cluster), start as a leader in the cluster
var startConfig kubevip.Config
var startConfigLB kubevip.LoadBalancer
var startLocalPeer string
var startRemotePeers, startBackends []string

func init() {
	// Get the configuration file
	kubeVipStart.Flags().StringVarP(&configPath, "config", "c", "", "Path to a kube-vip configuration")
	kubeVipStart.Flags().BoolVarP(&disableVIP, "disableVIP", "d", false, "Disable the VIP functionality")

	// Pointers so we can see if they're nil (and not called)
	kubeVipStart.Flags().StringVar(&startConfig.Interface, "interface", "eth0", "Name of the interface to bind to")
	kubeVipStart.Flags().StringVar(&startConfig.VIP, "vip", "192.168.0.1", "The Virtual IP addres")
	kubeVipStart.Flags().BoolVar(&startConfig.SingleNode, "singleNode", false, "Start this instance as a single node")
	kubeVipStart.Flags().BoolVar(&startConfig.StartAsLeader, "startAsLeader", false, "Start this instance as the cluster leader")
	kubeVipStart.Flags().BoolVar(&startConfig.GratuitousARP, "arp", false, "Use ARP broadcasts to improve VIP re-allocations")
	kubeVipStart.Flags().StringVar(&startLocalPeer, "localPeer", "server1:192.168.0.1:10000", "Settings for this peer, format: id:address:port")
	kubeVipStart.Flags().StringSliceVar(&startRemotePeers, "remotePeers", []string{"server2:192.168.0.2:10000", "server3:192.168.0.3:10000"}, "Comma seperated remotePeers, format: id:address:port")
	// Load Balancer flags
	kubeVipStart.Flags().BoolVar(&startConfigLB.BindToVip, "lbBindToVip", false, "Bind example load balancer to VIP")
	kubeVipStart.Flags().StringVar(&startConfigLB.Type, "lbType", "tcp", "Type of load balancer instance (tcp/http)")
	kubeVipStart.Flags().StringVar(&startConfigLB.Name, "lbName", "Example Load Balancer", "The name of a load balancer instance")
	kubeVipStart.Flags().IntVar(&startConfigLB.Port, "lbPort", 8080, "Port that load balander will expose on")
	kubeVipStart.Flags().IntVar(&startConfigLB.BackendPort, "lbBackEndPort", 6443, "A port that all backends may be using (optional)")
	kubeVipStart.Flags().StringSliceVar(&startBackends, "lbBackends", []string{"192.168.0.1:8080", "192.168.0.2:8080"}, "Comma seperated backends, format: address:port")
}

func parseEnvironment(c *kubevip.Config) error {

	// Find interface
	env := os.Getenv(vipInterface)
	if env != "" {
		c.Interface = env
	}

	// Find vip address
	env = os.Getenv(vipAddress)
	if env != "" {
		// TODO - parse address net.Host()
		c.VIP = env
	}

	// Find Single Node
	env = os.Getenv(vipSingleNode)
	if env != "" {
		b, err := strconv.ParseBool(env)
		if err != nil {
			return err
		}
		c.SingleNode = b
	}

	// Find Start As Leader
	// TODO - does this need depricating?

	// Find ARP
	env = os.Getenv(vipArp)
	if env != "" {
		b, err := strconv.ParseBool(env)
		if err != nil {
			return err
		}
		c.GratuitousARP = b
	}

	//Removal of seperate peer
	env = os.Getenv(vipLocalPeer)
	if env != "" {
		// Parse the string in format <id>:<address>:<port>
		peer, err := kubevip.ParsePeerConfig(env)
		if err != nil {
			return err
		}
		c.LocalPeer = *peer
	}

	env = os.Getenv(vipPeers)
	if env != "" {
		// TODO - perhaps make this optional?
		// Remove existing peers
		c.RemotePeers = []kubevip.RaftPeer{}

		// Parse the remote peers (comma seperated)
		s := strings.Split(env, ",")
		if len(s) == 0 {
			return fmt.Errorf("The Remote Peer List [%s] is unable to be parsed, should be in comma seperated format <id>:<address>:<port>", env)
		}
		for x := range s {
			// Parse the each remote peer string in format <id>:<address>:<port>
			peer, err := kubevip.ParsePeerConfig(s[x])
			if err != nil {
				return err
			}

			c.RemotePeers = append(c.RemotePeers, *peer)

		}
	}

	// Find Add Peers as Backends

	env = os.Getenv(vipAddPeersToLB)
	if env != "" {
		b, err := strconv.ParseBool(env)
		if err != nil {
			return err
		}
		c.AddPeersAsBackends = b
	}

	// Load Balancer configuration
	return parseEnvironmentLoadBalancer(c)
}

func parseEnvironmentLoadBalancer(c *kubevip.Config) error {
	// Check if an existing load-balancer configuration already exists
	if len(c.LoadBalancers) == 0 {
		c.LoadBalancers = append(c.LoadBalancers, kubevip.LoadBalancer{})
	}

	// Find LoadBalancer Port
	env := os.Getenv(lbPort)
	if env != "" {
		i, err := strconv.ParseInt(env, 8, 0)
		if err != nil {
			return err
		}
		c.LoadBalancers[0].Port = int(i)
	}

	// Find Type of LoadBalancer
	env = os.Getenv(lbType)
	if env != "" {
		c.LoadBalancers[0].Type = env
	}

	// Find Type of LoadBalancer Name
	env = os.Getenv(lbName)
	if env != "" {
		c.LoadBalancers[0].Name = env
	}

	// Find If LB should bind to Vip
	env = os.Getenv(lbBindToVip)
	if env != "" {
		b, err := strconv.ParseBool(env)
		if err != nil {
			return err
		}
		c.LoadBalancers[0].BindToVip = b
	}

	// Find global backendport
	env = os.Getenv(lbBackendPort)
	if env != "" {
		i, err := strconv.ParseInt(env, 8, 0)
		if err != nil {
			return err
		}
		c.LoadBalancers[0].BackendPort = int(i)
	}

	// Parse backends
	env = os.Getenv(lbBackends)
	if env != "" {
		// TODO - perhaps make this optional?
		// Remove existing backends
		c.LoadBalancers[0].Backends = []kubevip.BackEnd{}

		// Parse the remote peers (comma seperated)
		s := strings.Split(env, ",")
		if len(s) == 0 {
			return fmt.Errorf("The Backends List [%s] is unable to be parsed, should be in comma seperated format <address>:<port>", env)
		}
		for x := range s {
			// Parse the each remote peer string in format <address>:<port>

			be, err := kubevip.ParseBackendConfig(s[x])
			if err != nil {
				return err
			}

			c.LoadBalancers[0].Backends = append(c.LoadBalancers[0].Backends, *be)

		}
	}
	return nil
}

var kubeVipStart = &cobra.Command{
	Use:   "start",
	Short: "Start the Virtual IP / Load balancer",
	Run: func(cmd *cobra.Command, args []string) {
		// Set the logging level for all subsequent functions
		log.SetLevel(log.Level(logLevel))
		var err error

		// If a configuration file is loaded, then it will overwrite flags

		if configPath != "" {
			c, err := kubevip.OpenConfig(configPath)
			startConfig = *c
			if err != nil {
				log.Fatalf("%v", err)
			}
		}

		// parse environment variables, these will overwrite anything loaded or flags
		err = parseEnvironment(&startConfig)
		if err != nil {
			log.Fatalln(err)
		}

		var newCluster *cluster.Cluster

		if startConfig.SingleNode {
			// If the Virtual IP isn't disabled then create the netlink configuration
			newCluster, err = cluster.InitCluster(&startConfig, disableVIP)
			if err != nil {
				log.Fatalf("%v", err)
			}
			// Start a single node cluster
			newCluster.StartSingleNode(&startConfig, disableVIP)
		} else {
			if disableVIP {
				log.Fatalln("Cluster mode requires the Virtual IP to be enabled, use single node with no VIP")
			}

			// If the Virtual IP isn't disabled then create the netlink configuration
			newCluster, err = cluster.InitCluster(&startConfig, disableVIP)
			if err != nil {
				log.Fatalf("%v", err)
			}

			// Start a multi-node (raft) cluster
			err = newCluster.StartCluster(&startConfig)
			if err != nil {
				log.Fatalf("%v", err)
			}
		}

		signalChan := make(chan os.Signal, 1)
		signal.Notify(signalChan, os.Interrupt)

		<-signalChan

		newCluster.Stop()

	},
}
