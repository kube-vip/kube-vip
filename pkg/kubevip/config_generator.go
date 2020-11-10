package kubevip

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/ghodss/yaml"
	appv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

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

	//vipAddress - defines the address that the vip will expose
	// DEPRECATED: will be removed in a next release
	vipAddress = "vip_address"

	//vipCidr - defines the cidr that the vip will use
	vipCidr = "vip_cidr"

	//address - defines the address that would be used as a vip
	// it may be an IP or a DNS name, in case of a DNS name
	// kube-vip will try to resolve it and use the IP as a VIP
	address = "address"

	//port - defines the port for the VIP
	port = "port"

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

	//vipPacket defines that the packet API will be used tor EIP
	vipPacket = "vip_packet"

	//vipPacket defines which project within Packet to use
	vipPacketProject = "vip_packetproject"

	//bgpEnable defines if BGP should be enabled
	bgpEnable = "bgp_enable"
	//bgpRouterID defines the routerID for the BGP server
	bgpRouterID = "bgp_routerid"
	//bgpRouterAS defines the AS for the BGP server
	bgpRouterAS = "bgp_as"
	//bgpPeerAddress defines the address for a BGP peer
	bgpPeerAddress = "bgp_peeraddress"
	//bgpPeerAS defines the AS for a BGP peer
	bgpPeerAS = "bgp_peeras"

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

