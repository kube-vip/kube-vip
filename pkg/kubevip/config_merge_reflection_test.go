package kubevip

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/kube-vip/kube-vip/pkg/debouncer"
	"github.com/kube-vip/kube-vip/pkg/detector"
)

// knownUnmergedFields records fields which the current hand-written merge does
// not copy from a file configuration. Each entry is a follow-up tracking note.
//
//nolint:gosec // map of Config field names to notes, not credentials
var knownUnmergedFields = map[string]string{
	"KubernetesAddr":                                "mergeConfigValues has no Kubernetes API address branch",
	"LoadBalancerClassOnly":                         "mergeConfigValues has no load balancer class-only branch",
	"LoadBalancerClassLegacyHandling":               "mergeConfigValues has no legacy class handling branch",
	"EnableServiceSecurity":                         "mergeConfigValues has no service security branch",
	"EnableUPNP":                                    "mergeConfigValues has no UPNP branch",
	"LoseLeadership":                                "mergeConfigValues has no leadership loss branch",
	"LeaderElectionType":                            "mergeConfigValues has no election backend branch",
	"KubernetesLeaderElection.EnableLeaderElection": "mergeLeaderElectionConfig does not copy the enable flag",
	"Etcd.CAFile":                                   "mergeConfigValues has no Etcd branch",
	"Etcd.ClientCertFile":                           "mergeConfigValues has no Etcd branch",
	"Etcd.ClientKeyFile":                            "mergeConfigValues has no Etcd branch",
	"Etcd.Endpoints":                                "mergeConfigValues has no Etcd branch",
	"AddPeersAsBackends":                            "mergeConfigValues has no RAFT backend branch",
	"AllowInterfaceNotUp":                           "mergeConfigValues has no interface tolerance branch",
	"CleanRoutingTable":                             "mergeConfigValues has no routing cleanup branch",
	"SkipDAD":                                       "mergeConfigValues has no DAD skip branch",
	"BGPConfig.MpbgpNexthop":                        "mergeBGPConfig has no MP-BGP nexthop branch",
	"BGPConfig.MpbgpIPv4":                           "mergeBGPConfig has no MP-BGP IPv4 branch",
	"BGPConfig.MpbgpIPv6":                           "mergeBGPConfig has no MP-BGP IPv6 branch",
	"BGPConfig.Zebra.Enabled":                       "mergeBGPConfig has no Zebra branch",
	"BGPConfig.Zebra.URL":                           "mergeBGPConfig has no Zebra branch",
	"BGPConfig.Zebra.Version":                       "mergeBGPConfig has no Zebra branch",
	"BGPConfig.Zebra.SoftwareName":                  "mergeBGPConfig has no Zebra branch",
	"BGPPeerConfig.Address":                         "mergeConfigValues has no standalone BGP peer branch",
	"BGPPeerConfig.Port":                            "mergeConfigValues has no standalone BGP peer branch",
	"BGPPeerConfig.Interface":                       "mergeConfigValues has no standalone BGP peer branch",
	"BGPPeerConfig.AS":                              "mergeConfigValues has no standalone BGP peer branch",
	"BGPPeerConfig.Password":                        "mergeConfigValues has no standalone BGP peer branch",
	"BGPPeerConfig.MultiHop":                        "mergeConfigValues has no standalone BGP peer branch",
	"BGPPeerConfig.MpbgpNexthop":                    "mergeConfigValues has no standalone BGP peer branch",
	"BGPPeerConfig.MpbgpIPv4":                       "mergeConfigValues has no standalone BGP peer branch",
	"BGPPeerConfig.MpbgpIPv6":                       "mergeConfigValues has no standalone BGP peer branch",
	"BGPPeerConfig.BFDEnabled":                      "mergeConfigValues has no standalone BGP peer branch",
	"BGPPeerConfig.BFDReceiveInterval":              "mergeConfigValues has no standalone BGP peer branch",
	"BGPPeerConfig.BFDTransmitInterval":             "mergeConfigValues has no standalone BGP peer branch",
	"BGPPeerConfig.BFDDetectMultiplier":             "mergeConfigValues has no standalone BGP peer branch",
	"BGPPeers":                                      "mergeConfigValues has no legacy BGP peers branch",
	"EnableInternalSNAT":                            "mergeConfigValues has no internal SNAT branch",
	"EgressWithNftables":                            "mergeConfigValues has no nftables egress branch",
	"PerServiceElectionOnDemand":                    "mergeConfigValues has no per-service election branch",
	"IsDualStack":                                   "mergeConfigValues has no derived dual-stack state branch",
	"RequireDualStack":                              "mergeConfigValues has no derived dual-stack requirement branch",
	"DisableServiceUpdates":                         "mergeConfigValues has no service update branch",
	"EnableEndpoints":                               "mergeConfigValues has no endpoints branch",
	"LoInterfaceGlobalScope":                        "mergeConfigValues has no loopback scope branch",
	"EgressClean":                                   "mergeConfigValues has no egress cleanup branch",
	"ConfigFile":                                    "mergeConfigValues has no nested config file branch",
}

