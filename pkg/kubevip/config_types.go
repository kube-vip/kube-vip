package kubevip

import (
	"net/url"

	"github.com/plunder-app/kube-vip/pkg/bgp"
)

// Config defines all of the settings for the Virtual IP / Load-balancer
type Config struct {

	// LeaderElection defines the settings around Kubernetes LeaderElection
	LeaderElection

	// LocalPeer is the configuration of this host
	LocalPeer RaftPeer `yaml:"localPeer"`

	// Peers are all of the peers within the RAFT cluster
	RemotePeers []RaftPeer `yaml:"remotePeers"`

	// AddPeersAsBackends, this will automatically add RAFT peers as backends to a loadbalancer
	AddPeersAsBackends bool `yaml:"addPeersAsBackends"`

	// VIP is the Virtual IP address exposed for the cluster
	VIP string `yaml:"vip"`

	// VIPCIDR is cidr range for the VIP (primarily needed for BGP)
	VIPCIDR string `yaml:"vipCidr"`

	// Address is the IP or DNS Name to use as a VirtualIP
	Address string `yaml:"address"`

	// Listen port for the VirtualIP
	Port int `yaml:"port"`

	// GratuitousARP will broadcast an ARP update when the VIP changes host
	GratuitousARP bool `yaml:"gratuitousARP"`

	// SingleNode will start the cluster as a single Node (Raft disabled)
	SingleNode bool `yaml:"singleNode"`

	// StartAsLeader, this will start this node as the leader before other nodes connect
	StartAsLeader bool `yaml:"startAsLeader"`

	// Interface is the network interface to bind to (default: First Adapter)
	Interface string `yaml:"interface,omitempty"`

	// EnableLoadBalancer, provides the flexibility to make the load-balancer optional
	EnableLoadBalancer bool `yaml:"enableLoadBalancer"`

	// EnableBGP, will use BGP to advertise the VIP address
	EnableBGP bool `yaml:"enableBGP"`

	// BGP Configuration
	BGPConfig     bgp.Config
	BGPPeerConfig bgp.Peer

	// EnablePacket, will use the packet API to update the EIP <-> VIP (if BGP is enabled then BGP will be used)
	EnablePacket bool `yaml:"enablePacket"`

	// PacketAPIKey, is the API token used to authenticate to the API
	PacketAPIKey string

	// PacketProject, is the name of a particular defined project
	PacketProject string

	// LoadBalancers are the various services we can load balance over
	LoadBalancers []LoadBalancer `yaml:"loadBalancers,omitempty"`
}

// LeaderElection defines all of the settings for Kubernetes LeaderElection
type LeaderElection struct {

	// EnableLeaderElection will use the Kubernetes leader election algorithim
	EnableLeaderElection bool `yaml:"enableLeaderElection"`

	// Lease Duration - length of time a lease can be held for
	LeaseDuration int

	// RenewDeadline - length of time a host can attempt to renew its lease
	RenewDeadline int

	// RetryPerion - Number of times the host will retry to hold a lease
	RetryPeriod int
}

// RaftPeer details the configuration of all cluster peers
type RaftPeer struct {
	// ID is the unique identifier a peer instance
	ID string `yaml:"id"`

	// IP Address of a peer instance
	Address string `yaml:"address"`

	// Listening port of this peer instance
	Port int `yaml:"port"`
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

	//BackendPort, is a port that all backends are listening on (To be used to simplify building a list of backends)
	BackendPort int `yaml:"backendPort"`

	//Backends, is an array of backend servers
	Backends []BackEnd `yaml:"backends"`
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
