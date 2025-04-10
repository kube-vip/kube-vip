package kubevip

import (
	"fmt"
	"log"
	"strconv"

	appv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	applyCoreV1 "k8s.io/client-go/applyconfigurations/core/v1"
	applyMetaV1 "k8s.io/client-go/applyconfigurations/meta/v1"
	applyRbacV1 "k8s.io/client-go/applyconfigurations/rbac/v1"

	"sigs.k8s.io/yaml"
)

// TransformApplyObjectToManifest transforms an apply object into a normal Kubernetes manifest
func TransformApplyObjectToManifest(applyObject interface{}) string {
	// Convert the apply object to an unstructured object
	unstructuredObj := &unstructured.Unstructured{}
	err := runtime.DefaultUnstructuredConverter.FromUnstructured(applyConfigToMap(applyObject), unstructuredObj)
	if err != nil {
		log.Fatalf("Error converting apply object to unstructured: %v", err)
	}

	// Marshal the unstructured object into YAML
	yamlData, err := yaml.Marshal(unstructuredObj.Object)
	if err != nil {
		log.Fatalf("Error marshaling unstructured object to YAML: %v", err)
	}

	return string(yamlData)
}

// Helper function to convert apply configuration to a map
func applyConfigToMap(applyConfig interface{}) map[string]interface{} {
	data, err := runtime.DefaultUnstructuredConverter.ToUnstructured(applyConfig)
	if err != nil {
		log.Fatalf("Error converting apply configuration to map: %v", err)
	}
	return data
}

// GenerateSA will create the service account for kube-vip
func GenerateSA(c *Config) *applyCoreV1.ServiceAccountApplyConfiguration {
	kind := "ServiceAccount"
	name := "kube-vip"
	var namespace string
	if c.ServiceNamespace != "" {
		namespace = c.ServiceNamespace
	} else {
		namespace = metav1.NamespaceSystem
	}
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
func GenerateRole(c *Config, role bool) *applyRbacV1.RoleApplyConfiguration {
	var kind, name string
	var namespace *string
	if role {
		kind = "Role"
		name = "kube-vip"
		if c.ServiceNamespace != "" {
			namespace = &c.ServiceNamespace
		} else {
			// If the namespace is empty then we need to set it to the system namespace
			copiedNamespace := metav1.NamespaceSystem
			namespace = &copiedNamespace
		}

	} else {
		kind = "ClusterRole"
		name = "system:kube-vip-role"

	}
	apiVersion := "rbac.authorization.k8s.io/v1"
	newManifest := &applyRbacV1.RoleApplyConfiguration{
		TypeMetaApplyConfiguration: applyMetaV1.TypeMetaApplyConfiguration{APIVersion: &apiVersion, Kind: &kind},
		ObjectMetaApplyConfiguration: &applyMetaV1.ObjectMetaApplyConfiguration{
			Name:      &name,
			Namespace: namespace,
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
			{
				APIGroups: []string{"discovery.k8s.io"},
				Resources: []string{"endpointslices"},
				Verbs:     []string{"list", "get", "watch", "update"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"pods"},
				Verbs:     []string{"list"},
			},
		},
	}
	return newManifest
}

// GenerateCRB will generate the clusterRoleBinding or rolebinding
func GenerateRoleBinding(rolebinding bool, saCfg *applyCoreV1.ServiceAccountApplyConfiguration, crCfg *applyRbacV1.RoleApplyConfiguration) *applyRbacV1.RoleBindingApplyConfiguration {
	apiVersion := "rbac.authorization.k8s.io/v1"
	apiGroup := "rbac.authorization.k8s.io"
	var kind, bindName string
	var namespace, objectNamespace *string
	if rolebinding {
		kind = "RoleBinding"
		bindName = "kube-vip"
		namespace = nil
		objectNamespace = saCfg.Namespace
	} else {
		kind = "ClusterRoleBinding"
		bindName = "system:kube-vip-binding"
		namespace = saCfg.Namespace
		objectNamespace = nil
	}
	newManifest := &applyRbacV1.RoleBindingApplyConfiguration{
		TypeMetaApplyConfiguration: applyMetaV1.TypeMetaApplyConfiguration{APIVersion: &apiVersion, Kind: &kind},
		ObjectMetaApplyConfiguration: &applyMetaV1.ObjectMetaApplyConfiguration{
			Name:      &bindName,
			Namespace: objectNamespace,
		},
		RoleRef: &applyRbacV1.RoleRefApplyConfiguration{
			APIGroup: &apiGroup,
			Kind:     crCfg.Kind,
			Name:     crCfg.Name,
		},
		Subjects: []applyRbacV1.SubjectApplyConfiguration{
			{
				Kind:      saCfg.Kind,
				Name:      saCfg.Name,
				Namespace: namespace,
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
		{
			Name: nodeName,
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					FieldPath: "spec.nodeName",
				},
			},
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
		// specify if global scope should be set when using the lo interface
		if c.LoInterfaceGlobalScope {
			iface = append(iface, corev1.EnvVar{
				Name:  vipInterfaceLoGlobal,
				Value: strconv.FormatBool(c.LoInterfaceGlobalScope),
			})
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
	// If BGP
	if c.EnableBGP {
		bgp := []corev1.EnvVar{
			{
				Name:  bgpEnable,
				Value: strconv.FormatBool(c.EnableBGP),
			},
		}
		newEnvironment = append(newEnvironment, bgp...)
	}

	// If BGP
	if c.EnableBGP {
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

	prometheus := []corev1.EnvVar{
		{
			Name:  prometheusServer,
			Value: c.PrometheusHTTPServer,
		},
	}
	newEnvironment = append(newEnvironment, prometheus...)

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

	if c.MirrorDestInterface != "" {
		mdif := []corev1.EnvVar{
			{
				Name:  mirrorDestInterface,
				Value: c.MirrorDestInterface,
			},
		}
		newEnvironment = append(newEnvironment, mdif...)
	}

	if c.HealthCheckPort != 0 {
		healthPort := []corev1.EnvVar{
			{
				Name:  healthCheckPort,
				Value: fmt.Sprintf("%d", c.HealthCheckPort),
			},
		}
		newEnvironment = append(newEnvironment, healthPort...)
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
				Drop: []corev1.Capability{
					"ALL",
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

	// TODO: we don't check error return values for any of these marshall/unmarshall functions
	b, _ := yaml.Marshal(newManifest)

	// This additional step is required to be able to delete a section of the manifest being generated
	m := make(map[string]interface{})
	_ = yaml.Unmarshal(b, &m)
	delete(m, "status")

	b, _ = yaml.Marshal(m)
	return string(b)
}
