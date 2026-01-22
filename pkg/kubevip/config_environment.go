package kubevip

import (
	"encoding/json"
	"fmt"
	"math"
	"math/bits"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kube-vip/kube-vip/pkg/detector"
	"github.com/kube-vip/kube-vip/pkg/utils"
	"sigs.k8s.io/yaml"
)

// ParseEnvironment - will popultate the configuration from environment variables
func ParseEnvironment(c *Config) error {
	if c == nil {
		return nil
	}
	// Ensure that logging is set through the environment variables
	env := os.Getenv(vipLogLevel)

	if env != "" {
		logLevel, err := strconv.ParseInt(env, 10, 32)
		if err != nil {
			return fmt.Errorf("unable to parse environment variable [vip_loglevel], should be int: %w", err)
		}
		c.Logging = int32(logLevel)
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

	// Determine if VIP should be preserved on leadership loss
	// true: VIP addresses remain on interface, only ARP/NDP broadcasting stops
	// false (default): VIP addresses are deleted on leadership loss (legacy behavior)
	env = os.Getenv(vipPreserveOnLeadershipLoss)
	if env != "" {
		b, err := strconv.ParseBool(env)
		if err != nil {
			return err
		}
		c.PreserveVIPOnLeadershipLoss = b
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

	// DHCP mode
	env = os.Getenv(dhcpMode)
	if env != "" {
		c.DHCPMode = env
	} else {
		if c.DNSMode != "first" {
			c.DHCPMode = c.DNSMode
		} else {
			c.DHCPMode = strings.ToLower(utils.IPv4Family)
		}
	}

	// DHCP backoff attempts
	env = os.Getenv(dhcpBackoffAttempts)
	if env != "" {
		tmp, err := strconv.ParseInt(env, 10, 32)
		if err != nil {
			return err
		}
		if tmp >= 0 {
			c.DHCPBackoffAttempts = uint(tmp)
		}
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
		c.BGPPeerConfig.AS = uint32(u64)
	}

	// Peer AS
	env = os.Getenv(bgpPeers)
	if env != "" {
		peers, err := ParseBGPPeerConfig(env)
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

	env = os.Getenv(zebraEnable)
	if env != "" {
		result, err := strconv.ParseBool(env)
		if err != nil {
			return err
		}
		c.BGPConfig.Zebra.Enabled = result
	}

	env = os.Getenv(zebraURL)
	if env != "" {
		c.BGPConfig.Zebra.URL = env
	}

	env = os.Getenv(zebraVersion)
	if env != "" {
		u64, err := strconv.ParseUint(env, 10, 32)
		if err != nil {
			return err
		}
		c.BGPConfig.Zebra.Version = uint32(u64)
	}

	env = os.Getenv(zebraSoftwareName)
	if env != "" {
		c.BGPConfig.Zebra.SoftwareName = env
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

	env = os.Getenv(enableEndpoints)
	if env != "" {
		b, err := strconv.ParseBool(env)
		if err != nil {
			return err
		}
		c.EnableEndpoints = b
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

	env = os.Getenv(healthCheckPort)
	if env != "" {
		i, err := strconv.ParseInt(env, 10, 32)
		if err != nil {
			return err
		}
		if i < 1024 {
			return fmt.Errorf("health check port should be > 1024")
		}
		c.HealthCheckPort = int(i)
	}

	env = os.Getenv(enableUPNP)
	if env != "" {
		b, err := strconv.ParseBool(env)
		if err != nil {
			return err
		}
		c.EnableUPNP = b
	}

	if env = os.Getenv(egressClean); env == "" {
		env = os.Getenv(strings.ToUpper(egressClean))
	}
	if env != "" {
		b, err := strconv.ParseBool(env)
		if err != nil {
			return err
		}
		c.EgressClean = b
	}

	// check for configuration file path
	env = os.Getenv(configFile)
	if env != "" {
		c.ConfigFile = env
	}

	return nil
}

// LoadConfigFromFile loads configuration from a JSON or YAML file
func LoadConfigFromFile(configFilePath string) (*Config, error) {
	if configFilePath == "" {
		return nil, fmt.Errorf("config file path is empty")
	}

	// Check if file exists
	if _, err := os.Stat(configFilePath); os.IsNotExist(err) {
		return nil, fmt.Errorf("config file does not exist: %s", configFilePath)
	}

	// Read file content
	data, err := os.ReadFile(configFilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file %s: %v", configFilePath, err)
	}

	var config Config
	ext := strings.ToLower(filepath.Ext(configFilePath))

	switch ext {
	case ".json":
		err = json.Unmarshal(data, &config)
		if err != nil {
			return nil, fmt.Errorf("failed to parse JSON config file %s: %v", configFilePath, err)
		}
	case ".yaml", ".yml":
		err = yaml.Unmarshal(data, &config)
		if err != nil {
			return nil, fmt.Errorf("failed to parse YAML config file %s: %v", configFilePath, err)
		}
	default:
		return nil, fmt.Errorf("unsupported config file format %s. Supported formats: .json, .yaml, .yml", ext)
	}

	return &config, nil
}

// MergeConfigFromFile merges configuration loaded from file with existing config
// Priority: command line flags > environment variables > config file
func MergeConfigFromFile(c *Config, configFilePath string) error {
	if configFilePath == "" {
		return nil // No config file specified, nothing to merge
	}

	fileConfig, err := LoadConfigFromFile(configFilePath)
	if err != nil {
		return err
	}

	// Merge file config with existing config
	// Only set values from file if they haven't been set by flags or env vars
	mergeConfigValues(c, fileConfig)

	return nil
}

// mergeConfigValues merges values from fileConfig into baseConfig
// Only overwrites zero values in baseConfig
func mergeConfigValues(baseConfig, fileConfig *Config) {
	// Basic configuration
	if baseConfig.Logging == 0 && fileConfig.Logging != 0 {
		baseConfig.Logging = fileConfig.Logging
	}

	// Network configuration
	if baseConfig.Interface == "" && fileConfig.Interface != "" {
		baseConfig.Interface = fileConfig.Interface
	}
	if baseConfig.ServicesInterface == "" && fileConfig.ServicesInterface != "" {
		baseConfig.ServicesInterface = fileConfig.ServicesInterface
	}
	if baseConfig.VIP == "" && fileConfig.VIP != "" {
		baseConfig.VIP = fileConfig.VIP
	}
	if baseConfig.VIPSubnet == "" && fileConfig.VIPSubnet != "" {
		baseConfig.VIPSubnet = fileConfig.VIPSubnet
	}
	if baseConfig.Address == "" && fileConfig.Address != "" {
		baseConfig.Address = fileConfig.Address
	}
	if baseConfig.Port == 0 && fileConfig.Port != 0 {
		baseConfig.Port = fileConfig.Port
	}
	if baseConfig.NodeName == "" && fileConfig.NodeName != "" {
		baseConfig.NodeName = fileConfig.NodeName
	}

	// Boolean flags - only merge if not explicitly set
	if !baseConfig.EnableARP && fileConfig.EnableARP {
		baseConfig.EnableARP = fileConfig.EnableARP
	}
	if !baseConfig.EnableBGP && fileConfig.EnableBGP {
		baseConfig.EnableBGP = fileConfig.EnableBGP
	}
	if !baseConfig.EnableWireguard && fileConfig.EnableWireguard {
		baseConfig.EnableWireguard = fileConfig.EnableWireguard
	}
	if !baseConfig.EnableRoutingTable && fileConfig.EnableRoutingTable {
		baseConfig.EnableRoutingTable = fileConfig.EnableRoutingTable
	}
	if !baseConfig.EnableControlPlane && fileConfig.EnableControlPlane {
		baseConfig.EnableControlPlane = fileConfig.EnableControlPlane
	}
	if !baseConfig.DetectControlPlane && fileConfig.DetectControlPlane {
		baseConfig.DetectControlPlane = fileConfig.DetectControlPlane
	}
	if !baseConfig.EnableServices && fileConfig.EnableServices {
		baseConfig.EnableServices = fileConfig.EnableServices
	}
	if !baseConfig.EnableServicesElection && fileConfig.EnableServicesElection {
		baseConfig.EnableServicesElection = fileConfig.EnableServicesElection
	}
	if !baseConfig.EnableNodeLabeling && fileConfig.EnableNodeLabeling {
		baseConfig.EnableNodeLabeling = fileConfig.EnableNodeLabeling
	}
	if !baseConfig.EnableLoadBalancer && fileConfig.EnableLoadBalancer {
		baseConfig.EnableLoadBalancer = fileConfig.EnableLoadBalancer
	}
	if !baseConfig.DDNS && fileConfig.DDNS {
		baseConfig.DDNS = fileConfig.DDNS
	}
	if !baseConfig.SingleNode && fileConfig.SingleNode {
		baseConfig.SingleNode = fileConfig.SingleNode
	}
	if !baseConfig.StartAsLeader && fileConfig.StartAsLeader {
		baseConfig.StartAsLeader = fileConfig.StartAsLeader
	}
	if !baseConfig.PreserveVIPOnLeadershipLoss && fileConfig.PreserveVIPOnLeadershipLoss {
		baseConfig.PreserveVIPOnLeadershipLoss = fileConfig.PreserveVIPOnLeadershipLoss
	}

	// Service configuration
	if baseConfig.Namespace == "" && fileConfig.Namespace != "" {
		baseConfig.Namespace = fileConfig.Namespace
	}
	if baseConfig.ServiceNamespace == "" && fileConfig.ServiceNamespace != "" {
		baseConfig.ServiceNamespace = fileConfig.ServiceNamespace
	}
	if baseConfig.ServicesLeaseName == "" && fileConfig.ServicesLeaseName != "" {
		baseConfig.ServicesLeaseName = fileConfig.ServicesLeaseName
	}

	// LoadBalancer configuration
	if baseConfig.LoadBalancerPort == 0 && fileConfig.LoadBalancerPort != 0 {
		baseConfig.LoadBalancerPort = fileConfig.LoadBalancerPort
	}
	if baseConfig.LoadBalancerForwardingMethod == "" && fileConfig.LoadBalancerForwardingMethod != "" {
		baseConfig.LoadBalancerForwardingMethod = fileConfig.LoadBalancerForwardingMethod
	}
	if baseConfig.LoadBalancerClassName == "" && fileConfig.LoadBalancerClassName != "" {
		baseConfig.LoadBalancerClassName = fileConfig.LoadBalancerClassName
	}

	// Routing Table configuration
	if baseConfig.RoutingTableID == 0 && fileConfig.RoutingTableID != 0 {
		baseConfig.RoutingTableID = fileConfig.RoutingTableID
	}
	if baseConfig.RoutingTableType == 0 && fileConfig.RoutingTableType != 0 {
		baseConfig.RoutingTableType = fileConfig.RoutingTableType
	}
	if baseConfig.RoutingProtocol == 0 && fileConfig.RoutingProtocol != 0 {
		baseConfig.RoutingProtocol = fileConfig.RoutingProtocol
	}

	// BGP configuration
	mergeBGPConfig(&baseConfig.BGPConfig, &fileConfig.BGPConfig)

	// Kubernetes configuration
	if baseConfig.K8sConfigFile == "" && fileConfig.K8sConfigFile != "" {
		baseConfig.K8sConfigFile = fileConfig.K8sConfigFile
	}

	// Leader Election configuration
	mergeLeaderElectionConfig(&baseConfig.KubernetesLeaderElection, &fileConfig.KubernetesLeaderElection)

	// Prometheus configuration
	if baseConfig.PrometheusHTTPServer == "" && fileConfig.PrometheusHTTPServer != "" {
		baseConfig.PrometheusHTTPServer = fileConfig.PrometheusHTTPServer
	}

	// DNS configuration
	if baseConfig.DNSMode == "" && fileConfig.DNSMode != "" {
		baseConfig.DNSMode = fileConfig.DNSMode
	}

	// DHCP configuration - mode
	if baseConfig.DHCPMode == "" && fileConfig.DHCPMode != "" {
		baseConfig.DHCPMode = fileConfig.DHCPMode
	}

	// DHCP configuration - backoff attempts
	if baseConfig.DHCPBackoffAttempts == DefaultDHCPBackoffAttempts && fileConfig.DHCPBackoffAttempts != DefaultDHCPBackoffAttempts {
		baseConfig.DHCPBackoffAttempts = fileConfig.DHCPBackoffAttempts
	}

	// Health check configuration
	if baseConfig.HealthCheckPort == 0 && fileConfig.HealthCheckPort != 0 {
		baseConfig.HealthCheckPort = fileConfig.HealthCheckPort
	}

	// Egress configuration
	if baseConfig.EgressPodCidr == "" && fileConfig.EgressPodCidr != "" {
		baseConfig.EgressPodCidr = fileConfig.EgressPodCidr
	}
	if baseConfig.EgressServiceCidr == "" && fileConfig.EgressServiceCidr != "" {
		baseConfig.EgressServiceCidr = fileConfig.EgressServiceCidr
	}

	// Mirror configuration
	if baseConfig.MirrorDestInterface == "" && fileConfig.MirrorDestInterface != "" {
		baseConfig.MirrorDestInterface = fileConfig.MirrorDestInterface
	}

	// Iptables configuration
	if baseConfig.IptablesBackend == "" && fileConfig.IptablesBackend != "" {
		baseConfig.IptablesBackend = fileConfig.IptablesBackend
	}

	// Backend health check interval
	if baseConfig.BackendHealthCheckInterval == 0 && fileConfig.BackendHealthCheckInterval != 0 {
		baseConfig.BackendHealthCheckInterval = fileConfig.BackendHealthCheckInterval
	}

	// ARP broadcast rate
	if baseConfig.ArpBroadcastRate == 0 && fileConfig.ArpBroadcastRate != 0 {
		baseConfig.ArpBroadcastRate = fileConfig.ArpBroadcastRate
	}

	// Annotations
	if baseConfig.Annotations == "" && fileConfig.Annotations != "" {
		baseConfig.Annotations = fileConfig.Annotations
	}

	// Load balancers slice
	if len(baseConfig.LoadBalancers) == 0 && len(fileConfig.LoadBalancers) > 0 {
		baseConfig.LoadBalancers = fileConfig.LoadBalancers
	}
}

// mergeBGPConfig merges BGP configuration
func mergeBGPConfig(base, file *BGPConfig) {
	if base.RouterID == "" && file.RouterID != "" {
		base.RouterID = file.RouterID
	}
	if base.AS == 0 && file.AS != 0 {
		base.AS = file.AS
	}
	if base.SourceIF == "" && file.SourceIF != "" {
		base.SourceIF = file.SourceIF
	}
	if base.SourceIP == "" && file.SourceIP != "" {
		base.SourceIP = file.SourceIP
	}
	if base.HoldTime == 0 && file.HoldTime != 0 {
		base.HoldTime = file.HoldTime
	}
	if base.KeepaliveInterval == 0 && file.KeepaliveInterval != 0 {
		base.KeepaliveInterval = file.KeepaliveInterval
	}
	if len(base.Peers) == 0 && len(file.Peers) > 0 {
		base.Peers = file.Peers
	}
}

// mergeLeaderElectionConfig merges leader election configuration
func mergeLeaderElectionConfig(base, file *KubernetesLeaderElection) {
	if base.LeaseName == "" && file.LeaseName != "" {
		base.LeaseName = file.LeaseName
	}
	if base.LeaseDuration == 0 && file.LeaseDuration != 0 {
		base.LeaseDuration = file.LeaseDuration
	}
	if base.RenewDeadline == 0 && file.RenewDeadline != 0 {
		base.RenewDeadline = file.RenewDeadline
	}
	if base.RetryPeriod == 0 && file.RetryPeriod != 0 {
		base.RetryPeriod = file.RetryPeriod
	}
	if len(base.LeaseAnnotations) == 0 && len(file.LeaseAnnotations) > 0 {
		base.LeaseAnnotations = file.LeaseAnnotations
	}
}