// ParseEnvironment - will popultate the configuration from environment variables
func ParseEnvironment(c *Config) error {

	// Find interface
	env := os.Getenv(vipInterface)
	if env != "" {
		c.Interface = env
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

	// Attempt to find the Lease configuration from teh environment variables
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

	// Find vip address
	env = os.Getenv(vipAddress)
	if env != "" {
		// TODO - parse address net.Host()
		c.VIP = env
	} else {
		c.Address = os.Getenv(address)
	}

	// Find vip port
	env = os.Getenv(port)
	if env != "" {
		i, err := strconv.ParseInt(env, 10, 32)
		if err != nil {
			return err
		}
		c.Port = int(i)
	}

	// Find vip address cidr range
	env = os.Getenv(vipCidr)
	if env != "" {
		// TODO - parse address net.Host()
		c.VIPCIDR = env
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

	// Find Start As Leader
	// TODO - does this need depricating?
	// Required when the host sets itself as leader before the state change
	env = os.Getenv(vipStartLeader)
	if env != "" {
		b, err := strconv.ParseBool(env)
		if err != nil {
			return err
		}
		c.StartAsLeader = b
	}

	// Find ARP
	env = os.Getenv(vipArp)
	if env != "" {
		b, err := strconv.ParseBool(env)
		if err != nil {
			return err
		}
		c.EnableARP = b
	}

	//Removal of seperate peer
	env = os.Getenv(vipLocalPeer)
	if env != "" {
		// Parse the string in format <id>:<address>:<port>
		peer, err := ParsePeerConfig(env)
		if err != nil {
			return err
		}
		c.LocalPeer = *peer
	}

	env = os.Getenv(vipPeers)
	if env != "" {
		// TODO - perhaps make this optional?
		// Remove existing peers
		c.RemotePeers = []RaftPeer{}

		// Parse the remote peers (comma seperated)
		s := strings.Split(env, ",")
		if len(s) == 0 {
			return fmt.Errorf("The Remote Peer List [%s] is unable to be parsed, should be in comma seperated format <id>:<address>:<port>", env)
		}
		for x := range s {
			// Parse the each remote peer string in format <id>:<address>:<port>
			peer, err := ParsePeerConfig(s[x])
			if err != nil {
				return err
			}

			c.RemotePeers = append(c.RemotePeers, *peer)

		}
	}

	// Find Add Peers as Backends
	env = os.Getenv(vipAddPeersToLB)
	if env != "" {
		b, err := strconv.ParseBool(env)
		if err != nil {
			return err
		}
		c.AddPeersAsBackends = b
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

	// BGP Peer options
	env = os.Getenv(bgpPeerAddress)
	if env != "" {
		c.BGPPeerConfig.Address = env
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

	// Enable the Packet API calls
	env = os.Getenv(vipPacket)
	if env != "" {
		b, err := strconv.ParseBool(env)
		if err != nil {
			return err
		}
		c.EnablePacket = b
	}

	// Find the Packet project name
	env = os.Getenv(vipPacketProject)
	if env != "" {
		// TODO - parse address net.Host()
		c.PacketProject = env
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

	// Load Balancer configuration
	return parseEnvironmentLoadBalancer(c)
}

func parseEnvironmentLoadBalancer(c *Config) error {
	// Check if an existing load-balancer configuration already exists
	if len(c.LoadBalancers) == 0 {
		c.LoadBalancers = append(c.LoadBalancers, LoadBalancer{})
	}

	// Find LoadBalancer Port
	env := os.Getenv(lbPort)
	if env != "" {
		i, err := strconv.ParseInt(env, 10, 32)
		if err != nil {
			return err
		}
		c.LoadBalancers[0].Port = int(i)
	}

	// Find Type of LoadBalancer
	env = os.Getenv(lbType)
	if env != "" {
		c.LoadBalancers[0].Type = env
	}

	// Find Type of LoadBalancer Name
	env = os.Getenv(lbName)
	if env != "" {
		c.LoadBalancers[0].Name = env
	}

	// Find If LB should bind to Vip
	env = os.Getenv(lbBindToVip)
	if env != "" {
		b, err := strconv.ParseBool(env)
		if err != nil {
			return err
		}
		c.LoadBalancers[0].BindToVip = b
	}

	// Find global backendport
	env = os.Getenv(lbBackendPort)
	if env != "" {
		i, err := strconv.ParseInt(env, 10, 32)
		if err != nil {
			return err
		}
		c.LoadBalancers[0].BackendPort = int(i)
	}

	// Parse backends
	env = os.Getenv(lbBackends)
	if env != "" {
		// TODO - perhaps make this optional?
		// Remove existing backends
		c.LoadBalancers[0].Backends = []BackEnd{}

		// Parse the remote peers (comma seperated)
		s := strings.Split(env, ",")
		if len(s) == 0 {
			return fmt.Errorf("The Backends List [%s] is unable to be parsed, should be in comma seperated format <address>:<port>", env)
		}
		for x := range s {
			// Parse the each remote peer string in format <address>:<port>

			be, err := ParseBackendConfig(s[x])
			if err != nil {
				return err
			}

			c.LoadBalancers[0].Backends = append(c.LoadBalancers[0].Backends, *be)

		}
	}
	return nil
}

// generatePodSpec will take a kube-vip config and generate a Pod spec
func generatePodSpec(c *Config, imageVersion string) *corev1.Pod {

	// build environment variables
	newEnvironment := []corev1.EnvVar{
		{
			Name:  vipArp,
			Value: strconv.FormatBool(c.EnableARP),
		},
		{
			Name:  vipInterface,
			Value: c.Interface,
		},
	}

	// If a CIDR is used add it to the manifest
	if c.VIPCIDR != "" {
		// build environment variables
		cidr := []corev1.EnvVar{
			{
				Name:  vipCidr,
				Value: c.VIPCIDR,
			},
		}
		newEnvironment = append(newEnvironment, cidr...)

	}

	// If Leader election is enabled then add the configuration to the manifest
	if c.EnableLeaderElection {
		// Generate Kubernetes Leader Election configuration
		leaderElection := []corev1.EnvVar{
			{
				Name:  vipLeaderElection,
				Value: strconv.FormatBool(c.EnableLeaderElection),
			},
			{
				Name:  vipLeaseDuration,
				Value: fmt.Sprintf("%d", c.LeaseDuration),
			},
			{
				Name:  vipRenewDeadline,
				Value: fmt.Sprintf("%d", c.RenewDeadline),
			},
			{
				Name:  vipRetryPeriod,
				Value: fmt.Sprintf("%d", c.RetryPeriod),
			},
		}

		// TODO - (for upgrade purposes), we'll set Kubernetes defaults if they're not already set
		// (https://github.com/kubernetes/client-go/blob/f0b431a6e0bfce3c7c1d10b223d46875df3d1c29/tools/leaderelection/leaderelection.go#L128)
		if c.LeaseDuration == 0 {
			c.LeaseDuration = 15
		}

		if c.RenewDeadline == 0 {
			c.RenewDeadline = 10
		}

		if c.RetryPeriod == 0 {
			c.RetryPeriod = 2
		}

		newEnvironment = append(newEnvironment, leaderElection...)
	} else {
		// Generate Raft configuration
		raft := []corev1.EnvVar{
			{
				Name:  vipStartLeader,
				Value: strconv.FormatBool(c.StartAsLeader),
			},
			{
				Name:  vipAddPeersToLB,
				Value: strconv.FormatBool(c.AddPeersAsBackends),
			},
			{
				Name:  vipLocalPeer,
				Value: fmt.Sprintf("%s:%s:%d", c.LocalPeer.ID, c.LocalPeer.Address, c.LocalPeer.Port),
			},
		}
		newEnvironment = append(newEnvironment, raft...)
	}

	// If Packet is enabled then add it to the manifest
	if c.EnablePacket {
		packet := []corev1.EnvVar{
			{
				Name:  vipPacket,
				Value: strconv.FormatBool(c.EnablePacket),
			},
			{
				Name:  vipPacketProject,
				Value: c.PacketProject,
			},
			{
				Name:  "PACKET_AUTH_TOKEN",
				Value: c.PacketAPIKey,
			},
		}
		newEnvironment = append(newEnvironment, packet...)

	}

	// If BGP is enabled then add it to the manifest
	if c.EnableBGP {
		bgp := []corev1.EnvVar{
			{
				Name:  bgpEnable,
				Value: strconv.FormatBool(c.EnableBGP),
			},
			{
				Name:  bgpRouterID,
				Value: c.BGPConfig.RouterID,
			},
			{
				Name:  bgpRouterAS,
				Value: fmt.Sprintf("%d", c.BGPConfig.AS),
			},
			{
				Name:  bgpPeerAddress,
				Value: c.BGPPeerConfig.Address,
			},
			{
				Name:  bgpPeerAS,
				Value: fmt.Sprintf("%d", c.BGPPeerConfig.AS),
			},
		}
		newEnvironment = append(newEnvironment, bgp...)

	}

	// If the load-balancer is enabled then add the configuration to the manifest
	if c.EnableLoadBalancer {
		lb := []corev1.EnvVar{
			{
				Name:  lbEnable,
				Value: strconv.FormatBool(c.EnableLoadBalancer),
			},
			{
				Name:  lbBackendPort,
				Value: fmt.Sprintf("%d", c.LoadBalancers[0].Port),
			},
			{
				Name:  lbName,
				Value: c.LoadBalancers[0].Name,
			},
			{
				Name:  lbType,
				Value: c.LoadBalancers[0].Type,
			},
			{
				Name:  lbBindToVip,
				Value: strconv.FormatBool(c.LoadBalancers[0].BindToVip),
			},
		}

		newEnvironment = append(newEnvironment, lb...)
	}

	if c.Address != "" {
		newEnvironment = append(newEnvironment, corev1.EnvVar{
			Name:  address,
			Value: c.Address,
		})
	} else {
		newEnvironment = append(newEnvironment, corev1.EnvVar{
			Name:  vipAddress,
			Value: c.VIP,
		})
	}

	// Parse peers into a comma seperated string
	if len(c.RemotePeers) != 0 {
		var peers string
		for x := range c.RemotePeers {
			if x != 0 {
				peers = fmt.Sprintf("%s,%s:%s:%d", peers, c.RemotePeers[x].ID, c.RemotePeers[x].Address, c.RemotePeers[x].Port)

			} else {
				peers = fmt.Sprintf("%s:%s:%d", c.RemotePeers[x].ID, c.RemotePeers[x].Address, c.RemotePeers[x].Port)

			}
			//peers = fmt.Sprintf("%s,%s:%s:%d", peers, c.RemotePeers[x].ID, c.RemotePeers[x].Address, c.RemotePeers[x].Port)
			//fmt.Sprintf("", peers)
		}
		peerEnvirontment := corev1.EnvVar{
			Name:  vipPeers,
			Value: peers,
		}
		newEnvironment = append(newEnvironment, peerEnvirontment)
	}

	newManifest := &corev1.Pod{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kube-vip",
			Namespace: "kube-system",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:            "kube-vip",
					Image:           fmt.Sprintf("plndr/kube-vip:%s", imageVersion),
					ImagePullPolicy: corev1.PullAlways,
					SecurityContext: &corev1.SecurityContext{
						Capabilities: &corev1.Capabilities{
							Add: []corev1.Capability{
								"NET_ADMIN",
								"SYS_TIME",
							},
						},
					},
					Args: []string{
						"start",
					},
					Env: newEnvironment,
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "kubeconfig",
							MountPath: "/etc/kubernetes/admin.conf",
						},
						{
							Name:      "ca-certs",
							MountPath: "/etc/ssl/certs",
							ReadOnly:  true,
						},
					},
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: "kubeconfig",
					VolumeSource: corev1.VolumeSource{
						HostPath: &corev1.HostPathVolumeSource{
							Path: "/etc/kubernetes/admin.conf",
						},
					},
				},
				{
					Name: "ca-certs",
					VolumeSource: corev1.VolumeSource{
						HostPath: &corev1.HostPathVolumeSource{
							Path: "/etc/ssl/certs",
						},
					},
				},
			},
			HostNetwork: true,
		},
	}
	return newManifest

}