// mergeReflectionBaseDefaults contains production defaults for fields whose
// merge branches compare against a non-zero value instead of the Go zero value.
// Add future non-zero-default fields here so the reflection test exercises the
// same merge condition as production.
var mergeReflectionBaseDefaults = map[string]any{
	"DHCPBackoffAttempts": uint(DefaultDHCPBackoffAttempts),
	"DebounceTime":        debouncer.DefaultTime,
}

type mergeReflectionField struct {
	path  string
	value reflect.Value
}

func TestMergeConfigCoversAllFields(t *testing.T) {
	t.Parallel()

	fields := collectMergeReflectionFields(t, reflect.TypeOf(Config{}), "")
	tested := make(map[string]struct{}, len(fields))

	for _, field := range fields {
		field := field
		tested[field.path] = struct{}{}
		t.Run(field.path, func(t *testing.T) {
			t.Parallel()

			baseConfig := &Config{}
			if defaultValue, ok := mergeReflectionBaseDefaults[field.path]; ok {
				defaultReflectValue := reflect.ValueOf(defaultValue)
				if defaultReflectValue.Type() != field.value.Type() {
					t.Fatalf("base default for %s has type %s, want %s; update mergeReflectionBaseDefaults", field.path, defaultReflectValue.Type(), field.value.Type())
				}
				setConfigField(baseConfig, field.path, defaultReflectValue)
			}
			fileConfig := &Config{}
			setConfigField(fileConfig, field.path, field.value)

			mergeConfigValues(baseConfig, fileConfig)
			got := configFieldValue(baseConfig, field.path)
			if reflect.DeepEqual(got.Interface(), field.value.Interface()) {
				if reason, ok := knownUnmergedFields[field.path]; ok {
					t.Fatalf("stale allowlist entry for %s: field now merges (%s); delete this knownUnmergedFields entry", field.path, reason)
				}
				return
			}

			if reason, ok := knownUnmergedFields[field.path]; ok {
				t.Logf("known unmerged field %s: %s; got %#v", field.path, reason, got.Interface())
				return
			}

			t.Fatalf("file value for %s was not preserved: got %#v, want %#v; add a merge branch in mergeConfigValues or add the field to knownUnmergedFields", field.path, got.Interface(), field.value.Interface())
		})
	}

	for path, reason := range knownUnmergedFields {
		if _, ok := tested[path]; !ok {
			t.Errorf("stale allowlist entry for %s: field was not exercised (%s); delete this knownUnmergedFields entry", path, reason)
		}
	}

	paths := make([]string, 0, len(knownUnmergedFields))
	for path := range knownUnmergedFields {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	t.Logf("tested %d reflected merge fields", len(fields))
	t.Logf("known unmerged fields (%d):", len(paths))
	for _, path := range paths {
		t.Logf("  %s", path)
	}
}

func collectMergeReflectionFields(t *testing.T, typ reflect.Type, prefix string) []mergeReflectionField {
	t.Helper()

	var fields []mergeReflectionField
	for i := 0; i < typ.NumField(); i++ {
		structField := typ.Field(i)
		if structField.PkgPath != "" {
			// Unexported fields cannot be set or compared through this test's
			// reflection helpers.
			continue
		}

		path := structField.Name
		if prefix != "" {
			path = prefix + "." + path
		}

		switch structField.Type.Kind() {
		case reflect.Struct:
			fields = append(fields, collectMergeReflectionFields(t, structField.Type, path)...)
		case reflect.Slice, reflect.Map:
			// The merge copies these fields as complete values. Populated values
			// exercise the slice or map branch and include nested struct fields.
			fields = append(fields, mergeReflectionField{
				path:  path,
				value: nonZeroMergeValue(t, structField.Type, path),
			})
		case reflect.Bool, reflect.String,
			reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
			reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			fields = append(fields, mergeReflectionField{
				path:  path,
				value: nonZeroMergeValue(t, structField.Type, path),
			})
		default:
			t.Fatalf("unsupported Config field %s of kind %s; add a reflection case for this kind or skip the field explicitly", path, structField.Type.Kind())
		}
	}
	return fields
}

func nonZeroMergeValue(t *testing.T, typ reflect.Type, path string) reflect.Value {
	t.Helper()

	value := reflect.New(typ).Elem()
	switch typ.Kind() {
	case reflect.Bool:
		value.SetBool(true)
	case reflect.String:
		value.SetString("merge-value:" + path)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		value.SetInt(17)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		value.SetUint(17)
	case reflect.Slice:
		value = reflect.MakeSlice(typ, 1, 1)
		value.Index(0).Set(nonZeroMergeValue(t, typ.Elem(), path+"[0]"))
	case reflect.Map:
		value = reflect.MakeMapWithSize(typ, 1)
		value.SetMapIndex(nonZeroMergeValue(t, typ.Key(), path+"[key]"), nonZeroMergeValue(t, typ.Elem(), path+"[value]"))
	case reflect.Struct:
		for i := 0; i < typ.NumField(); i++ {
			field := value.Field(i)
			if field.CanSet() {
				field.Set(nonZeroMergeValue(t, typ.Field(i).Type, path+"."+typ.Field(i).Name))
			}
		}
	default:
		t.Fatalf("unsupported Config value type %s of kind %s at %s; add a value-generation case for this kind or skip the field explicitly", typ, typ.Kind(), path)
	}
	return value
}

func setConfigField(config *Config, path string, value reflect.Value) {
	field := reflect.ValueOf(config).Elem()
	for _, name := range strings.Split(path, ".") {
		field = field.FieldByName(name)
	}
	field.Set(value)
}

func configFieldValue(config *Config, path string) reflect.Value {
	field := reflect.ValueOf(config).Elem()
	for _, name := range strings.Split(path, ".") {
		field = field.FieldByName(name)
	}
	return field
}

type environmentSetting struct {
	name  string
	value string
}

// parseEnvironmentEnvVarNames is the shared inventory of names read by
// ParseEnvironment. It drives both environment cleanup and the completeness
// check below. Keep canonical names in sync with the constants in
// config_envvar.go; the uppercase names are the supported aliases.
var parseEnvironmentEnvVarNames = []string{
	vipLogLevel, instanceName, vipInterface, vipInterfaceLoGlobal,
	vipLoseLeadership, vipLoseLeadershipTimeoutSeconds, vipServicesInterface,
	vipAllowInterfaceNotUp, vipLeaderElection, vipLeaseName, vipLeaseDuration,
	vipRenewDeadline, vipRetryPeriod, vipLeaseAnnotations, nodeName, vipAddress,
	address, port, vipDdns, cpNamespace, cpEnable, cpDetect, kubernetesAddr,
	svcEnable, svcElection, svcNamespace, svcLeaseName, lbClassOnly, lbClassName,
	lbClassLegacyHandling, vipSubnet, vipSingleNode, annotations, vipStartLeader,
	vipArp, vipArpRate, vipPreserveOnLeadershipLoss, vipWireguard, vipRoutingTable,
	vipRoutingTableID, vipRoutingTableType, vipRoutingProtocol, vipCleanRoutingTable,
	vipSkipDAD, dnsMode, dhcpMode, dhcpBackoffAttempts, disableServiceUpdates,
	bgpEnable, bgpAttachIPToInterface, bgpRouterInterface, bgpRouterID, bgpRouterAS,
	bgpPeerAddress, bgpPeers, bgpPeerAS, bgpPeerPassword, bgpMultiHop, bgpSourceIF,
	bgpSourceIP, bgpHoldTime, bgpKeepaliveInterval, controlPlaneHealthCheckAddress,
	controlPlaneHealthCheckPeriodSeconds, controlPlaneHealthCheckTimeoutSeconds,
	controlPlaneHealthCheckFailureThreshold, controlPlaneHealthCheckCAPath, zebraEnable,
	zebraURL, zebraVersion, zebraSoftwareName, mpbgpNexthop, mpbgpIPv4, mpbgpIPv6,
	lbEnable, lbPort, lbForwardingMethod, EnableServiceSecurity,
	EnableNodeLabeling, prometheusServer, egressPodCidr, egressServiceCidr,
	egressWithNftables, perServiceElectionOnDemand, egressEnableInternalSNAT,
	k8sConfigFile, enableEndpoints, mirrorDestInterface, iptablesBackend,
	backendHealthCheckInterval, healthCheckPort, enableUPNP, egressClean, configFile,
	strings.ToUpper(instanceName), strings.ToUpper(egressClean),
}

func TestParseEnvironmentRoundTrip(t *testing.T) {
	tests := []struct {
		name      string
		env       string
		value     string
		valueFunc func(*testing.T) string
		path      string
		want      any
		wantFunc  func(*testing.T, string) any
		setup     []environmentSetting
	}{
		{name: "logging", env: vipLogLevel, value: "5", path: "Logging", want: int32(5)},
		{name: "instance name", env: instanceName, value: "release-a", path: "InstanceName", want: "release-a"},
		{name: "uppercase instance name", env: strings.ToUpper(instanceName), value: "release-uppercase", path: "InstanceName", want: "release-uppercase"},
		{name: "interface", env: vipInterface, value: "eth9", path: "Interface", want: "eth9"},
		{name: "loopback global scope", env: vipInterfaceLoGlobal, value: "true", path: "LoInterfaceGlobalScope", want: true},
		{name: "lose leadership", env: vipLoseLeadership, value: "true", path: "LoseLeadership", want: true},
		{name: "lose leadership timeout", env: vipLoseLeadershipTimeoutSeconds, value: "45", path: "LoseLeadershipTimeoutSeconds", want: 45},
		{name: "services interface", env: vipServicesInterface, value: "eth8", path: "ServicesInterface", want: "eth8"},
		{name: "interface not up", env: vipAllowInterfaceNotUp, value: "true", path: "AllowInterfaceNotUp", want: true},
		{name: "leader election", env: vipLeaderElection, value: "true", path: "KubernetesLeaderElection.EnableLeaderElection", want: true},
		{name: "lease name", env: vipLeaseName, value: "lease-a", path: "KubernetesLeaderElection.LeaseName", want: "lease-a"},
		{name: "lease duration", env: vipLeaseDuration, value: "20", path: "KubernetesLeaderElection.LeaseDuration", want: 20},
		{name: "renew deadline", env: vipRenewDeadline, value: "12", path: "KubernetesLeaderElection.RenewDeadline", want: 12},
		{name: "retry period", env: vipRetryPeriod, value: "3", path: "KubernetesLeaderElection.RetryPeriod", want: 3},
		{name: "lease annotations", env: vipLeaseAnnotations, value: `{"team":"network"}`, path: "KubernetesLeaderElection.LeaseAnnotations", want: map[string]string{"team": "network"}},
		{name: "node name", env: nodeName, value: "node-a", path: "NodeName", want: "node-a"},
		{name: "vip address", env: vipAddress, value: "192.0.2.10", path: "VIP", want: "192.0.2.10"},
		{name: "address", env: address, value: "vip.example.test", path: "Address", want: "vip.example.test"},
		{name: "port", env: port, value: "6443", path: "Port", want: uint16(6443)},
		{name: "ddns", env: vipDdns, value: "true", path: "DDNS", want: true},
		{name: "namespace", env: cpNamespace, value: "control-plane", path: "Namespace", want: "control-plane"},
		{name: "control plane enable", env: cpEnable, value: "true", path: "EnableControlPlane", want: true},
		{name: "control plane detect", env: cpDetect, value: "true", path: "DetectControlPlane", want: true},
		{name: "kubernetes address", env: kubernetesAddr, value: "https://192.0.2.20:6443", path: "KubernetesAddr", want: "https://192.0.2.20:6443"},
		{name: "services enable", env: svcEnable, value: "true", path: "EnableServices", want: true},
		{name: "services election", env: svcElection, value: "true", path: "EnableServicesElection", want: true, setup: []environmentSetting{{name: svcEnable, value: "true"}}},
		{name: "service namespace", env: svcNamespace, value: "services", path: "ServiceNamespace", want: "services", setup: []environmentSetting{{name: svcEnable, value: "true"}}},
		{name: "services lease name", env: svcLeaseName, value: "services-lease", path: "ServicesLeaseName", want: "services-lease", setup: []environmentSetting{{name: svcEnable, value: "true"}}},
		{name: "load balancer class only", env: lbClassOnly, value: "true", path: "LoadBalancerClassOnly", want: true, setup: []environmentSetting{{name: svcEnable, value: "true"}}},
		{name: "load balancer class name", env: lbClassName, value: "class-a", path: "LoadBalancerClassName", want: "class-a", setup: []environmentSetting{{name: svcEnable, value: "true"}}},
		{name: "legacy class handling", env: lbClassLegacyHandling, value: "true", path: "LoadBalancerClassLegacyHandling", want: true, setup: []environmentSetting{{name: svcEnable, value: "true"}}},
		{name: "vip subnet", env: vipSubnet, value: "24", path: "VIPSubnet", want: "24"},
		{name: "single node", env: vipSingleNode, value: "true", path: "SingleNode", want: true},
		{name: "annotations", env: annotations, value: "kube-vip.io", path: "Annotations", want: "kube-vip.io"},
		{name: "start as leader", env: vipStartLeader, value: "true", path: "StartAsLeader", want: true},
		{name: "arp", env: vipArp, value: "true", path: "EnableARP", want: true},
		{name: "arp rate", env: vipArpRate, value: "1234", path: "ArpBroadcastRate", want: int64(1234)},
		{name: "preserve vip", env: vipPreserveOnLeadershipLoss, value: "true", path: "PreserveVIPOnLeadershipLoss", want: true},
		{name: "wireguard", env: vipWireguard, value: "true", path: "EnableWireguard", want: true},
		{name: "routing table", env: vipRoutingTable, value: "true", path: "EnableRoutingTable", want: true},
		{name: "routing table id", env: vipRoutingTableID, value: "198", path: "RoutingTableID", want: 198},
		{name: "routing table type", env: vipRoutingTableType, value: "2", path: "RoutingTableType", want: 2},
		{name: "routing protocol", env: vipRoutingProtocol, value: "248", path: "RoutingProtocol", want: 248},
		{name: "clean routing table", env: vipCleanRoutingTable, value: "true", path: "CleanRoutingTable", want: true},
		{name: "skip dad", env: vipSkipDAD, value: "true", path: "SkipDAD", want: true},
		{name: "dns mode", env: dnsMode, value: "dual", path: "DNSMode", want: "dual"},
		{name: "dhcp mode", env: dhcpMode, value: "ipv6", path: "DHCPMode", want: "ipv6"},
		{name: "dhcp backoff attempts", env: dhcpBackoffAttempts, value: "4", path: "DHCPBackoffAttempts", want: uint(4)},
		{name: "disable service updates", env: disableServiceUpdates, value: "true", path: "DisableServiceUpdates", want: true},
		{name: "bgp enable", env: bgpEnable, value: "true", path: "EnableBGP", want: true},
		{name: "bgp attach ip", env: bgpAttachIPToInterface, value: "true", path: "BGPAttachIPToInterface", want: true},
		{name: "bgp router interface", env: bgpRouterInterface, valueFunc: testBGPInterface, path: "BGPConfig.RouterID", wantFunc: testBGPRouterID},
		{name: "bgp router id", env: bgpRouterID, value: "192.0.2.1", path: "BGPConfig.RouterID", want: "192.0.2.1"},
		{name: "bgp router as", env: bgpRouterAS, value: "65000", path: "BGPConfig.AS", want: uint32(65000)},
		{name: "bgp peer as", env: bgpPeerAS, value: "65001", path: "BGPPeerConfig.AS", want: uint32(65001)},
		{name: "bgp peers", env: bgpPeers, value: "192.0.2.2:65001", path: "BGPConfig.Peers", want: []BGPPeer{{Address: "192.0.2.2", Port: 179, AS: 65001, BFDReceiveInterval: 300, BFDTransmitInterval: 300, BFDDetectMultiplier: 3}}},
		{name: "mpbgp nexthop", env: mpbgpNexthop, value: "auto_sourceif", path: "BGPConfig.MpbgpNexthop", want: "auto_sourceif"},
		{name: "mpbgp ipv4", env: mpbgpIPv4, value: "192.0.2.3", path: "BGPConfig.MpbgpIPv4", want: "192.0.2.3"},
		{name: "mpbgp ipv6", env: mpbgpIPv6, value: "2001:db8::3", path: "BGPConfig.MpbgpIPv6", want: "2001:db8::3"},
		{name: "bgp multihop", env: bgpMultiHop, value: "true", path: "BGPPeerConfig.MultiHop", want: true},
		{name: "bgp peer password", env: bgpPeerPassword, value: "secret", path: "BGPPeerConfig.Password", want: "secret"},
		{name: "bgp source interface", env: bgpSourceIF, value: "eth7", path: "BGPConfig.SourceIF", want: "eth7"},
		{name: "bgp source ip", env: bgpSourceIP, value: "192.0.2.4", path: "BGPConfig.SourceIP", want: "192.0.2.4"},
		{name: "bgp peer address", env: bgpPeerAddress, value: "192.0.2.5", path: "BGPPeerConfig.Address", want: "192.0.2.5"},
		{name: "bgp hold time", env: bgpHoldTime, value: "30", path: "BGPConfig.HoldTime", want: uint64(30)},
		{name: "bgp keepalive", env: bgpKeepaliveInterval, value: "10", path: "BGPConfig.KeepaliveInterval", want: uint64(10)},
		{name: "health check address", env: controlPlaneHealthCheckAddress, value: "http://192.0.2.6:8080/healthz", path: "ControlPlaneHealthCheck.Address", want: "http://192.0.2.6:8080/healthz"},
		{name: "health check period", env: controlPlaneHealthCheckPeriodSeconds, value: "5", path: "ControlPlaneHealthCheck.PeriodSeconds", want: 5},
		{name: "health check timeout", env: controlPlaneHealthCheckTimeoutSeconds, value: "3", path: "ControlPlaneHealthCheck.TimeoutSeconds", want: 3},
		{name: "health check threshold", env: controlPlaneHealthCheckFailureThreshold, value: "2", path: "ControlPlaneHealthCheck.FailureThreshold", want: 2},
		{name: "health check ca path", env: controlPlaneHealthCheckCAPath, value: "/etc/kube-vip/ca.crt", path: "ControlPlaneHealthCheck.CAPath", want: "/etc/kube-vip/ca.crt"},
		{name: "zebra enable", env: zebraEnable, value: "true", path: "BGPConfig.Zebra.Enabled", want: true},
		{name: "zebra url", env: zebraURL, value: "unix:/run/zserv.api", path: "BGPConfig.Zebra.URL", want: "unix:/run/zserv.api"},
		{name: "zebra version", env: zebraVersion, value: "6", path: "BGPConfig.Zebra.Version", want: uint32(6)},
		{name: "zebra software", env: zebraSoftwareName, value: "frr", path: "BGPConfig.Zebra.SoftwareName", want: "frr"},
		{name: "load balancer enable", env: lbEnable, value: "true", path: "EnableLoadBalancer", want: true},
		{name: "load balancer port", env: lbPort, value: "8443", path: "LoadBalancerPort", want: uint16(8443)},
		{name: "load balancer forwarding", env: lbForwardingMethod, value: "local", path: "LoadBalancerForwardingMethod", want: "local"},
		{name: "service security", env: EnableServiceSecurity, value: "true", path: "EnableServiceSecurity", want: true},
		{name: "node labeling", env: EnableNodeLabeling, value: "true", path: "EnableNodeLabeling", want: true},
		{name: "prometheus", env: prometheusServer, value: ":2112", path: "PrometheusHTTPServer", want: ":2112"},
		{name: "egress pod cidr", env: egressPodCidr, value: "10.244.0.0/16", path: "EgressPodCidr", want: "10.244.0.0/16"},
		{name: "egress service cidr", env: egressServiceCidr, value: "10.96.0.0/12", path: "EgressServiceCidr", want: "10.96.0.0/12"},
		{name: "egress nftables", env: egressWithNftables, value: "true", path: "EgressWithNftables", want: true},
		{name: "per-service election", env: perServiceElectionOnDemand, value: "true", path: "PerServiceElectionOnDemand", want: true},
		{name: "internal snat", env: egressEnableInternalSNAT, value: "true", path: "EnableInternalSNAT", want: true},
		{name: "kubernetes config file", env: k8sConfigFile, value: "/etc/kubernetes/admin.conf", path: "K8sConfigFile", want: "/etc/kubernetes/admin.conf"},
		{name: "endpoints", env: enableEndpoints, value: "true", path: "EnableEndpoints", want: true},
		{name: "mirror destination", env: mirrorDestInterface, value: "eth6", path: "MirrorDestInterface", want: "eth6"},
		{name: "iptables backend", env: iptablesBackend, value: "nft", path: "IptablesBackend", want: "nft"},
		{name: "backend health interval", env: backendHealthCheckInterval, value: "15", path: "BackendHealthCheckInterval", want: 15},
		{name: "health check port", env: healthCheckPort, value: "1024", path: "HealthCheckPort", want: 1024},
		{name: "upnp", env: enableUPNP, value: "true", path: "EnableUPNP", want: true},
		{name: "egress clean", env: egressClean, value: "true", path: "EgressClean", want: true},
		{name: "uppercase egress clean", env: strings.ToUpper(egressClean), value: "true", path: "EgressClean", want: true},
		{name: "config file", env: configFile, value: "/etc/kube-vip/config.yaml", path: "ConfigFile", want: "/etc/kube-vip/config.yaml"},
	}

	sharedEnvVars := make(map[string]struct{}, len(parseEnvironmentEnvVarNames))
	for _, env := range parseEnvironmentEnvVarNames {
		if _, ok := sharedEnvVars[env]; ok {
			t.Errorf("environment variable %q appears more than once in parseEnvironmentEnvVarNames; remove the duplicate", env)
		}
		sharedEnvVars[env] = struct{}{}
	}
	testedEnvVars := make(map[string]struct{}, len(tests))
	for _, tc := range tests {
		if _, ok := sharedEnvVars[tc.env]; !ok {
			t.Errorf("round-trip case %q uses environment variable %q outside parseEnvironmentEnvVarNames; add it to the shared list", tc.name, tc.env)
		}
		testedEnvVars[tc.env] = struct{}{}
	}
	for _, env := range parseEnvironmentEnvVarNames {
		if _, ok := testedEnvVars[env]; !ok {
			t.Errorf("environment variable %q is cleared but has no round-trip test; add a TestParseEnvironmentRoundTrip case", env)
		}
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			clearParseEnvironmentVariables(t)
			for _, setting := range tc.setup {
				t.Setenv(setting.name, setting.value)
			}
			value := tc.value
			if tc.valueFunc != nil {
				value = tc.valueFunc(t)
			}
			t.Setenv(tc.env, value)

			config := &Config{}
			if err := ParseEnvironment(config); err != nil {
				t.Fatalf("ParseEnvironment() error = %v; fix ParseEnvironment or adjust this test case's environment values", err)
			}

			got := configFieldValue(config, tc.path)
			want := tc.want
			if tc.wantFunc != nil {
				want = tc.wantFunc(t, value)
			}
			if !reflect.DeepEqual(got.Interface(), want) {
				t.Fatalf("%s = %#v, want %#v; update ParseEnvironment or this test case's expected value", tc.path, got.Interface(), want)
			}
		})
	}
}

func testBGPInterface(t *testing.T) string {
	t.Helper()

	interfaceName, _, err := detector.FindIPAddress("")
	if err != nil {
		t.Skipf("cannot test %s without a non-loopback interface: %v", bgpRouterInterface, err)
	}
	return interfaceName
}

func testBGPRouterID(t *testing.T, interfaceName string) any {
	t.Helper()

	_, address, err := detector.FindIPAddress(interfaceName)
	if err != nil {
		t.Fatalf("cannot resolve the test BGP interface %q: %v; use a non-loopback interface for this test", interfaceName, err)
	}
	return address
}

func clearParseEnvironmentVariables(t *testing.T) {
	for _, name := range parseEnvironmentEnvVarNames {
		t.Setenv(name, "")
	}
}
