package kubevip

import (
	"net/url"

	"github.com/kube-vip/kube-vip/pkg/bgp"
)

// Config defines all of the settings for the Kube-Vip Pod
type Config struct {
	// Logging, settings
	Logging int `yaml:"logging"`

	// EnableARP, will use ARP to advertise the VIP address
	EnableARP bool `yaml:"enableARP"`

	// EnableBGP, will use BGP to advertise the VIP address
	EnableBGP bool `yaml:"enableBGP"`

	// EnableWireguard, will use wireguard to advertise the VIP address
	EnableWireguard bool `yaml:"enableWireguard"`

	// EnableRoutingTable, will use the routing table to advertise the VIP address
	EnableRoutingTable bool `yaml:"enableRoutingTable"`

	// EnableControlPlane, will enable the control plane functionality (used for hybrid behaviour)
	EnableControlPlane bool `yaml:"enableControlPlane"`

	// EnableServices, will enable the services functionality (used for hybrid behaviour)
	EnableServices bool `yaml:"enableServices"`

	// EnableServicesElection, will enable leaderElection per service
	EnableServicesElection bool `yaml:"enableServicesElection"`

	// LoadBalancerClassOnly, will enable load balancing only for services with LoadBalancerClass set to "kube-vip.io/kube-vip-class"
	LoadBalancerClassOnly bool `yaml:"lbClassOnly"`

	// LoadBalancerClassName, will limit the load balancing services to services with LoadBalancerClass set to this value
	LoadBalancerClassName string `yaml:"lbClassName"`

	// EnableServiceSecurity, will enable the use of iptables to secure services
	EnableServiceSecurity bool `yaml:"EnableServiceSecurity"`

	// ArpBroadcastRate, defines how often kube-vip will update the network about updates to the network
	ArpBroadcastRate int64 `yaml:"arpBroadcastRate"`

	// Annotations will define if we're going to wait and lookup configuration from Kubernetes node annotations
	Annotations string

	// LeaderElection defines the settings around Kubernetes LeaderElection
	LeaderElection

	// AddPeersAsBackends, this will automatically add RAFT peers as backends to a loadbalancer
	AddPeersAsBackends bool `yaml:"addPeersAsBackends"`

	// VIP is the Virtual IP address exposed for the cluster (TODO: deprecate)
	VIP string `yaml:"vip"`

	// VipSubnet is the Subnet that is applied to the VIP
	VIPSubnet string `yaml:"vipSubnet"`

	// VIPCIDR is cidr range for the VIP (primarily needed for BGP)
	VIPCIDR string `yaml:"vipCidr"`

	// Address is the IP or DNS Name to use as a VirtualIP
	Address string `yaml:"address"`

	// Listen port for the VirtualIP
	Port int `yaml:"port"`

	// Namespace will define which namespace the control plane pods will run in
	Namespace string `yaml:"namespace"`

	// Namespace will define which namespace the control plane pods will run in
	ServiceNamespace string `yaml:"serviceNamespace"`

	// use DDNS to allocate IP when Address is set to a DNS Name
	DDNS bool `yaml:"ddns"`

	// SingleNode will start the cluster as a single Node (Raft disabled)
	SingleNode bool `yaml:"singleNode"`

	// StartAsLeader, this will start this node as the leader before other nodes connect
	StartAsLeader bool `yaml:"startAsLeader"`

	// Interface is the network interface to bind to (default: First Adapter)
	Interface string `yaml:"interface,omitempty"`

	// ServicesInterface is the network interface to bind to for services (optional)
	ServicesInterface string `yaml:"servicesInterface,omitempty"`

	// EnableLoadBalancer, provides the flexibility to make the load-balancer optional
	EnableLoadBalancer bool `yaml:"enableLoadBalancer"`

	// Listen port for the IPVS Service
	LoadBalancerPort int `yaml:"lbPort"`

	// Forwarding method for the IPVS Service
	LoadBalancerForwardingMethod string `yaml:"lbForwardingMethod"`

	// Routing Table ID for when using routing table mode
	RoutingTableID int `yaml:"routingTableID"`

	// BGP Configuration
	BGPConfig     bgp.Config
	BGPPeerConfig bgp.Peer
	BGPPeers      []string

	// EnableMetal, will use the metal API to update the EIP <-> VIP (if BGP is enabled then BGP will be used)
	EnableMetal bool `yaml:"enableMetal"`

	// MetalAPIKey, is the API token used to authenticate to the API
	MetalAPIKey string

	// MetalProject, is the name of a particular defined project
	MetalProject string

	// MetalProjectID, is the name of a particular defined project
	MetalProjectID string

	// ProviderConfig, is the path to a provider configuration file
	ProviderConfig string

	// LoadBalancers are the various services we can load balance over
	LoadBalancers []LoadBalancer `yaml:"loadBalancers,omitempty"`

	// The hostport used to expose Prometheus metrics over an HTTP server
	PrometheusHTTPServer string `yaml:"prometheusHTTPServer,omitempty"`

	// Egress configuration

	// EgressPodCidr, this contains the pod cidr range to ignore Egress
	EgressPodCidr string

	// EgressServiceCidr, this contains the service cidr range to ignore
	EgressServiceCidr string

	// EgressWithNftables, this will use the iptables-nftables OVER iptables
	EgressWithNftables bool
}

// LeaderElection defines all of the settings for Kubernetes LeaderElection
type LeaderElection struct {
	// EnableLeaderElection will use the Kubernetes leader election algorithm
	EnableLeaderElection bool `yaml:"enableLeaderElection"`

	// Lease Duration - length of time a lease can be held for
	LeaseDuration int

	// RenewDeadline - length of time a host can attempt to renew its lease
	RenewDeadline int

	// RetryPerion - Number of times the host will retry to hold a lease
	RetryPeriod int
}

// LoadBalancer contains the configuration of a load balancing instance
type LoadBalancer struct {
	// Name of a LoadBalancer
	Name string `yaml:"name"`

	// Type of LoadBalancer, either TCP of HTTP(s)
	Type string `yaml:"type"`

	// Listening frontend port of this LoadBalancer instance
	Port int `yaml:"port"`

	// BindToVip will bind the load balancer port to the VIP itself
	BindToVip bool `yaml:"bindToVip"`

	// BackendPort, is a port that all backends are listening on (To be used to simplify building a list of backends)
	BackendPort int `yaml:"backendPort"`

	// Backends, is an array of backend servers
	Backends []BackEnd `yaml:"backends"`

	// Forwarding method of LoadBalancer, either Local, Tunnel, DirectRoute or Bypass
	ForwardingMethod string `yaml:"forwardingMethod"`
}

// BackEnd is a server we will load balance over
type BackEnd struct {
	// Backend Port to Load Balance to
	Port int `yaml:"port"`

	// Address of a server/service
	Address string `yaml:"address"`

	// URL is a raw URL to a backend service
	RawURL string `yaml:"url,omitempty"`

	// ParsedURL - A validated URL to a backend
	ParsedURL *url.URL `yaml:"parsedURL,omitempty"`
}
