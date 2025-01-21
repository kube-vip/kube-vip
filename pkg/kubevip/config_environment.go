package kubevip

import (
	"encoding/json"
	"fmt"
	"math"
	"math/bits"
	"os"
	"strconv"

	"github.com/kube-vip/kube-vip/pkg/bgp"
	"github.com/kube-vip/kube-vip/pkg/detector"
)

// ParseEnvironment - will popultate the configuration from environment variables
func ParseEnvironment(c *Config) error {
	if c == nil {
		return nil
	}
	// Ensure that logging is set through the environment variables
	env := os.Getenv(vipLogLevel)
	// Set default value
	if env == "" {
		env = "4"
	}

	if env != "" {
		logLevel, err := strconv.ParseInt(env, 10, 32)
		if err != nil {
			panic("Unable to parse environment variable [vip_loglevel], should be int")
		}
		c.Logging = int(logLevel)
	}

	// Find interface
	env = os.Getenv(vipInterface)
	if env != "" {
		c.Interface = env
	}

	env = os.Getenv(vipInterfaceLoGlobal)
	if env != "" {
		b, err := strconv.ParseBool(env)
		if err != nil {
			return err
		}
		c.LoInterfaceGlobalScope = b
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

	// Attempt to find the Lease name from the environment variables
	env = os.Getenv(vipLeaseName)
	if env != "" {
		c.LeaseName = env
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

	// Attempt to find the Lease annotations from the environment variables
	env = os.Getenv(vipLeaseAnnotations)
	if env != "" {
		err := json.Unmarshal([]byte(env), &c.LeaseAnnotations)
		if err != nil {
			return err
		}
	}

	env = os.Getenv(nodeName)
	if env != "" {
		c.NodeName = env
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
		i, err := strconv.ParseUint(env, 10, 16)
		if err != nil {
			return err
		}
		c.Port = uint16(i)
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

	// Find the namespace that the control plane should use (for leaderElection lock)
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
		c.EnableControlPlane = b
	}

	// Find controlplane toggle
	env = os.Getenv(cpDetect)
	if env != "" {
		b, err := strconv.ParseBool(env)
		if err != nil {
			return err
		}
		c.DetectControlPlane = b
	}

	env = os.Getenv(kubernetesAddr)
	if env != "" {
		c.KubernetesAddr = env
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

		// Load-balancer class name
		env, exists := os.LookupEnv(lbClassName)
		if exists {
			c.LoadBalancerClassName = env
		}

		// Load-balancer class legacy handling
		env = os.Getenv(lbClassLegacyHandling)
		if env != "" {
			b, err := strconv.ParseBool(env)
			if err != nil {
				return err
			}
			c.LoadBalancerClassLegacyHandling = b
		}

		// Find the namespace that the control plane should use (for leaderElection lock)
		env = os.Getenv(svcNamespace)
		if env != "" {
			c.ServiceNamespace = env
		}

		// Gets the leaseName for services in arp mode
		env = os.Getenv(svcLeaseName)
		if env != "" {
			c.ServicesLeaseName = env
		}
	}

	// Find vip address cidr range
	env = os.Getenv(vipCidr)
	if env != "" {
		c.VIPCIDR = env
	}

	// Find vip address subnet
	env = os.Getenv(vipSubnet)
	if env != "" {
		c.VIPSubnet = env
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

	// Find if ARP is enabled
	env = os.Getenv(vipArp)
	if env != "" {
		b, err := strconv.ParseBool(env)
		if err != nil {
			return err
		}
		c.EnableARP = b
	}

	// Find if ARP is enabled
	env = os.Getenv(vipArpRate)
	if env != "" {
		i64, err := strconv.ParseInt(env, 10, 32)
		if err != nil {
			return err
		}
		c.ArpBroadcastRate = i64
	} else {
		// default to three seconds
		c.ArpBroadcastRate = 3000
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

	// Routing Table ID
	env = os.Getenv(vipRoutingTableID)
	if env != "" {
		i, err := strconv.ParseInt(env, 10, 64)
		if err != nil {
			return err
		}
		if i >= 0 && i <= math.MaxInt {
			c.RoutingTableID = int(i)
		} else if i < 0 {
			return fmt.Errorf("no support of negative [%d] in env var %q", i, vipRoutingTableID)
		} else {
			// +1 for the signing bit as it is 0 for positive integers
			return fmt.Errorf("no support for int64, system natively supports [int%d]", bits.OnesCount(math.MaxInt)+1)
		}
	}

	// Routing Table Type
	env = os.Getenv(vipRoutingTableType)
	if env != "" {
		i, err := strconv.ParseInt(env, 10, 32)
		if err != nil {
			return err
		}
		c.RoutingTableType = int(i)
	}

	// Routing protocol
	env = os.Getenv(vipRoutingProtocol)
	if env != "" {
		i, err := strconv.ParseInt(env, 10, 32)
		if err != nil {
			return err
		}
		c.RoutingProtocol = int(i)
	}

	// Clean routing table
	env = os.Getenv(vipCleanRoutingTable)
	if env != "" {
		b, err := strconv.ParseBool(env)
		if err != nil {
			return err
		}
		c.CleanRoutingTable = b
	}

	// DNS mode
	env = os.Getenv(dnsMode)
	if env != "" {
		c.DNSMode = env
	}

	// Disable updates for services (status.LoadBalancer.Ingress will not be updated)
	env = os.Getenv(disableServiceUpdates)
	if env != "" {
		b, err := strconv.ParseBool(env)
		if err != nil {
			return err
		}
		c.DisableServiceUpdates = b
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
		c.BGPPeerConfig.Spec.AS = uint32(u64)
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

	// MPBGP mode
	env = os.Getenv(mpbgpNexthop)
	if env != "" {
		c.BGPConfig.MpbgpNexthop = env
	}

	// MPBGP fixed IPv4
	env = os.Getenv(mpbgpIPv4)
	if env != "" {
		c.BGPConfig.MpbgpIPv4 = env
	}

	// MPBGP fixed IPv6
	env = os.Getenv(mpbgpIPv6)
	if env != "" {
		c.BGPConfig.MpbgpIPv6 = env
	}

	// BGP Peer mutlihop
	env = os.Getenv(bgpMultiHop)
	if env != "" {
		b, err := strconv.ParseBool(env)
		if err != nil {
			return err
		}
		c.BGPPeerConfig.Spec.MultiHop = b
	}

	// BGP Peer password
	env = os.Getenv(bgpPeerPassword)
	if env != "" {
		c.BGPPeerConfig.Spec.Password = env
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
		c.BGPPeerConfig.Spec.Address = env
		// If we've added in a peer configuration, then we should add it to the BGP configuration
		c.BGPConfig.Peers = append(c.BGPConfig.Peers, c.BGPPeerConfig)
	}

	// BGP Timers options
	env = os.Getenv(bgpHoldTime)
	if env != "" {
		u64, err := strconv.ParseUint(env, 10, 32)
		if err != nil {
			return err
		}
		c.BGPConfig.HoldTime = u64
	}
	env = os.Getenv(bgpKeepaliveInterval)
	if env != "" {
		u64, err := strconv.ParseUint(env, 10, 32)
		if err != nil {
			return err
		}
		c.BGPConfig.KeepaliveInterval = u64
	}

	// Enable the Equinix Metal API calls
	env = os.Getenv(vipPacket)
	if env != "" {
		b, err := strconv.ParseBool(env)
		if err != nil {
			return err
		}
		c.EnableMetal = b
	}

	// Find the Equinix Metal project name
	env = os.Getenv(vipPacketProject)
	if env != "" {
		// TODO - parse address net.Host()
		c.MetalProject = env
	}

	// Find the Equinix Metal project ID
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
		i, err := strconv.ParseUint(env, 10, 16)
		if err != nil {
			return err
		}
		c.LoadBalancerPort = uint16(i)
	}

	// Find loadbalancer forwarding method
	env = os.Getenv(lbForwardingMethod)
	if env != "" {
		c.LoadBalancerForwardingMethod = env
	}

	env = os.Getenv(EnableServiceSecurity)
	if env != "" {
		b, err := strconv.ParseBool(env)
		if err != nil {
			return err
		}
		c.EnableServiceSecurity = b
	}

	// Find if node labeling is enabled
	env = os.Getenv(EnableNodeLabeling)
	if env != "" {
		b, err := strconv.ParseBool(env)
		if err != nil {
			return err
		}
		c.EnableNodeLabeling = b
	}

	// Find Prometheus configuration
	env = os.Getenv(prometheusServer)
	if env != "" {
		c.PrometheusHTTPServer = env
	}

	// Set Egress configuration(s)
	env = os.Getenv(egressPodCidr)
	if env != "" {
		c.EgressPodCidr = env
	}

	env = os.Getenv(egressServiceCidr)
	if env != "" {
		c.EgressServiceCidr = env
	}

	// if this is set then we're enabling nftables
	env = os.Getenv(egressWithNftables)
	if env != "" {
		b, err := strconv.ParseBool(env)
		if err != nil {
			return err
		}
		c.EgressWithNftables = b
	}

	// check to see if we're using a specific path to the Kubernetes config file
	env = os.Getenv(k8sConfigFile)
	if env != "" {
		c.K8sConfigFile = env
	}

	env = os.Getenv(enableEndpointSlices)
	if env != "" {
		b, err := strconv.ParseBool(env)
		if err != nil {
			return err
		}
		c.EnableEndpointSlices = b
	}

	env = os.Getenv(mirrorDestInterface)
	if env != "" {
		c.MirrorDestInterface = env
	}

	env = os.Getenv(iptablesBackend)
	if env != "" {
		c.IptablesBackend = env
	}

	env = os.Getenv(backendHealthCheckInterval)
	if env != "" {
		i, err := strconv.ParseInt(env, 10, 32)
		if err != nil {
			return err
		}
		c.BackendHealthCheckInterval = int(i)
	}

	return nil
}
