package kubevip

import (
	"os"
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
