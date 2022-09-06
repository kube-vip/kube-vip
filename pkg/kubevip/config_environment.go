package kubevip

import (
	"os"
	"strconv"

	"github.com/kube-vip/kube-vip/pkg/bgp"
	"github.com/kube-vip/kube-vip/pkg/detector"
	log "github.com/sirupsen/logrus"
)

// ParseEnvironment - will populate the configuration from environment variables
func ParseEnvironment(c *Config) error {

	// Ensure that logging is set through the environment variables
	env := os.Getenv(vipLogLevel)
	if env != "" {
		logLevel, err := strconv.ParseUint(env, 10, 32)
		if err != nil {
			panic("Unable to parse environment variable [vip_loglevel], should be int")
		}
		log.SetLevel(log.Level(logLevel))
	}

	// Find interface
	env = os.Getenv(vipInterface)
	if env != "" {
		c.Interface = env
	}

	// Find (services) interface
	env = os.Getenv(vipServicesInterface)
	if env != "" {
		c.ServicesInterface = env
	}

	// Find provider configuration
	env = os.Getenv(providerConfig)
	if env != "" {
		c.ProviderConfig = env
	}

	// Find Kubernetes Leader Election configuration
	env = os.Getenv(vipLeaderElection)
	if env != "" {
		b, err := strconv.ParseBool(env)
		if err != nil {
			return err
		}
		c.EnableLeaderElection = b
	}

	// Attempt to find the Lease configuration from the environment variables
	env = os.Getenv(vipLeaseDuration)
	if env != "" {
		i, err := strconv.ParseInt(env, 10, 32)
		if err != nil {
			return err
		}
		c.LeaseDuration = int(i)
	}

	env = os.Getenv(vipRenewDeadline)
	if env != "" {
		i, err := strconv.ParseInt(env, 10, 32)
		if err != nil {
			return err
		}
		c.RenewDeadline = int(i)
	}

	env = os.Getenv(vipRetryPeriod)
	if env != "" {
		i, err := strconv.ParseInt(env, 10, 32)
		if err != nil {
			return err
		}
		c.RetryPeriod = int(i)
	}

	// Find vip address
	env = os.Getenv(vipAddress)
	if env != "" {
		// TODO - parse address net.Host()
		c.VIP = env
		// } else {
		// 	c.VIP = os.Getenv(address)
	}

	// Find address
	env = os.Getenv(address)
	if env != "" {
		// TODO - parse address net.Host()
		c.Address = env
	}

	// Find vip port
	env = os.Getenv(port)
	if env != "" {
		i, err := strconv.ParseInt(env, 10, 32)
		if err != nil {
			return err
		}
		c.Port = int(i)
	}

	// Find vipDdns
	env = os.Getenv(vipDdns)
	if env != "" {
		b, err := strconv.ParseBool(env)
		if err != nil {
			return err
		}
		c.DDNS = b
	}

	// Find the namespace that the control pane should use (for leaderElection lock)
	env = os.Getenv(cpNamespace)
	if env != "" {
		c.Namespace = env
	}

	// Find controlplane toggle
	env = os.Getenv(cpEnable)
	if env != "" {
		b, err := strconv.ParseBool(env)
		if err != nil {
			return err
		}
		c.EnableControlPane = b
	}

	// Find Services toggle
	env = os.Getenv(svcEnable)
	if env != "" {
		b, err := strconv.ParseBool(env)
		if err != nil {
			return err
		}
		c.EnableServices = b

		// Find Services leader Election
		env = os.Getenv(svcElection)
		if env != "" {
			b, err := strconv.ParseBool(env)
			if err != nil {
				return err
			}
			c.EnableServicesElection = b
		}

		// Find load-balancer class only
		env = os.Getenv(lbClassOnly)
		if env != "" {
			b, err := strconv.ParseBool(env)
			if err != nil {
				return err
			}
			c.LoadBalancerClassOnly = b
		}
	}

	// Find vip address cidr range
	env = os.Getenv(vipCidr)
	if env != "" {
		c.VIPCIDR = env
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

	// Find annotation configuration
	env = os.Getenv(annotations)
	if env != "" {
		c.Annotations = env
	}

	// Find Start As Leader
	// TODO - does this need deprecating?
	// Required when the host sets itself as leader before the state change
	env = os.Getenv(vipStartLeader)
	if env != "" {
		b, err := strconv.ParseBool(env)
		if err != nil {
			return err
		}
		c.StartAsLeader = b
	}

	// Find ARP
	env = os.Getenv(vipArp)
	if env != "" {
		b, err := strconv.ParseBool(env)
		if err != nil {
			return err
		}
		c.EnableARP = b
	}

	// Wireguard Mode
	env = os.Getenv(vipWireguard)
	if env != "" {
		b, err := strconv.ParseBool(env)
		if err != nil {
			return err
		}
		c.EnableWireguard = b
	}

	// Routing Table Mode
	env = os.Getenv(vipRoutingTable)
	if env != "" {
		b, err := strconv.ParseBool(env)
		if err != nil {
			return err
		}
		c.EnableRoutingTable = b
	}

	// BGP Server options
	env = os.Getenv(bgpEnable)
	if env != "" {
		b, err := strconv.ParseBool(env)
		if err != nil {
			return err
		}
		c.EnableBGP = b
	}

	// BGP Router interface determines an interface that we can use to find an address for
	env = os.Getenv(bgpRouterInterface)
	if env != "" {
		_, address, err := detector.FindIPAddress(env)
		if err != nil {
			return err
		}
		c.BGPConfig.RouterID = address
	}

	// RouterID
	env = os.Getenv(bgpRouterID)
	if env != "" {
		c.BGPConfig.RouterID = env
	}

	// AS
	env = os.Getenv(bgpRouterAS)
	if env != "" {
		u64, err := strconv.ParseUint(env, 10, 32)
		if err != nil {
			return err
		}
		c.BGPConfig.AS = uint32(u64)
	}

	// Peer AS
	env = os.Getenv(bgpPeerAS)
	if env != "" {
		u64, err := strconv.ParseUint(env, 10, 32)
		if err != nil {
			return err
		}
		c.BGPPeerConfig.AS = uint32(u64)
	}

	// Peer AS
	env = os.Getenv(bgpPeers)
	if env != "" {
		peers, err := bgp.ParseBGPPeerConfig(env)
		if err != nil {
			return err
		}
		c.BGPConfig.Peers = peers
	}

	// BGP Peer mutlihop
	env = os.Getenv(bgpMultiHop)
	if env != "" {
		b, err := strconv.ParseBool(env)
		if err != nil {
			return err
		}
		c.BGPPeerConfig.MultiHop = b
	}

	// BGP Peer password
	env = os.Getenv(bgpPeerPassword)
	if env != "" {
		c.BGPPeerConfig.Password = env
	}

	// BGP Source Interface
	env = os.Getenv(bgpSourceIF)
	if env != "" {
		c.BGPConfig.SourceIF = env
	}

	// BGP Source Address
	env = os.Getenv(bgpSourceIP)
	if env != "" {
		c.BGPConfig.SourceIP = env
	}

	// BGP Peer options, add them if relevant
	env = os.Getenv(bgpPeerAddress)
	if env != "" {
		c.BGPPeerConfig.Address = env
		// If we've added in a peer configuration, then we should add it to the BGP configuration
		c.BGPConfig.Peers = append(c.BGPConfig.Peers, c.BGPPeerConfig)
	}

	// Enable the Packet API calls
	env = os.Getenv(vipPacket)
	if env != "" {
		b, err := strconv.ParseBool(env)
		if err != nil {
			return err
		}
		c.EnableMetal = b
	}

	// Find the Packet project name
	env = os.Getenv(vipPacketProject)
	if env != "" {
		// TODO - parse address net.Host()
		c.MetalProject = env
	}

	// Find the Packet project ID
	env = os.Getenv(vipPacketProjectID)
	if env != "" {
		// TODO - parse address net.Host()
		c.MetalProjectID = env
	}

	// Enable the load-balancer
	env = os.Getenv(lbEnable)
	if env != "" {
		b, err := strconv.ParseBool(env)
		if err != nil {
			return err
		}
		c.EnableLoadBalancer = b
	}

	// Find loadbalancer port
	env = os.Getenv(lbPort)
	if env != "" {
		i, err := strconv.ParseInt(env, 10, 32)
		if err != nil {
			return err
		}
		c.LoadBalancerPort = int(i)
	}

	// Find loadbalancer forwarding method
	env = os.Getenv(lbForwardingMethod)
	if env != "" {
		c.LoadBalancerForwardingMethod = env
	}

	// Find Prometheus configuration
	env = os.Getenv(prometheusServer)
	if env != "" {
		c.PrometheusHTTPServer = env
	}

	return nil
}
