package kubevip

import (
	"fmt"
	"strconv"

	appv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	applyCoreV1 "k8s.io/client-go/applyconfigurations/core/v1"
	applyMetaV1 "k8s.io/client-go/applyconfigurations/meta/v1"
	applyRbacV1 "k8s.io/client-go/applyconfigurations/rbac/v1"

	"sigs.k8s.io/yaml"
)

// GenerateSA will create the service account for kube-vip
func GenerateSA() *applyCoreV1.ServiceAccountApplyConfiguration {
	kind := "ServiceAccount"
	name := "kube-vip"
	namespace := "kube-system"
	newManifest := &applyCoreV1.ServiceAccountApplyConfiguration{
		TypeMetaApplyConfiguration: applyMetaV1.TypeMetaApplyConfiguration{APIVersion: &corev1.SchemeGroupVersion.Version, Kind: &kind},
		ObjectMetaApplyConfiguration: &applyMetaV1.ObjectMetaApplyConfiguration{
			Name:      &name,
			Namespace: &namespace,
		},
	}
	return newManifest
}

// GenerateCR will generate the Cluster role for kube-vip
func GenerateCR() *applyRbacV1.ClusterRoleApplyConfiguration {
	name := "system:kube-vip-role"
	roleRefKind := "ClusterRole"
	apiVersion := "rbac.authorization.k8s.io/v1"

	newManifest := &applyRbacV1.ClusterRoleApplyConfiguration{
		TypeMetaApplyConfiguration: applyMetaV1.TypeMetaApplyConfiguration{APIVersion: &apiVersion, Kind: &roleRefKind},
		ObjectMetaApplyConfiguration: &applyMetaV1.ObjectMetaApplyConfiguration{
			Name: &name,
		},
		Rules: []applyRbacV1.PolicyRuleApplyConfiguration{
			{
				APIGroups: []string{""},
				Resources: []string{"services/status"},
				Verbs:     []string{"update"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"services", "endpoints"},
				Verbs:     []string{"list", "get", "watch", "endoints"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"nodes"},
				Verbs:     []string{"list", "get", "watch", "update", "patch"},
			},
			{
				APIGroups: []string{"coordination.k8s.io"},
				Resources: []string{"leases"},
				Verbs:     []string{"list", "get", "watch", "update", "create"},
			},
		},
	}
	return newManifest
}

// GenerateCRB will generate the clusterRoleBinding
func GenerateCRB() *applyRbacV1.ClusterRoleBindingApplyConfiguration {
	kind := "ClusterRoleBinding"
	apiVersion := "rbac.authorization.k8s.io/v1"
	subjectKind := "ServiceAccount"
	apiGroup := "rbac.authorization.k8s.io"
	roleRefKind := "ClusterRole"
	roleRefName := "system:kube-vip-role"
	name := "kube-vip"
	bindName := "system:kube-vip-role-binding"
	namespace := "kube-system"

	newManifest := &applyRbacV1.ClusterRoleBindingApplyConfiguration{
		TypeMetaApplyConfiguration: applyMetaV1.TypeMetaApplyConfiguration{APIVersion: &apiVersion, Kind: &kind},
		ObjectMetaApplyConfiguration: &applyMetaV1.ObjectMetaApplyConfiguration{
			Name: &bindName,
		},
		RoleRef: &applyRbacV1.RoleRefApplyConfiguration{
			APIGroup: &apiGroup,
			Kind:     &roleRefKind,
			Name:     &roleRefName,
		},
		Subjects: []applyRbacV1.SubjectApplyConfiguration{
			{
				Kind:      &subjectKind,
				Name:      &name,
				Namespace: &namespace,
			},
		},
	}
	return newManifest
}

