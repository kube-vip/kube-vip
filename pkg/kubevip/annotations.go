package kubevip

const (
	// Hardware address of the host that has the VIP
	HwAddrKey = "kube-vip.io/hwaddr"

	// The IP address that is requested
	RequestedIP = "kube-vip.io/requestedIP"

	// The host that has the VIP
	VipHost = "kube-vip.io/vipHost"

	// Enable Egress on a service
	Egress = "kube-vip.io/egress"

	// Enable internal Egress
	EgressInternal = "kube-vip.io/egress-internal"

	// Egress should be IPv6
	EgressIPv6 = "kube-vip.io/egress-ipv6"

	// Ports that traffic is allowed to access from the egress VIP
	EgressDestinationPorts = "kube-vip.io/egress-destination-ports"

	// Allowed incoming ports to the VIP
	EgressSourcePorts = "kube-vip.io/egress-source-ports"

	// Allowed networks for the Egress to be enabled for
	EgressAllowedNetworks = "kube-vip.io/egress-allowed-networks"

	// Networks that we wont Egress for
	EgressDeniedNetworks = "kube-vip.io/egress-denied-networks"

	// The current active endpoint(pod) for the Egress VIP
	ActiveEndpoint = "kube-vip.io/active-endpoint"

	// The current active endpoint(pod) for the Egress VIP (v6)
	ActiveEndpointIPv6 = "kube-vip.io/active-endpoint-ipv6"

	// Flush the conntrack rules (remove existing sessions) once Egress is configured
	FlushContrack = "kube-vip.io/flush-conntrack"

	LoadbalancerIPAnnotation = "kube-vip.io/loadbalancerIPs"
	LoadbalancerHostname     = "kube-vip.io/loadbalancerHostname"
	ServiceInterface         = "kube-vip.io/serviceInterface"
	UpnpEnabled              = "kube-vip.io/forwardUPNP"
)
