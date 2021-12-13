package kubevip

import (
	"fmt"
	"os"
	"strconv"

	"github.com/ghodss/yaml"
	"github.com/kube-vip/kube-vip/pkg/bgp"
	"github.com/kube-vip/kube-vip/pkg/detector"
	log "github.com/sirupsen/logrus"
	appv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ParseEnvironment - will popultate the configuration from environment variables
func ParseEnvironment(c *Config) error {

	// Ensure that logging is set through the environment variables
	env := os.Getenv(vipLogLevel)
	if env != "" {
		logLevel, err := strconv.Atoi(env)
		if err != nil {
			panic("Unable to parse environment variable [vip_loglevel], should be int")
		}
		log.SetLevel(log.Level(logLevel))
	}

	// Find interface
	env = os.Getenv(vipInterface)
	if env != "" {
		c.Interface = env
	}

	// Find (services) interface
	env = os.Getenv(vipServicesInterface)
	if env != "" {
		c.ServicesInterface = env
	}

	// Find provider configuration
	env = os.Getenv(providerConfig)
	if env != "" {
		c.ProviderConfig = env
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

	// Attempt to find the Lease configuration from the environment variables
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
		// } else {
		// 	c.VIP = os.Getenv(address)
	}

	// Find address
	env = os.Getenv(address)
	if env != "" {
		// TODO - parse address net.Host()
		c.Address = env
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

	// Find vipDdns
	env = os.Getenv(vipDdns)
	if env != "" {
		b, err := strconv.ParseBool(env)
		if err != nil {
			return err
		}
		c.DDNS = b
	}

	// Find vip address cidr range
	env = os.Getenv(cpNamespace)
	if env != "" {
		c.Namespace = env
	}

	// Find the namespace that the control pane should use (for leaderElection lock)
	env = os.Getenv(cpNamespace)
	if env != "" {
		c.Namespace = env
	}

	// Find controlplane toggle
	env = os.Getenv(cpEnable)
	if env != "" {
		b, err := strconv.ParseBool(env)
		if err != nil {
			return err
		}
		c.EnableControlPane = b
	}

	// Find Services toggle
	env = os.Getenv(svcEnable)
	if env != "" {
		b, err := strconv.ParseBool(env)
		if err != nil {
			return err
		}
		c.EnableServices = b
	}

	// Find vip address cidr range
	env = os.Getenv(vipCidr)
	if env != "" {
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

	// Find annotation configuration
	env = os.Getenv(annotations)
	if env != "" {
		c.Annotations = env
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

	// BGP Server options
	env = os.Getenv(bgpEnable)
	if env != "" {
		b, err := strconv.ParseBool(env)
		if err != nil {
			return err
		}
		c.EnableBGP = b
	}

	// BGP Router interface determines an interface that we can use to find an address for
	env = os.Getenv(bgpRouterInterface)
	if env != "" {
		_, address, err := detector.FindIPAddress(env)
		if err != nil {
			return err
		}
		c.BGPConfig.RouterID = address
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

	// Peer AS
	env = os.Getenv(bgpPeerAS)
	if env != "" {
		u64, err := strconv.ParseUint(env, 10, 32)
		if err != nil {
			return err
		}
		c.BGPPeerConfig.AS = uint32(u64)
	}

	// Peer AS
	env = os.Getenv(bgpPeers)
	if env != "" {
		peers, err := bgp.ParseBGPPeerConfig(env)
		if err != nil {
			return err
		}
		c.BGPConfig.Peers = peers
	}

	// BGP Peer mutlihop
	env = os.Getenv(bgpMultiHop)
	if env != "" {
		b, err := strconv.ParseBool(env)
		if err != nil {
			return err
		}
		c.BGPPeerConfig.MultiHop = b
	}

	// BGP Peer password
	env = os.Getenv(bgpPeerPassword)
	if env != "" {
		c.BGPPeerConfig.Password = env
	}

	// BGP Source Interface
	env = os.Getenv(bgpSourceIF)
	if env != "" {
		c.BGPConfig.SourceIF = env
	}

	// BGP Source Address
	env = os.Getenv(bgpSourceIP)
	if env != "" {
		c.BGPConfig.SourceIP = env
	}

	// BGP Peer options, add them if relevant
	env = os.Getenv(bgpPeerAddress)
	if env != "" {
		c.BGPPeerConfig.Address = env
		// If we've added in a peer configuration, then we should add it to the BGP configuration
		c.BGPConfig.Peers = append(c.BGPConfig.Peers, c.BGPPeerConfig)
	}

	// Enable the Packet API calls
	env = os.Getenv(vipPacket)
	if env != "" {
		b, err := strconv.ParseBool(env)
		if err != nil {
			return err
		}
		c.EnableMetal = b
	}

	// Find the Packet project name
	env = os.Getenv(vipPacketProject)
	if env != "" {
		// TODO - parse address net.Host()
		c.MetalProject = env
	}

	// Find the Packet project ID
	env = os.Getenv(vipPacketProjectID)
	if env != "" {
		// TODO - parse address net.Host()
		c.MetalProjectID = env
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

	// Find loadbalancer port
	env = os.Getenv(lbPort)
	if env != "" {
		i, err := strconv.ParseInt(env, 10, 32)
		if err != nil {
			return err
		}
		c.LoadBalancerPort = int(i)
	}

	// Find loadbalancer forwarding method
	env = os.Getenv(lbForwardingMethod)
	if env != "" {
		c.LoadBalancerForwardingMethod = env
	}
	return nil
}

// generatePodSpec will take a kube-vip config and generate a Pod spec
func generatePodSpec(c *Config, imageVersion string, inCluster bool) *corev1.Pod {
	command := "manager"

	// build environment variables
	newEnvironment := []corev1.EnvVar{
		{
			Name:  vipArp,
			Value: strconv.FormatBool(c.EnableARP),
		},
		{
			Name:  port,
			Value: fmt.Sprintf("%d", c.Port),
		},
	}

	// If we're specifically saying which interface to use then add it to the manifest
	if c.Interface != "" {
		iface := []corev1.EnvVar{
			{
				Name:  vipInterface,
				Value: c.Interface,
			},
		}
		newEnvironment = append(newEnvironment, iface...)
	}

	// Detect if we should be using a separate interface for sercices
	if c.ServicesInterface != "" {
		// build environment variables
		svcInterface := []corev1.EnvVar{
			{
				Name:  vipServicesInterface,
				Value: c.ServicesInterface,
			},
		}
		newEnvironment = append(newEnvironment, svcInterface...)
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

	// If we're doing the hybrid mode
	if c.EnableControlPane {
		cp := []corev1.EnvVar{
			{
				Name:  cpEnable,
				Value: strconv.FormatBool(c.EnableControlPane),
			},
			{
				Name:  cpNamespace,
				Value: c.Namespace,
			},
			{
				Name:  vipDdns,
				Value: strconv.FormatBool(c.DDNS),
			},
		}
		newEnvironment = append(newEnvironment, cp...)
	}

	// If we're doing the hybrid mode
	if c.EnableServices {
		cp := []corev1.EnvVar{
			{
				Name:  svcEnable,
				Value: strconv.FormatBool(c.EnableServices),
			},
		}
		newEnvironment = append(newEnvironment, cp...)
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
	}

	// If we're specifying an annotation configuration
	if c.Annotations != "" {
		annotations := []corev1.EnvVar{
			{
				Name:  annotations,
				Value: c.Annotations,
			},
		}
		newEnvironment = append(newEnvironment, annotations...)

	}

	// If we're specifying a configuration
	if c.ProviderConfig != "" {
		provider := []corev1.EnvVar{
			{
				Name:  providerConfig,
				Value: c.ProviderConfig,
			},
		}
		newEnvironment = append(newEnvironment, provider...)
	}

	// If Packet is enabled then add it to the manifest
	if c.EnableMetal {
		packet := []corev1.EnvVar{
			{
				Name:  vipPacket,
				Value: strconv.FormatBool(c.EnableMetal),
			},
			{
				Name:  vipPacketProject,
				Value: c.MetalProject,
			},
			{
				Name:  vipPacketProjectID,
				Value: c.MetalProjectID,
			},
			{
				Name:  "PACKET_AUTH_TOKEN",
				Value: c.MetalAPIKey,
			},
		}
		newEnvironment = append(newEnvironment, packet...)
	}

	// If BGP, but we're not using packet
	if c.EnableBGP {
		bgp := []corev1.EnvVar{
			{
				Name:  bgpEnable,
				Value: strconv.FormatBool(c.EnableBGP),
			},
		}
		newEnvironment = append(newEnvironment, bgp...)
	}
	// If BGP, but we're not using packet
	if c.EnableBGP && !c.EnableMetal {
		bgpConfig := []corev1.EnvVar{
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
				Name:  bgpPeerPassword,
				Value: c.BGPPeerConfig.Password,
			},
			{
				Name:  bgpPeerAS,
				Value: fmt.Sprintf("%d", c.BGPPeerConfig.AS),
			},
		}

		// Detect if we should be using a source interface for speaking to a bgp peer
		if c.BGPConfig.SourceIF != "" {
			bgpConfig = append(bgpConfig, corev1.EnvVar{
				Name:  bgpSourceIF,
				Value: c.BGPConfig.SourceIF,
			},
			)
		}
		// Detect if we should be using a source address for speaking to a bgp peer

		if c.BGPConfig.SourceIP != "" {
			bgpConfig = append(bgpConfig, corev1.EnvVar{
				Name:  bgpSourceIP,
				Value: c.BGPConfig.SourceIP,
			},
			)
		}

		var peers string
		if len(c.BGPPeers) != 0 {
			for x := range c.BGPPeers {
				if x != 0 {
					peers = fmt.Sprintf("%s,%s", peers, c.BGPPeers[x])
				} else {
					peers = c.BGPPeers[x]
				}
			}
			bgpConfig = append(bgpConfig, corev1.EnvVar{
				Name:  bgpPeers,
				Value: peers,
			},
			)
		}

		newEnvironment = append(newEnvironment, bgpConfig...)

	}

	// If the load-balancer is enabled then add the configuration to the manifest
	if c.EnableLoadBalancer {
		lb := []corev1.EnvVar{
			{
				Name:  lbEnable,
				Value: strconv.FormatBool(c.EnableLoadBalancer),
			},
			{
				Name:  lbPort,
				Value: fmt.Sprintf("%d", c.LoadBalancerPort),
			},
			{
				Name:  lbForwardingMethod,
				Value: c.LoadBalancerForwardingMethod,
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
					Image:           fmt.Sprintf("ghcr.io/kube-vip/kube-vip:%s", imageVersion),
					ImagePullPolicy: corev1.PullAlways,
					SecurityContext: &corev1.SecurityContext{
						Capabilities: &corev1.Capabilities{
							Add: []corev1.Capability{
								"NET_ADMIN",
								"NET_RAW",
								"SYS_TIME",
							},
						},
					},
					Args: []string{
						command,
					},
					Env: newEnvironment,
				},
			},
			HostNetwork: true,
		},
	}

	if inCluster {
		// If we're running this inCluster then the acccount name will be required
		newManifest.Spec.ServiceAccountName = "kube-vip"
	} else {
		// If this isn't inside a cluster then add the external path mount
		adminConfMount := corev1.VolumeMount{
			Name:      "kubeconfig",
			MountPath: "/etc/kubernetes/admin.conf",
		}
		newManifest.Spec.Containers[0].VolumeMounts = append(newManifest.Spec.Containers[0].VolumeMounts, adminConfMount)
		adminConfVolume := corev1.Volume{
			Name: "kubeconfig",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: "/etc/kubernetes/admin.conf",
				},
			},
		}
		newManifest.Spec.Volumes = append(newManifest.Spec.Volumes, adminConfVolume)
		// Add Host modification

		hostAlias := corev1.HostAlias{
			IP:        "127.0.0.1",
			Hostnames: []string{"kubernetes"},
		}
		newManifest.Spec.HostAliases = append(newManifest.Spec.HostAliases, hostAlias)
	}

	if c.ProviderConfig != "" {
		providerConfigMount := corev1.VolumeMount{
			Name:      "cloud-sa-volume",
			MountPath: "/etc/cloud-sa",
			ReadOnly:  true,
		}
		newManifest.Spec.Containers[0].VolumeMounts = append(newManifest.Spec.Containers[0].VolumeMounts, providerConfigMount)

		providerConfigVolume := corev1.Volume{
			Name: "cloud-sa-volume",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: "metal-cloud-config",
				},
			},
		}
		newManifest.Spec.Volumes = append(newManifest.Spec.Volumes, providerConfigVolume)

	}

	return newManifest

}

// GeneratePodManifestFromConfig will take a kube-vip config and generate a manifest
func GeneratePodManifestFromConfig(c *Config, imageVersion string, inCluster bool) string {
	newManifest := generatePodSpec(c, imageVersion, inCluster)
	b, _ := yaml.Marshal(newManifest)
	return string(b)
}

// GenerateDeamonsetManifestFromConfig will take a kube-vip config and generate a manifest
func GenerateDeamonsetManifestFromConfig(c *Config, imageVersion string, inCluster, taint bool) string {

	podSpec := generatePodSpec(c, imageVersion, inCluster).Spec
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
	if taint {
		newManifest.Spec.Template.Spec.Tolerations = []corev1.Toleration{
			{
				Effect:   corev1.TaintEffectNoSchedule,
				Operator: corev1.TolerationOpExists,
			},
			{
				Effect:   corev1.TaintEffectNoExecute,
				Operator: corev1.TolerationOpExists,
			},
		}
		newManifest.Spec.Template.Spec.Affinity = &corev1.Affinity{
			NodeAffinity: &corev1.NodeAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
					NodeSelectorTerms: []corev1.NodeSelectorTerm{
						{
							MatchExpressions: []corev1.NodeSelectorRequirement{
								{
									Key:      "node-role.kubernetes.io/master",
									Operator: corev1.NodeSelectorOpExists,
								},
							},
						},
						{
							MatchExpressions: []corev1.NodeSelectorRequirement{
								{
									Key:      "node-role.kubernetes.io/control-plane",
									Operator: corev1.NodeSelectorOpExists,
								},
							},
						},
					},
				},
			},
		}
	}
	b, _ := yaml.Marshal(newManifest)
	return string(b)
}