// generatePodSpec will take a kube-vip config and generate a Pod spec
func generatePodSpec(c *Config, imageVersion string, inCluster bool) *corev1.Pod {
	command := "manager"

	// Determine where the pods should be living (for multi-tenancy)
	var namespace string
	if c.ServiceNamespace != "" {
		namespace = c.ServiceNamespace
	} else {
		namespace = metav1.NamespaceSystem
	}

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

	// If a subnet is required for the VIP
	if c.VIPSubnet != "" {
		// build environment variables
		cidr := []corev1.EnvVar{
			{
				Name:  vipSubnet,
				Value: c.VIPSubnet,
			},
		}
		newEnvironment = append(newEnvironment, cidr...)
	}

	if c.DNSMode != "" {
		// build environment variables
		dnsModeSelector := []corev1.EnvVar{
			{
				Name:  dnsMode,
				Value: c.DNSMode,
			},
		}
		newEnvironment = append(newEnvironment, dnsModeSelector...)
	}

	// If we're doing the hybrid mode
	if c.EnableControlPlane {
		cp := []corev1.EnvVar{
			{
				Name:  cpEnable,
				Value: strconv.FormatBool(c.EnableControlPlane),
			},
			{
				Name:  cpNamespace,
				Value: c.Namespace,
			},
		}
		if c.DDNS {
			cp = append(cp, corev1.EnvVar{
				Name:  vipDdns,
				Value: strconv.FormatBool(c.DDNS),
			})
		}
		if c.DetectControlPlane {
			cp = append(cp, corev1.EnvVar{
				Name:  cpDetect,
				Value: strconv.FormatBool(c.DetectControlPlane),
			})
		}
		newEnvironment = append(newEnvironment, cp...)
	}

	// If we're doing the hybrid mode
	if c.EnableServices {
		svc := []corev1.EnvVar{
			{
				Name:  svcEnable,
				Value: strconv.FormatBool(c.EnableServices),
			},
			{
				Name:  svcLeaseName,
				Value: c.ServicesLeaseName,
			},
		}
		newEnvironment = append(newEnvironment, svc...)
		if c.EnableServicesElection {
			svcElection := []corev1.EnvVar{
				{
					Name:  svcElection,
					Value: strconv.FormatBool(c.EnableServicesElection),
				},
			}
			newEnvironment = append(newEnvironment, svcElection...)
		}
		if c.LoadBalancerClassOnly {
			lbClassOnlyVar := []corev1.EnvVar{
				{
					Name:  lbClassOnly,
					Value: strconv.FormatBool(c.LoadBalancerClassOnly),
				},
			}
			newEnvironment = append(newEnvironment, lbClassOnlyVar...)
		}
		if c.EnableServiceSecurity {
			EnableServiceSecurityVar := []corev1.EnvVar{
				{
					Name:  EnableServiceSecurity,
					Value: strconv.FormatBool(c.EnableServiceSecurity),
				},
			}
			newEnvironment = append(newEnvironment, EnableServiceSecurityVar...)
		}
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
				Name:  vipLeaseName,
				Value: c.LeaseName,
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

	// If we're enabling node labeling on leader election
	if c.EnableNodeLabeling {
		EnableNodeLabeling := []corev1.EnvVar{
			{
				Name:  EnableNodeLabeling,
				Value: strconv.FormatBool(c.EnableNodeLabeling),
			},
		}
		newEnvironment = append(newEnvironment, EnableNodeLabeling...)
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

	// If Equinix Metal is enabled then add it to the manifest
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

	// Detect and enable wireguard mode
	if c.EnableWireguard {
		wireguard := []corev1.EnvVar{
			{
				Name:  vipWireguard,
				Value: strconv.FormatBool(c.EnableWireguard),
			},
		}
		newEnvironment = append(newEnvironment, wireguard...)
	}

	// Detect and enable routing table mode
	if c.EnableRoutingTable {
		routingtable := []corev1.EnvVar{
			{
				Name:  vipRoutingTable,
				Value: strconv.FormatBool(c.EnableRoutingTable),
			},
		}
		newEnvironment = append(newEnvironment, routingtable...)
	}

	// If BGP, but we're not using Equinix Metal
	if c.EnableBGP {
		bgp := []corev1.EnvVar{
			{
				Name:  bgpEnable,
				Value: strconv.FormatBool(c.EnableBGP),
			},
		}
		newEnvironment = append(newEnvironment, bgp...)
	}
	// If BGP, but we're not using Equinix Metal
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

	if c.PrometheusHTTPServer != "" {
		prometheus := []corev1.EnvVar{
			{
				Name:  prometheusServer,
				Value: c.PrometheusHTTPServer,
			},
		}
		newEnvironment = append(newEnvironment, prometheus...)
	}

	if c.EnableEndpointSlices {
		newEnvironment = append(newEnvironment, corev1.EnvVar{
			Name:  enableEndpointSlices,
			Value: strconv.FormatBool(c.EnableEndpointSlices),
		})
	}

	if c.DisableServiceUpdates {
		// Disable service updates
		disServiceUpdates := []corev1.EnvVar{
			{
				Name:  disableServiceUpdates,
				Value: strconv.FormatBool(c.DisableServiceUpdates),
			},
		}
		newEnvironment = append(newEnvironment, disServiceUpdates...)
	}

	var securityContext *corev1.SecurityContext
	if c.LoadBalancerForwardingMethod == "masquerade" {
		var privileged = true
		securityContext = &corev1.SecurityContext{
			Privileged: &privileged,
		}
	} else {
		securityContext = &corev1.SecurityContext{
			Capabilities: &corev1.Capabilities{
				Add: []corev1.Capability{
					"NET_ADMIN",
					"NET_RAW",
				},
			},
		}
	}

	newManifest := &corev1.Pod{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kube-vip",
			Namespace: namespace,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:            "kube-vip",
					Image:           fmt.Sprintf("ghcr.io/kube-vip/kube-vip:%s", imageVersion),
					ImagePullPolicy: corev1.PullIfNotPresent,
					SecurityContext: securityContext,
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
		// If we're running this inCluster then the account name will be required
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
					Path: c.K8sConfigFile,
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

// GenerateDaemonsetManifestFromConfig will take a kube-vip config and generate a manifest
func GenerateDaemonsetManifestFromConfig(c *Config, imageVersion string, inCluster, taint bool) string {
	// Determine where the pod should be deployed
	var namespace string
	if c.ServiceNamespace != "" {
		namespace = c.ServiceNamespace
	} else {
		namespace = metav1.NamespaceSystem
	}

	podSpec := generatePodSpec(c, imageVersion, inCluster).Spec
	newManifest := &appv1.DaemonSet{
		TypeMeta: metav1.TypeMeta{
			Kind:       "DaemonSet",
			APIVersion: "apps/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kube-vip-ds",
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":    "kube-vip-ds",
				"app.kubernetes.io/version": imageVersion,
			},
		},
		Spec: appv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app.kubernetes.io/name": "kube-vip-ds",
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app.kubernetes.io/name":    "kube-vip-ds",
						"app.kubernetes.io/version": imageVersion,
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
