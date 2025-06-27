package manager

const (

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

	// Use internal implementation of egress
	egressInternal = "kube-vip.io/egress-internal"

	// The current active endpoint(pod) for the Egress VIP
	activeEndpoint = "kube-vip.io/active-endpoint"

	// The current active endpoint(pod) for the Egress VIP (v6)
	activeEndpointIPv6 = "kube-vip.io/active-endpoint-ipv6"

	// Flush the conntrack rules (remove existing sessions) once Egress is configured
	flushContrack = "kube-vip.io/flush-conntrack"

	upnpEnabled = "kube-vip.io/forwardUPNP"
)
