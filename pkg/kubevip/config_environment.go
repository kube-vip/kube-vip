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
	"gopkg.in/yaml.v3"
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

	if env = os.Getenv(instanceName); env == "" {
		env = os.Getenv(strings.ToUpper(instanceName))
	}
	if env != "" {
		c.InstanceName = env
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

	env = os.Getenv(vipLoseLeadership)
	if env != "" {
		b, err := strconv.ParseBool(env)
		if err != nil {
			return err
		}
		c.LoseLeadership = b
	}

	env = os.Getenv(vipLoseLeadershipTimeoutSeconds)
	if env != "" {
		i, err := strconv.ParseInt(env, 10, 32)
		if err != nil {
			return fmt.Errorf("parsing env var %s (value: %s): %w", vipLoseLeadershipTimeoutSeconds, env, err)
		}
		c.LoseLeadershipTimeoutSeconds = int(i)
	}
	// Find (services) interface
	env = os.Getenv(vipServicesInterface)
	if env != "" {
		c.ServicesInterface = env
	}

	// Tolerate a down interface
	env = os.Getenv(vipAllowInterfaceNotUp)
	if env != "" {
		b, err := strconv.ParseBool(env)
		if err != nil {
			return err
		}
		c.AllowInterfaceNotUp = b
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

	// Find Services toggle. Related settings are independent: an environment
	// override remains valid when EnableServices came from the config file.
	env = os.Getenv(svcEnable)
	if env != "" {
		b, err := strconv.ParseBool(env)
		if err != nil {
			return err
		}
		c.EnableServices = b
	}

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

	// Skip Duplicate Address Detection when adding the VIP address
	env = os.Getenv(vipSkipDAD)
	if env != "" {
		b, err := strconv.ParseBool(env)
		if err != nil {
			return err
		}
		c.SkipDAD = b
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

	env = os.Getenv(bgpAttachIPToInterface)
	if env != "" {
		b, err := strconv.ParseBool(env)
		if err != nil {
			return err
		}
		c.BGPAttachIPToInterface = b
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
	} else if _, ok := os.LookupEnv(bgpPeers); ok {
		c.BGPConfig.Peers = nil
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

	// BGP health check options
	env = os.Getenv(controlPlaneHealthCheckAddress)
	if env != "" {
		c.ControlPlaneHealthCheck.Address = env
	}
	env = os.Getenv(controlPlaneHealthCheckPeriodSeconds)
	if env != "" {
		i, err := strconv.ParseInt(env, 10, 32)
		if err != nil {
			return fmt.Errorf("parsing env var %s (value: %s): %w", controlPlaneHealthCheckPeriodSeconds, env, err)
		}
		c.ControlPlaneHealthCheck.PeriodSeconds = int(i)
	}
	env = os.Getenv(controlPlaneHealthCheckTimeoutSeconds)
	if env != "" {
		i, err := strconv.ParseInt(env, 10, 32)
		if err != nil {
			return fmt.Errorf("parsing env var %s (value: %s): %w", controlPlaneHealthCheckTimeoutSeconds, env, err)
		}
		c.ControlPlaneHealthCheck.TimeoutSeconds = int(i)
	}
	env = os.Getenv(controlPlaneHealthCheckFailureThreshold)
	if env != "" {
		i, err := strconv.ParseInt(env, 10, 32)
		if err != nil {
			return fmt.Errorf("parsing env var %s (value: %s): %w", controlPlaneHealthCheckFailureThreshold, env, err)
		}
		c.ControlPlaneHealthCheck.FailureThreshold = int(i)
	}
	env = os.Getenv(controlPlaneHealthCheckCAPath)
	if env != "" {
		c.ControlPlaneHealthCheck.CAPath = env
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

	env = os.Getenv(perServiceElectionOnDemand)
	if env != "" {
		b, err := strconv.ParseBool(env)
		if err != nil {
			return err
		}
		c.PerServiceElectionOnDemand = b
	}

	// if this is set then we're enabling the internal SNAT rule that kube-vip adds to the egress chain
	env = os.Getenv(egressEnableInternalSNAT)
	if env != "" {
		b, err := strconv.ParseBool(env)
		if err != nil {
			return err
		}
		c.EnableInternalSNAT = b
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

	// Explicitly empty string variables clear lower-priority file values.
	stringEnvironment := map[string]*string{
		vipInterface: &c.Interface, vipServicesInterface: &c.ServicesInterface,
		vipLeaseName: &c.LeaseName, nodeName: &c.NodeName, vipAddress: &c.VIP, address: &c.Address,
		cpNamespace: &c.Namespace, kubernetesAddr: &c.KubernetesAddr, svcNamespace: &c.ServiceNamespace,
		svcLeaseName: &c.ServicesLeaseName, lbClassName: &c.LoadBalancerClassName, vipSubnet: &c.VIPSubnet,
		annotations: &c.Annotations, dnsMode: &c.DNSMode, dhcpMode: &c.DHCPMode,
		bgpRouterID: &c.BGPConfig.RouterID, mpbgpNexthop: &c.BGPConfig.MpbgpNexthop,
		mpbgpIPv4: &c.BGPConfig.MpbgpIPv4, mpbgpIPv6: &c.BGPConfig.MpbgpIPv6,
		bgpPeerPassword: &c.BGPPeerConfig.Password, bgpSourceIF: &c.BGPConfig.SourceIF,
		bgpSourceIP: &c.BGPConfig.SourceIP, controlPlaneHealthCheckAddress: &c.ControlPlaneHealthCheck.Address,
		controlPlaneHealthCheckCAPath: &c.ControlPlaneHealthCheck.CAPath, zebraURL: &c.BGPConfig.Zebra.URL,
		zebraSoftwareName: &c.BGPConfig.Zebra.SoftwareName, lbForwardingMethod: &c.LoadBalancerForwardingMethod,
		prometheusServer: &c.PrometheusHTTPServer, egressPodCidr: &c.EgressPodCidr,
		egressServiceCidr: &c.EgressServiceCidr, k8sConfigFile: &c.K8sConfigFile,
		mirrorDestInterface: &c.MirrorDestInterface, iptablesBackend: &c.IptablesBackend,
		configFile: &c.ConfigFile, debounceTime: &c.DebounceTime,
	}
	for name, destination := range stringEnvironment {
		if value, ok := os.LookupEnv(name); ok {
			*destination = value
		}
	}
	if value, ok := os.LookupEnv(instanceName); ok && value != "" {
		c.InstanceName = value
	} else if value, ok := os.LookupEnv(strings.ToUpper(instanceName)); ok {
		c.InstanceName = value
	}

	return nil
}

// LoadConfigFromFile loads runtime configuration from a JSON or YAML file.
func LoadConfigFromFile(configFilePath string) (*Config, error) {
	config := &Config{}
	if err := MergeConfigFromFile(config, configFilePath); err != nil {
		return nil, err
	}
	return config, nil
}

// MergeConfigFromFile overlays fields present in a JSON or YAML file. Decoding
// into the destination preserves defaults for omitted fields and preserves
// explicit false, zero, empty string, and empty collection values.
func MergeConfigFromFile(config *Config, configFilePath string) error {
	if configFilePath == "" {
		return fmt.Errorf("config file path is empty")
	}
	data, err := os.ReadFile(configFilePath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("config file does not exist: %s", configFilePath)
		}
		return fmt.Errorf("failed to read config file %s: %w", configFilePath, err)
	}

	ext := strings.ToLower(filepath.Ext(configFilePath))
	if ext != ".json" && ext != ".yaml" && ext != ".yml" {
		return fmt.Errorf("unsupported config file format %s; supported formats: .json, .yaml, .yml", ext)
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)
	if err := decoder.Decode(config); err != nil {
		format := strings.TrimPrefix(strings.ToUpper(ext), ".")
		return fmt.Errorf("failed to parse %s config file %s: %w", format, configFilePath, err)
	}

	// Runtime BGP consumes BGPConfig.Peers. BGPPeerConfig is retained for CLI
	// and environment compatibility and normalized when supplied by a file.
	if config.BGPPeerConfig.Address != "" {
		config.BGPConfig.Peers = append(config.BGPConfig.Peers, config.BGPPeerConfig)
		config.BGPPeerConfig = BGPPeer{}
	}
	config.ConfigFile = configFilePath
	return nil
}
