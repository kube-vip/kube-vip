package kubevip

import "net/url"

// Config defines all of the settings for the Virtual IP / Load-balancer
type Config struct {

	// LocalPeer is the configuration of this host
	LocalPeer RaftPeer `yaml:"localPeer"`

	// Peers are all of the peers within the RAFT cluster
	RemotePeers []RaftPeer `yaml:"remotePeers"`

	// AddPeersAsBackends, this will automatically add RAFT peers as backends to a loadbalancer
	AddPeersAsBackends bool `yaml:"addPeersAsBackends"`

	// VIP is the Virtual IP address exposed for the cluster
	VIP string `yaml:"vip"`

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

	// LoadBalancers are the various services we can load balance over
	LoadBalancers []LoadBalancer `yaml:"loadBalancers,omitempty"`
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
