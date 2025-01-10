package manager

const (
	// Hardware address of the host that has the VIP
	hwAddrKey = "kube-vip.io/hwaddr"

	// The IP address that is requested
	requestedIP = "kube-vip.io/requestedIP"

	// The host that has the VIP
	vipHost = "kube-vip.io/vipHost"

	// Enable egress on a service
	egress = "kube-vip.io/egress"

	// Egress should be IPv6
	egressIPv6 = "kube-vip.io/egress-ipv6"

	// Ports that traffic is allowed to access from the egress VIP
	egressDestinationPorts = "kube-vip.io/egress-destination-ports"

	// Allowed incoming ports to the VIP
	egressSourcePorts = "kube-vip.io/egress-source-ports"

	// Allowed networks for the Egress to be enabled for
	egressAllowedNetworks = "kube-vip.io/egress-allowed-networks"

	// Networks that we wont Egress for
	egressDeniedNetworks = "kube-vip.io/egress-denied-networks"

	// The current active endpoint(pod) for the Egress VIP
	activeEndpoint = "kube-vip.io/active-endpoint"

	// The current active endpoint(pod) for the Egress VIP (v6)
	activeEndpointIPv6 = "kube-vip.io/active-endpoint-ipv6"

	// Flush the conntrack rules (remove existing sessions) once Egress is configured
	flushContrack = "kube-vip.io/flush-conntrack"

	loadbalancerIPAnnotation = "kube-vip.io/loadbalancerIPs"
	loadbalancerHostname     = "kube-vip.io/loadbalancerHostname"
	serviceInterface         = "kube-vip.io/serviceInterface"
	upnpEnabled              = "kube-vip.io/forwardUPNP"
)
