package kubevip

// Environment variables
const (

	//vipArp - defines if the arp broadcast should be enabled
	vipArp = "vip_arp"

	//vipLeaderElection - defines if the kubernetes algorithim should be used
	vipLeaderElection = "vip_leaderelection"

	//vipLeaderElection - defines if the kubernetes algorithim should be used
	vipLeaseDuration = "vip_leaseduration"

	//vipLeaderElection - defines if the kubernetes algorithim should be used
	vipRenewDeadline = "vip_renewdeadline"

	//vipLeaderElection - defines if the kubernetes algorithim should be used
	vipRetryPeriod = "vip_retryperiod"

	//vipLogLevel - defines the level of logging to produce (5 being the most verbose)
	vipLogLevel = "vip_loglevel"

	//vipInterface - defines the interface that the vip should bind too
	vipInterface = "vip_interface"

	//vipCidr - defines the cidr that the vip will use
	vipCidr = "vip_cidr"

	/////////////////////////////////////
	// TO DO:
	// Determine how to tidy this mess up
	/////////////////////////////////////

	//vipAddress - defines the address that the vip will expose
	// DEPRECATED: will be removed in a next release
	vipAddress = "vip_address"

	// address - defines the address that would be used as a vip
	// it may be an IP or a DNS name, in case of a DNS name
	// kube-vip will try to resolve it and use the IP as a VIP
	address = "address"

	//port - defines the port for the VIP
	port = "port"

	// annotations
	annotations = "annotation"

	//vipDdns - defines if use dynamic dns to allocate IP for "address"
	vipDdns = "vip_ddns"

	//vipSingleNode - defines the vip start as a single node cluster
	vipSingleNode = "vip_singlenode"

	//vipStartLeader - will start this instance as the leader of the cluster
	vipStartLeader = "vip_startleader"

	//vipPeers defines the configuration of raft peer(s)
	vipPeers = "vip_peers"

	//vipLocalPeer defines the configuration of the local raft peer
	vipLocalPeer = "vip_localpeer"

	//vipRemotePeers defines the configuration of the local raft peer
	vipRemotePeers = "vip_remotepeers"

	//vipAddPeersToLB defines that RAFT peers should be added to the load-balancer
	vipAddPeersToLB = "vip_addpeerstolb"

	//vipPacket defines that the packet API will be used for EIP
	vipPacket = "vip_packet"

	//vipPacketProject defines which project within Packet to use
	vipPacketProject = "vip_packetproject"

	//vipPacketProjectID defines which projectID within Packet to use
	vipPacketProjectID = "vip_packetprojectid"

	//providerConfig defines a path to a configuration that should be parsed
	providerConfig = "provider_config"

	//bgpEnable defines if BGP should be enabled
	bgpEnable = "bgp_enable"
	//bgpRouterID defines the routerID for the BGP server
	bgpRouterID = "bgp_routerid"
	//bgpRouterInterface defines the interface that we can find the address for
	bgpRouterInterface = "bgp_routerinterface"
	//bgpRouterAS defines the AS for the BGP server
	bgpRouterAS = "bgp_as"
	//bgpPeerAddress defines the address for a BGP peer
	bgpPeerAddress = "bgp_peeraddress"
	//bgpPeers defines the address for a BGP peer
	bgpPeers = "bgp_peers"
	//bgpPeerAS defines the AS for a BGP peer
	bgpPeerAS = "bgp_peeras"
	//bgpPeerAS defines the AS for a BGP peer
	bgpPeerPassword = "bgp_peerpass"
	//bgpMultiHop enables mulithop routing
	bgpMultiHop = "bgp_multihop"

	//cpNamespace defines the namespace the control plane pods will run in
	cpNamespace = "cp_namespace"

	//cpEnable starts kube-vip in the hybrid mode
	cpEnable = "cp_enable"

	//cpEnable starts kube-vip in the hybrid mode
	svcEnable = "svc_enable"

	//lbEnable defines if the load-balancer should be enabled
	lbEnable = "lb_enable"

	//lbBindToVip defines if the load-balancer should bind ONLY to the virtual IP
	lbBindToVip = "lb_bindtovip"

	//lbName defines the name of load-balancer
	lbName = "lb_name"

	//lbType defines the type of load-balancer
	lbType = "lb_type"

	//lbPort defines the port of load-balancer
	lbPort = "lb_port"

	//lbBackendPort defines a port that ALL backends are using
	lbBackendPort = "lb_backendport"

	//lbBackends defines the backends of load-balancer
	lbBackends = "lb_backends"

	//vipConfigMap defines the configmap that kube-vip will watch for service definitions
	vipConfigMap = "vip_configmap"
)
