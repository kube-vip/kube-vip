package kubevip

import (
	"maps"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/kube-vip/kube-vip/pkg/metrics"
	applyRbacV1 "k8s.io/client-go/applyconfigurations/rbac/v1"
)

func TestGenerateRoleServiceCIDRAccess(t *testing.T) {
	clusterRole := GenerateRole(&Config{}, false)
	if !hasServiceCIDRRule(clusterRole) {
		t.Fatal("generated ClusterRole is missing ServiceCIDR access")
	}

	role := GenerateRole(&Config{ServiceNamespace: "kube-vip"}, true)
	if hasServiceCIDRRule(role) {
		t.Fatal("generated namespaced Role contains ineffective ServiceCIDR access")
	}
}

func hasServiceCIDRRule(role *applyRbacV1.RoleApplyConfiguration) bool {
	for _, rule := range role.Rules {
		if slices.Contains(rule.APIGroups, "networking.k8s.io") &&
			slices.Contains(rule.Resources, "servicecidrs") &&
			slices.Contains(rule.Verbs, "get") &&
			slices.Contains(rule.Verbs, "list") &&
			slices.Contains(rule.Verbs, "watch") {
			return true
		}
	}
	return false
}

func TestParseEnvironment(t *testing.T) {

	tests := []struct {
		name    string
		c       *Config
		wantErr bool
	}{
		{"nil config", nil, false},
		{"basic config", &Config{Interface: "eth0", ServicesInterface: "eth1"}, false},
	}
	for _, tt := range tests {
		t.Logf("%v", tt.c)
		t.Run(tt.name, func(t *testing.T) {
			if err := ParseEnvironment(tt.c); (err != nil) != tt.wantErr {
				t.Errorf("ParseEnvironment() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestParseEnvironmentInstanceName(t *testing.T) {
	tests := []struct {
		name      string
		lowercase string
		uppercase string
		want      string
	}{
		{name: "lowercase", lowercase: "release_a", want: "release_a"},
		{name: "uppercase fallback", uppercase: "release_b", want: "release_b"},
		{name: "lowercase takes precedence", lowercase: "release_a", uppercase: "release_b", want: "release_a"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(instanceName, tt.lowercase)
			t.Setenv(strings.ToUpper(instanceName), tt.uppercase)

			config := &Config{}
			if err := ParseEnvironment(config); err != nil {
				t.Fatalf("ParseEnvironment() error = %v", err)
			}
			if config.InstanceName != tt.want {
				t.Fatalf("InstanceName = %q, want %q", config.InstanceName, tt.want)
			}
		})
	}
}

func TestParseEnvironmentBGPAttachIPToInterface(t *testing.T) {
	t.Setenv(bgpAttachIPToInterface, "true")

	config := &Config{}
	if err := ParseEnvironment(config); err != nil {
		t.Fatalf("ParseEnvironment() error = %v", err)
	}
	if !config.BGPAttachIPToInterface {
		t.Fatal("BGPAttachIPToInterface = false, want true")
	}
}

func TestGeneratePodSpecBGPAttachIPToInterface(t *testing.T) {
	pod, err := generatePodSpec(&Config{
		EnableBGP:              true,
		BGPAttachIPToInterface: true,
	}, "ghcr.io/kube-vip/kube-vip", "v0.0.0", true)
	if err != nil {
		t.Fatalf("generatePodSpec() error = %v", err)
	}

	for _, env := range pod.Spec.Containers[0].Env {
		if env.Name == bgpAttachIPToInterface && env.Value == "true" {
			return
		}
	}
	t.Fatalf("%s=true is missing from generated pod environment", bgpAttachIPToInterface)
}

func TestGeneratePodSpecInstanceName(t *testing.T) {
	tests := []struct {
		name         string
		instanceName string
		wantPresent  bool
	}{
		{name: "configured", instanceName: "release_a", wantPresent: true},
		{name: "empty", wantPresent: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod, err := generatePodSpec(&Config{InstanceName: tt.instanceName}, "ghcr.io/kube-vip/kube-vip", "v0.0.0", true)
			if err != nil {
				t.Fatalf("generatePodSpec() error = %v", err)
			}

			var value string
			found := false
			for _, env := range pod.Spec.Containers[0].Env {
				if env.Name == instanceName {
					found = true
					value = env.Value
					break
				}
			}
			if found != tt.wantPresent {
				t.Fatalf("instance_name present = %t, want %t", found, tt.wantPresent)
			}
			if found && value != tt.instanceName {
				t.Fatalf("instance_name = %q, want %q", value, tt.instanceName)
			}
		})
	}
}

func TestGeneratePodSpecPprof(t *testing.T) {
	tests := []struct {
		name        string
		enablePprof bool
		addr        string
		wantEnv     map[string]string
	}{
		{
			name:        "enabled emits both variables",
			enablePprof: true,
			addr:        metrics.DefaultPprofHTTPServer,
			wantEnv:     map[string]string{enablePprof: "true", pprofServer: metrics.DefaultPprofHTTPServer},
		},
		{
			name:        "disabled emits neither",
			enablePprof: false,
			addr:        metrics.DefaultPprofHTTPServer,
			wantEnv:     map[string]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod, err := generatePodSpec(&Config{
				EnablePprof:     tt.enablePprof,
				PprofHTTPServer: tt.addr,
			}, "ghcr.io/kube-vip/kube-vip", "v0.0.0", true)
			if err != nil {
				t.Fatalf("generatePodSpec() error = %v", err)
			}

			got := map[string]string{}
			for _, env := range pod.Spec.Containers[0].Env {
				if env.Name == enablePprof || env.Name == pprofServer {
					got[env.Name] = env.Value
				}
			}

			if !maps.Equal(got, tt.wantEnv) {
				t.Fatalf("pprof pod environment = %v, want %v", got, tt.wantEnv)
			}
		})
	}
}

func TestParseEnvironmentConfigFile(t *testing.T) {
	// Save original environment
	originalConfigFile := os.Getenv("config_file")
	defer func() {
		if originalConfigFile != "" {
			os.Setenv("config_file", originalConfigFile)
		} else {
			os.Unsetenv("config_file")
		}
	}()

	tests := []struct {
		name           string
		envValue       string
		expectedConfig string
	}{
		{
			name:           "config_file environment variable set",
			envValue:       "/etc/kube-vip/config.yaml",
			expectedConfig: "/etc/kube-vip/config.yaml",
		},
		{
			name:           "config_file environment variable empty",
			envValue:       "",
			expectedConfig: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set environment variable
			if tt.envValue != "" {
				os.Setenv("config_file", tt.envValue)
			} else {
				os.Unsetenv("config_file")
			}

			config := &Config{}
			err := ParseEnvironment(config)
			if err != nil {
				t.Errorf("ParseEnvironment() unexpected error = %v", err)
			}

			if config.ConfigFile != tt.expectedConfig {
				t.Errorf("ConfigFile = %v, expected %v", config.ConfigFile, tt.expectedConfig)
			}
		})
	}
}

func TestGeneratePodSpecRoutingTable(t *testing.T) {
	routingTableEnv := []string{vipRoutingTable, vipRoutingTableID, vipRoutingTableType, vipRoutingProtocol, vipCleanRoutingTable}

	tests := []struct {
		name    string
		config  Config
		wantEnv map[string]string
	}{
		{
			name: "table mode emits its settings",
			config: Config{
				EnableRoutingTable: true,
				RoutingTableID:     100,
				RoutingTableType:   2,
				RoutingProtocol:    250,
				CleanRoutingTable:  true,
			},
			wantEnv: map[string]string{
				vipRoutingTable:      "true",
				vipRoutingTableID:    "100",
				vipRoutingTableType:  "2",
				vipRoutingProtocol:   "250",
				vipCleanRoutingTable: "true",
			},
		},
		{
			name:    "table mode without settings emits only the mode",
			config:  Config{EnableRoutingTable: true},
			wantEnv: map[string]string{vipRoutingTable: "true"},
		},
		{
			name: "table mode with the flag defaults",
			config: Config{
				EnableRoutingTable: true,
				RoutingTableID:     198,
				RoutingTableType:   1,
				RoutingProtocol:    248,
			},
			wantEnv: map[string]string{
				vipRoutingTable:     "true",
				vipRoutingTableID:   "198",
				vipRoutingTableType: "1",
				vipRoutingProtocol:  "248",
			},
		},
		{
			name: "settings without table mode emit nothing",
			config: Config{
				RoutingTableID:    100,
				RoutingTableType:  2,
				RoutingProtocol:   250,
				CleanRoutingTable: true,
			},
			wantEnv: map[string]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod, err := generatePodSpec(&tt.config, "ghcr.io/kube-vip/kube-vip", "v0.0.0", true)
			if err != nil {
				t.Fatalf("generatePodSpec() error = %v", err)
			}

			got := map[string]string{}
			for _, env := range pod.Spec.Containers[0].Env {
				if slices.Contains(routingTableEnv, env.Name) {
					got[env.Name] = env.Value
				}
			}

			if !maps.Equal(got, tt.wantEnv) {
				t.Fatalf("routing table pod environment = %v, want %v", got, tt.wantEnv)
			}
		})
	}
}

