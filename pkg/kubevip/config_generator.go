package kubevip

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/ghodss/yaml"
	appv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Environment variables
const (

	//vipArp - defines if the arp broadcast should be enabled
	vipArp = "vip_arp"

	//vipLeaderElection - defines if the kubernetes algorithim should be used
	vipLeaderElection = "vip_leaderelection"

	//vipLogLevel - defines the level of logging to produce (5 being the most verbose)
	vipLogLevel = "vip_loglevel"

	//vipInterface - defines the interface that the vip should bind too
	vipInterface = "vip_interface"

	//vipAddress - defines the address that the vip will expose
	vipAddress = "vip_address"

	//vipCidr - defines the cidr that the vip will use
	vipCidr = "vip_cidr"

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

	// Find Single Node
	env = os.Getenv(vipLeaderElection)
	if env != "" {
		b, err := strconv.ParseBool(env)
		if err != nil {
			return err
		}
		c.EnableLeaderElection = b
	}

	// Find vip address
	env = os.Getenv(vipAddress)
	if env != "" {
		// TODO - parse address net.Host()
		c.VIP = env
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
		c.GratuitousARP = b
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
		i, err := strconv.ParseInt(env, 8, 0)
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
		i, err := strconv.ParseInt(env, 8, 0)
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

// GenerateManifestFromConfig will take a kube-vip config and generate a manifest
func GenerateManifestFromConfig(c *Config, imageVersion string) string {

	// build environment variables
	newEnvironment := []appv1.EnvVar{
		{
			Name:  vipArp,
			Value: strconv.FormatBool(c.GratuitousARP),
		},
		{
			Name:  vipLeaderElection,
			Value: strconv.FormatBool(c.EnableLeaderElection),
		},
		{
			Name:  vipInterface,
			Value: c.Interface,
		},
		{
			Name:  vipAddress,
			Value: c.VIP,
		},
	}

	// If a CIDR is used add it to the manifest
	if c.VIPCIDR != "" {
		// build environment variables
		cidr := []appv1.EnvVar{
			{
				Name:  vipCidr,
				Value: c.VIPCIDR,
			},
		}
		newEnvironment = append(newEnvironment, cidr...)

	}

	// If Leader election is enabled then add the configuration to the manifest
	if !c.EnableLeaderElection {
		raft := []appv1.EnvVar{
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
		packet := []appv1.EnvVar{
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
		bgp := []appv1.EnvVar{
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
		lb := []appv1.EnvVar{
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
		peerEnvirontment := appv1.EnvVar{
			Name:  vipPeers,
			Value: peers,
		}
		newEnvironment = append(newEnvironment, peerEnvirontment)
	}

	newManifest := &appv1.Pod{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kube-vip",
			Namespace: "kube-system",
		},
		Spec: appv1.PodSpec{
			Containers: []appv1.Container{
				{
					Name:            "kube-vip",
					Image:           fmt.Sprintf("plndr/kube-vip:%s", imageVersion),
					ImagePullPolicy: appv1.PullAlways,
					SecurityContext: &appv1.SecurityContext{
						Capabilities: &appv1.Capabilities{
							Add: []appv1.Capability{
								"NET_ADMIN",
								"SYS_TIME",
							},
						},
					},
					Args: []string{
						"start",
					},
					Env: newEnvironment,
					VolumeMounts: []appv1.VolumeMount{
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
			Volumes: []appv1.Volume{
				{
					Name: "kubeconfig",
					VolumeSource: appv1.VolumeSource{
						HostPath: &appv1.HostPathVolumeSource{
							Path: "/etc/kubernetes/admin.conf",
						},
					},
				},
				{
					Name: "ca-certs",
					VolumeSource: appv1.VolumeSource{
						HostPath: &appv1.HostPathVolumeSource{
							Path: "/etc/ssl/certs",
						},
					},
				},
			},
			HostNetwork: true,
		},
	}

	b, _ := yaml.Marshal(newManifest)
	return string(b)
}