// GeneratePodManifestFromConfig will take a kube-vip config and generate a manifest
func GeneratePodManifestFromConfig(c *Config, imageVersion string) string {
	newManifest := generatePodSpec(c, imageVersion)
	b, _ := yaml.Marshal(newManifest)
	return string(b)
}

// GenerateDeamonsetManifestFromConfig will take a kube-vip config and generate a manifest
func GenerateDeamonsetManifestFromConfig(c *Config, imageVersion string) string {

	podSpec := generatePodSpec(c, imageVersion).Spec
	newManifest := &appv1.DaemonSet{
		TypeMeta: metav1.TypeMeta{
			Kind:       "DaemonSet",
			APIVersion: "apps/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kube-vip-ds",
			Namespace: "kube-system",
		},
		Spec: appv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"name": "kube-vip-ds",
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"name": "kube-vip-ds",
					},
				},
				Spec: podSpec,
			},
		},
	}

	newManifest.Spec.Template.Spec.Tolerations = []corev1.Toleration{
		{
			Key:    "node-role.kubernetes.io/master",
			Effect: corev1.TaintEffectNoSchedule,
		},
	}
	newManifest.Spec.Template.Spec.NodeSelector = map[string]string{
		"node-role.kubernetes.io/master": "true",
	}

	b, _ := yaml.Marshal(newManifest)
	return string(b)
}