func TestGeneratePodSpecBGPSettings(t *testing.T) {
	bgpSettingsEnv := []string{bgpMultiHop, bgpHoldTime, bgpKeepaliveInterval, zebraEnable, zebraURL, zebraVersion, zebraSoftwareName}

	tests := []struct {
		name    string
		config  Config
		wantEnv map[string]string
	}{
		{
			name: "BGP mode emits multihop, timer and Zebra settings",
			config: Config{
				EnableBGP: true,
				BGPConfig: BGPConfig{
					HoldTime:          9,
					KeepaliveInterval: 3,
					Zebra: ZebraConfig{
						Enabled:      true,
						URL:          "unix:/run/frr/zserv.api",
						Version:      6,
						SoftwareName: "frr10.1",
					},
				},
				BGPPeerConfig: BGPPeer{Address: "192.0.2.1", AS: 65001, MultiHop: true},
			},
			wantEnv: map[string]string{
				bgpMultiHop:          "true",
				bgpHoldTime:          "9",
				bgpKeepaliveInterval: "3",
				zebraEnable:          "true",
				zebraURL:             "unix:/run/frr/zserv.api",
				zebraVersion:         "6",
				zebraSoftwareName:    "frr10.1",
			},
		},
		{
			name:    "BGP mode without settings emits none of them",
			config:  Config{EnableBGP: true},
			wantEnv: map[string]string{},
		},
		{
			name: "Zebra connection settings are left out while Zebra is disabled",
			config: Config{
				EnableBGP: true,
				BGPConfig: BGPConfig{
					Zebra: ZebraConfig{
						URL:          "unix:/var/run/frr/zserv.api",
						Version:      6,
						SoftwareName: "frr8.3",
					},
				},
			},
			wantEnv: map[string]string{},
		},
		{
			name: "settings without BGP mode emit nothing",
			config: Config{
				BGPConfig: BGPConfig{
					HoldTime:          9,
					KeepaliveInterval: 3,
					Zebra:             ZebraConfig{Enabled: true, URL: "unix:/run/frr/zserv.api"},
				},
				BGPPeerConfig: BGPPeer{MultiHop: true},
			},
			wantEnv: map[string]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod, err := generatePodSpec(&tt.config, "ghcr.io/kube-vip/kube-vip", "v0.0.0", true)
			if err != nil {
				t.Fatalf("generatePodSpec() error = %v", err)
			}

			got := map[string]string{}
			for _, env := range pod.Spec.Containers[0].Env {
				if slices.Contains(bgpSettingsEnv, env.Name) {
					got[env.Name] = env.Value
				}
			}

			if !maps.Equal(got, tt.wantEnv) {
				t.Fatalf("BGP pod environment = %v, want %v", got, tt.wantEnv)
			}
		})
	}
}

