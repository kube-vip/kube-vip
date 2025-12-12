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

	// Configure LoadBalancer IPs instead of relying on a controller
	LoadbalancerIPAnnotation = "kube-vip.io/loadbalancerIPs"

	// Ignore the LoadBalancer Service
	LoadbalancerIgnore = "kube-vip.io/ignore"

	// Used to configure DHCP with a Hostname
	LoadbalancerHostname = "kube-vip.io/loadbalancerHostname"

	// Define an interface name to bind the address of the LoadBalancer to
	ServiceInterface = "kube-vip.io/serviceInterface"

	ServiceSecurityIgnore = "kube-vip.io/ignore-service-security"

	// Enable UPNP on a Service
	UpnpEnabled = "kube-vip.io/forwardUPNP"

	// Set the UPNP lease duration for a specific service using duration format (e.g., "30s", "1h")
	UpnpLeaseDuration = "kube-vip.io/upnp-lease-duration"

	RPFilter = "kube-vip.io/rp_filter" // Set the return path filter for a specific service interface

	ServiceLease = "kube-vip.io/leaseName"
)
