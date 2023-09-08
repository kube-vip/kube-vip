package kubevip

// Environment variables
const (

	// vipArp - defines if the arp broadcast should be enabled
	vipArp = "vip_arp"

	// vip_arpRate - defines the rate of gARP broadcasts
	vipArpRate = "vip_arpRate"

	// vipLeaderElection - defines if the kubernetes algorithm should be used
	vipLeaderElection = "vip_leaderelection"

	// vipLeaseName - defines the name of the lease lock
	vipLeaseName = "vip_leasename"

	// vipLeaderElection - defines if the kubernetes algorithm should be used
	vipLeaseDuration = "vip_leaseduration"

	// vipLeaderElection - defines if the kubernetes algorithm should be used
	vipRenewDeadline = "vip_renewdeadline"

	// vipLeaderElection - defines if the kubernetes algorithm should be used
	vipRetryPeriod = "vip_retryperiod"

	// vipLeaderElection - defines the annotations given to the lease lock
	vipLeaseAnnotations = "vip_leaseannotations"

	// vipLogLevel - defines the level of logging to produce (5 being the most verbose)
	vipLogLevel = "vip_loglevel"

	// vipInterface - defines the interface that the vip should bind too
	vipInterface = "vip_interface"

	// vipServicesInterface - defines the interface that the service vips should bind too
	vipServicesInterface = "vip_servicesinterface"

	// vipCidr - defines the cidr that the vip will use (for BGP)
	vipCidr = "vip_cidr"

	// vipSubnet - defines the subnet that the vip will use
	vipSubnet = "vip_subnet"

	// egressPodCidr - defines the cidr that egress will ignore
	egressPodCidr = "egress_podcidr"

	// egressServiceCidr - defines the cidr that egress will ignore
	egressServiceCidr = "egress_servicecidr"

	// egressWithNftables - enables using nftables over iptables
	egressWithNftables = "egress_withnftables"

	/////////////////////////////////////
	// TO DO:
	// Determine how to tidy this mess up
	/////////////////////////////////////

	// vipAddress - defines the address that the vip will expose
	// DEPRECATED: will be removed in a next release
	vipAddress = "vip_address"

	// address - defines the address that would be used as a vip
	// it may be an IP or a DNS name, in case of a DNS name
	// kube-vip will try to resolve it and use the IP as a VIP
	address = "address"

	// port - defines the port for the VIP
	port = "port"

	// annotations
	annotations = "annotation"

	// vipDdns - defines if use dynamic dns to allocate IP for "address"
	vipDdns = "vip_ddns"

	// vipSingleNode - defines the vip start as a single node cluster
	vipSingleNode = "vip_singlenode"

	// vipStartLeader - will start this instance as the leader of the cluster
	vipStartLeader = "vip_startleader"

	// vipPacket defines that the packet API will be used for EIP
	vipPacket = "vip_packet"

	// vipPacketProject defines which project within Packet to use
	vipPacketProject = "vip_packetproject"

	// vipPacketProjectID defines which projectID within Packet to use
	vipPacketProjectID = "vip_packetprojectid"

	// providerConfig defines a path to a configuration that should be parsed
	providerConfig = "provider_config"

	// bgpEnable defines if BGP should be enabled
	bgpEnable = "bgp_enable"
	// bgpRouterID defines the routerID for the BGP server
	bgpRouterID = "bgp_routerid"
	// bgpRouterInterface defines the interface that we can find the address for
	bgpRouterInterface = "bgp_routerinterface"
	// bgpRouterAS defines the AS for the BGP server
	bgpRouterAS = "bgp_as"
	// bgpPeerAddress defines the address for a BGP peer
	bgpPeerAddress = "bgp_peeraddress"
	// bgpPeers defines the address for a BGP peer
	bgpPeers = "bgp_peers"
	// bgpPeerAS defines the AS for a BGP peer
	bgpPeerAS = "bgp_peeras"
	// bgpPeerAS defines the AS for a BGP peer
	bgpPeerPassword = "bgp_peerpass" // nolint
	// bgpMultiHop enables mulithop routing
	bgpMultiHop = "bgp_multihop"
	// bgpSourceIF defines the source interface for BGP peering
	bgpSourceIF = "bgp_sourceif"
	// bgpSourceIP defines the source address for BGP peering
	bgpSourceIP = "bgp_sourceip"

	// vipWireguard - defines if wireguard will be used for vips
	vipWireguard = "vip_wireguard" //nolint

	// vipRoutingTable - defines if table mode will be used for vips
	vipRoutingTable = "vip_routingtable" //nolint

	// vipRoutingTableID - defines which table mode will be used for vips
	vipRoutingTableID = "vip_routingtableid" //nolint

	// vipRoutingTableType - defines which table type will be used for vip routes
	// 						 valid values for this variable can be found in:
	//						 https://pkg.go.dev/golang.org/x/sys/unix#RTN_UNSPEC
	//						 Note that route type have the prefix `RTN_`, and you
	//						 specify the integer value, not the name. For example:
	//						 you should say `vip_routingtabletype=2` for RTN_LOCAL
	vipRoutingTableType = "vip_routingtabletype" //nolint

	// cpNamespace defines the namespace the control plane pods will run in
	cpNamespace = "cp_namespace"

	// cpEnable enables the control plane feature
	cpEnable = "cp_enable"

	// svcEnable enables the Kubernetes service feature
	svcEnable = "svc_enable"

	// svcNamespace defines the namespace the service pods will run in
	svcNamespace = "svc_namespace"

	// svcElection enables election per Kubernetes service
	svcElection = "svc_election"

	// lbClassOnly enables load-balancer for class "kube-vip.io/kube-vip-class" only
	lbClassOnly = "lb_class_only"

	// lbClassName enables load-balancer for a specific class only
	lbClassName = "lb_class_name"

	// lbEnable defines if the load-balancer should be enabled
	lbEnable = "lb_enable"

	// lbPort defines the port of load-balancer
	lbPort = "lb_port"

	// lbForwardingMethod defines the forwarding method of load-balancer
	lbForwardingMethod = "lb_fwdmethod"

	// EnableServiceSecurity defines if the load-balancer should only allow traffic to service ports
	EnableServiceSecurity = "enable_service_security"

	// prometheusServer defines the address prometheus listens on
	prometheusServer = "prometheus_server"

	// vipConfigMap defines the configmap that kube-vip will watch for service definitions
	// vipConfigMap = "vip_configmap"
)