func TestGeneratedBGPSettingsRoundTrip(t *testing.T) {
	config := Config{
		EnableBGP: true,
		BGPConfig: BGPConfig{
			AS:                65000,
			HoldTime:          9,
			KeepaliveInterval: 3,
			Zebra: ZebraConfig{
				Enabled:      true,
				URL:          "unix:/run/frr/zserv.api",
				Version:      6,
				SoftwareName: "frr10.1",
			},
		},
		BGPPeerConfig: BGPPeer{Address: "192.0.2.1", AS: 65001, MultiHop: true},
	}

	pod, err := generatePodSpec(&config, "ghcr.io/kube-vip/kube-vip", "v0.0.0", true)
	if err != nil {
		t.Fatalf("generatePodSpec() error = %v", err)
	}
	for _, env := range pod.Spec.Containers[0].Env {
		if env.ValueFrom == nil {
			t.Setenv(env.Name, env.Value)
		}
	}

	got := &Config{}
	if err := ParseEnvironment(got); err != nil {
		t.Fatalf("ParseEnvironment() error = %v", err)
	}

	if len(got.BGPConfig.Peers) != 1 || !got.BGPConfig.Peers[0].MultiHop {
		t.Fatalf("BGP peers = %+v, want a single multihop peer", got.BGPConfig.Peers)
	}
	if got.BGPConfig.HoldTime != config.BGPConfig.HoldTime || got.BGPConfig.KeepaliveInterval != config.BGPConfig.KeepaliveInterval {
		t.Fatalf("BGP timers = hold %d keepalive %d, want hold %d keepalive %d",
			got.BGPConfig.HoldTime, got.BGPConfig.KeepaliveInterval, config.BGPConfig.HoldTime, config.BGPConfig.KeepaliveInterval)
	}
	if got.BGPConfig.Zebra != config.BGPConfig.Zebra {
		t.Fatalf("Zebra config = %+v, want %+v", got.BGPConfig.Zebra, config.BGPConfig.Zebra)
	}
}

