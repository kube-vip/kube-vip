package kubevip

import (
	"os"
	"strings"
	"testing"
)

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
