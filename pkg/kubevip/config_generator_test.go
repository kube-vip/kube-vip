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

func TestParseEnvironmentBGPCommunities(t *testing.T) {
	tests := []struct {
		name                 string
		env                  map[string]string
		wantCommunities      []string
		wantLargeCommunities []string
	}{
		{
			name:                 "unset preserves configured values",
			wantCommunities:      []string{"65000:1"},
			wantLargeCommunities: []string{"65000:1:1"},
		},
		{
			name: "comma separated values replace configured values",
			env: map[string]string{
				bgpCommunities:      " 65000:100, no-export ,",
				bgpLargeCommunities: "4200000000:1:2,4200000000:3:4",
			},
			wantCommunities:      []string{"65000:100", "no-export"},
			wantLargeCommunities: []string{"4200000000:1:2", "4200000000:3:4"},
		},
		{
			name: "empty values clear configured values",
			env: map[string]string{
				bgpCommunities:      "",
				bgpLargeCommunities: "",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for name, value := range tt.env {
				t.Setenv(name, value)
			}

			config := &Config{BGPConfig: BGPConfig{
				Communities:      []string{"65000:1"},
				LargeCommunities: []string{"65000:1:1"},
			}}
			if err := ParseEnvironment(config); err != nil {
				t.Fatalf("ParseEnvironment() error = %v", err)
			}
			if !slices.Equal(config.BGPConfig.Communities, tt.wantCommunities) {
				t.Fatalf("Communities = %q, want %q", config.BGPConfig.Communities, tt.wantCommunities)
			}
			if !slices.Equal(config.BGPConfig.LargeCommunities, tt.wantLargeCommunities) {
				t.Fatalf("LargeCommunities = %q, want %q", config.BGPConfig.LargeCommunities, tt.wantLargeCommunities)
			}
		})
	}
}

func TestGeneratePodSpecBGPCommunities(t *testing.T) {
	communityEnv := []string{bgpCommunities, bgpLargeCommunities}

	tests := []struct {
		name    string
		config  Config
		wantEnv map[string]string
	}{
		{
			name: "BGP mode emits configured communities",
			config: Config{
				EnableBGP: true,
				BGPConfig: BGPConfig{
					Communities:      []string{"65000:100", "no-export"},
					LargeCommunities: []string{"4200000000:1:2"},
				},
			},
			wantEnv: map[string]string{
				bgpCommunities:      "65000:100,no-export",
				bgpLargeCommunities: "4200000000:1:2",
			},
		},
		{
			name:    "BGP mode without communities emits nothing",
			config:  Config{EnableBGP: true},
			wantEnv: map[string]string{},
		},
		{
			name: "communities without BGP mode emit nothing",
			config: Config{BGPConfig: BGPConfig{
				Communities:      []string{"65000:100"},
				LargeCommunities: []string{"4200000000:1:2"},
			}},
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
				if slices.Contains(communityEnv, env.Name) {
					got[env.Name] = env.Value
				}
			}

			if !maps.Equal(got, tt.wantEnv) {
				t.Fatalf("BGP community pod environment = %v, want %v", got, tt.wantEnv)
			}
		})
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