func TestGeneratePodSpecLoadBalancerClass(t *testing.T) {
	lbClassEnv := []string{lbClassName, lbClassLegacyHandling}

	tests := []struct {
		name    string
		config  Config
		wantEnv map[string]string
	}{
		{
			name: "default class and legacy handling emit nothing",
			config: Config{
				EnableServices:                  true,
				LoadBalancerClassName:           LBClassName,
				LoadBalancerClassLegacyHandling: true,
			},
			wantEnv: map[string]string{},
		},
		{
			name: "custom class name and disabled legacy handling are emitted",
			config: Config{
				EnableServices:                  true,
				LoadBalancerClassName:           "kube-vip.io/kube-vip-class-b",
				LoadBalancerClassLegacyHandling: false,
			},
			wantEnv: map[string]string{
				lbClassName:           "kube-vip.io/kube-vip-class-b",
				lbClassLegacyHandling: "false",
			},
		},
		{
			name: "empty class name is emitted",
			config: Config{
				EnableServices:                  true,
				LoadBalancerClassName:           "",
				LoadBalancerClassLegacyHandling: true,
			},
			wantEnv: map[string]string{
				lbClassName: "",
			},
		},
		{
			name: "class settings without services emit nothing",
			config: Config{
				LoadBalancerClassName:           "kube-vip.io/kube-vip-class-b",
				LoadBalancerClassLegacyHandling: false,
			},
			wantEnv: map[string]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod, err := generatePodSpec(&tt.config, "ghcr.io/kube-vip/kube-vip", "v0.0.0", true)
			if err != nil {
				t.Fatalf("generatePodSpec() error = %v", err)
			}

			got := map[string]string{}
			for _, env := range pod.Spec.Containers[0].Env {
				if slices.Contains(lbClassEnv, env.Name) {
					got[env.Name] = env.Value
				}
			}

			if !maps.Equal(got, tt.wantEnv) {
				t.Fatalf("load balancer class pod environment = %v, want %v", got, tt.wantEnv)
			}
		})
	}
}

func TestGeneratedLoadBalancerClassRoundTrip(t *testing.T) {
	config := Config{
		EnableServices:                  true,
		LoadBalancerClassName:           "kube-vip.io/kube-vip-class-b",
		LoadBalancerClassLegacyHandling: false,
	}

	pod, err := generatePodSpec(&config, "ghcr.io/kube-vip/kube-vip", "v0.0.0", true)
	if err != nil {
		t.Fatalf("generatePodSpec() error = %v", err)
	}
	for _, env := range pod.Spec.Containers[0].Env {
		if env.ValueFrom == nil {
			t.Setenv(env.Name, env.Value)
		}
	}

	// Start from the flag defaults the pod would otherwise run with
	got := &Config{LoadBalancerClassName: LBClassName, LoadBalancerClassLegacyHandling: true}
	if err := ParseEnvironment(got); err != nil {
		t.Fatalf("ParseEnvironment() error = %v", err)
	}

	if got.LoadBalancerClassName != config.LoadBalancerClassName {
		t.Fatalf("LoadBalancerClassName = %q, want %q", got.LoadBalancerClassName, config.LoadBalancerClassName)
	}
	if got.LoadBalancerClassLegacyHandling != config.LoadBalancerClassLegacyHandling {
		t.Fatalf("LoadBalancerClassLegacyHandling = %t, want %t", got.LoadBalancerClassLegacyHandling, config.LoadBalancerClassLegacyHandling)
	}
}
