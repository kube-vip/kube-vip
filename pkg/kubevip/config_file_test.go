package kubevip

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kube-vip/kube-vip/pkg/bgp"
)

func TestLoadConfigFromFile(t *testing.T) {
	// Create temporary directory for test files
	tmpDir, err := os.MkdirTemp("", "kube-vip-config-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	tests := []struct {
		name           string
		filename       string
		content        string
		expectedConfig *Config
		wantErr        bool
		errContains    string
	}{
		{
			name:     "Valid YAML config",
			filename: "config.yaml",
			content: `
logging: 2
enableARP: true
enableControlPlane: true
enableServices: true
address: "192.168.1.100"
port: 6443
interface: "eth0"
namespace: "kube-system"
vipSubnet: "192.168.1.0/24"
leaseName: "test-lease"
leaseDuration: 15
renewDeadline: 10
retryPeriod: 2
prometheusHTTPServer: ":2112"
`,
			expectedConfig: &Config{
				Logging:              2,
				EnableARP:            true,
				EnableControlPlane:   true,
				EnableServices:       true,
				Address:              "192.168.1.100",
				Port:                 6443,
				Interface:            "eth0",
				Namespace:            "kube-system",
				VIPSubnet:            "192.168.1.0/24",
				PrometheusHTTPServer: ":2112",
				KubernetesLeaderElection: KubernetesLeaderElection{
					LeaseName:     "test-lease",
					LeaseDuration: 15,
					RenewDeadline: 10,
					RetryPeriod:   2,
				},
			},
			wantErr: false,
		},
		{
			name:     "Valid JSON config",
			filename: "config.json",
			content: `{
  "logging": 3,
  "enableBGP": true,
  "enableServices": true,
  "address": "10.0.0.100",
  "port": 8443,
  "interface": "ens192",
  "namespace": "kube-system",
  "loadBalancers": [
    {
      "name": "control-plane",
      "ports": [
        {
          "type": "TCP",
          "port": 6443
        }
      ],
      "bindToVip": true,
      "forwardingMethod": "local"
    }
  ]
}`,
			expectedConfig: &Config{
				Logging:        3,
				EnableBGP:      true,
				EnableServices: true,
				Address:        "10.0.0.100",
				Port:           8443,
				Interface:      "ens192",
				Namespace:      "kube-system",
				LoadBalancers: []LoadBalancer{
					{
						Name: "control-plane",
						Ports: []Port{
							{
								Type: "TCP",
								Port: 6443,
							},
						},
						BindToVip:        true,
						ForwardingMethod: "local",
					},
				},
			},
			wantErr: false,
		},
		{
			name:     "Complex BGP config",
			filename: "bgp-config.yaml",
			content: `
enableBGP: true
bgpConfig:
  routerID: "192.168.1.1"
  as: 65000
  sourceIF: "eth0"
  holdTime: 60
  keepaliveInterval: 20
  peers:
    - address: "192.168.1.2"
      as: 65001
      port: 179
      multiHop: false
    - address: "192.168.1.3"
      as: 65002
      port: 179
      multiHop: true
`,
			expectedConfig: &Config{
				EnableBGP: true,
				BGPConfig: bgp.Config{
					RouterID:          "192.168.1.1",
					AS:                65000,
					SourceIF:          "eth0",
					HoldTime:          60,
					KeepaliveInterval: 20,
					Peers: []bgp.Peer{
						{
							Address:  "192.168.1.2",
							AS:       65001,
							Port:     179,
							MultiHop: false,
						},
						{
							Address:  "192.168.1.3",
							AS:       65002,
							Port:     179,
							MultiHop: true,
						},
					},
				},
			},
			wantErr: false,
		},
		{
			name:        "Invalid JSON",
			filename:    "invalid.json",
			content:     `{"logging": 2, "invalid": }`,
			wantErr:     true,
			errContains: "failed to parse JSON config file",
		},
		{
			name:        "Invalid YAML",
			filename:    "invalid.yaml",
			content:     "logging: 2\ninvalid: [unclosed",
			wantErr:     true,
			errContains: "failed to parse YAML config file",
		},
		{
			name:        "Unsupported format",
			filename:    "config.txt",
			content:     "logging=2",
			wantErr:     true,
			errContains: "unsupported config file format",
		},
		{
			name:        "Empty path",
			filename:    "",
			content:     "",
			wantErr:     true,
			errContains: "config file path is empty",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var filePath string
			if tt.filename != "" {
				filePath = filepath.Join(tmpDir, tt.filename)
				if err := os.WriteFile(filePath, []byte(tt.content), 0600); err != nil {
					t.Fatalf("Failed to write test file: %v", err)
				}
			}

			config, err := LoadConfigFromFile(filePath)

			if tt.wantErr {
				if err == nil {
					t.Errorf("LoadConfigFromFile() expected error, got nil")
					return
				}
				if tt.errContains != "" && !containsString(err.Error(), tt.errContains) {
					t.Errorf("LoadConfigFromFile() error = %v, expected to contain %v", err, tt.errContains)
				}
				return
			}

			if err != nil {
				t.Errorf("LoadConfigFromFile() unexpected error = %v", err)
				return
			}

			if config == nil {
				t.Errorf("LoadConfigFromFile() returned nil config")
				return
			}

			// Compare key fields
			if config.Logging != tt.expectedConfig.Logging {
				t.Errorf("Logging = %v, expected %v", config.Logging, tt.expectedConfig.Logging)
			}
			if config.EnableARP != tt.expectedConfig.EnableARP {
				t.Errorf("EnableARP = %v, expected %v", config.EnableARP, tt.expectedConfig.EnableARP)
			}
			if config.EnableBGP != tt.expectedConfig.EnableBGP {
				t.Errorf("EnableBGP = %v, expected %v", config.EnableBGP, tt.expectedConfig.EnableBGP)
			}
			if config.Address != tt.expectedConfig.Address {
				t.Errorf("Address = %v, expected %v", config.Address, tt.expectedConfig.Address)
			}
			if config.Port != tt.expectedConfig.Port {
				t.Errorf("Port = %v, expected %v", config.Port, tt.expectedConfig.Port)
			}
			if config.Interface != tt.expectedConfig.Interface {
				t.Errorf("Interface = %v, expected %v", config.Interface, tt.expectedConfig.Interface)
			}

			// Test BGP config if present
			if tt.expectedConfig.EnableBGP {
				if config.BGPConfig.RouterID != tt.expectedConfig.BGPConfig.RouterID {
					t.Errorf("BGPConfig.RouterID = %v, expected %v", config.BGPConfig.RouterID, tt.expectedConfig.BGPConfig.RouterID)
				}
				if config.BGPConfig.AS != tt.expectedConfig.BGPConfig.AS {
					t.Errorf("BGPConfig.AS = %v, expected %v", config.BGPConfig.AS, tt.expectedConfig.BGPConfig.AS)
				}
				if len(config.BGPConfig.Peers) != len(tt.expectedConfig.BGPConfig.Peers) {
					t.Errorf("BGPConfig.Peers length = %v, expected %v", len(config.BGPConfig.Peers), len(tt.expectedConfig.BGPConfig.Peers))
				}
			}

			// Test LoadBalancers if present
			if len(tt.expectedConfig.LoadBalancers) > 0 {
				if len(config.LoadBalancers) != len(tt.expectedConfig.LoadBalancers) {
					t.Errorf("LoadBalancers length = %v, expected %v", len(config.LoadBalancers), len(tt.expectedConfig.LoadBalancers))
				}
			}
		})
	}
}

func TestMergeConfigFromFile(t *testing.T) {
	// Create temporary directory for test files
	tmpDir, err := os.MkdirTemp("", "kube-vip-merge-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create test config file
	configFile := filepath.Join(tmpDir, "test-config.yaml")
	configContent := `
logging: 3
enableARP: true
enableServices: true
address: "192.168.1.200"
port: 6443
interface: "eth1"
namespace: "test-namespace"
leaseName: "file-lease"
leaseDuration: 20
prometheusHTTPServer: ":3000"
`
	if err := os.WriteFile(configFile, []byte(configContent), 0600); err != nil {
		t.Fatalf("Failed to write test config file: %v", err)
	}

	tests := []struct {
		name           string
		baseConfig     *Config
		configFilePath string
		expected       *Config
		wantErr        bool
	}{
		{
			name:           "Merge with empty base config",
			baseConfig:     &Config{},
			configFilePath: configFile,
			expected: &Config{
				Logging:              3,
				EnableARP:            true,
				EnableServices:       true,
				Address:              "192.168.1.200",
				Port:                 6443,
				Interface:            "eth1",
				Namespace:            "test-namespace",
				PrometheusHTTPServer: ":3000",
				KubernetesLeaderElection: KubernetesLeaderElection{
					LeaseName:     "file-lease",
					LeaseDuration: 20,
				},
			},
			wantErr: false,
		},
		{
			name: "Merge respects existing values (priority test)",
			baseConfig: &Config{
				Logging:   5,      // Should not be overridden
				Port:      8443,   // Should not be overridden
				Interface: "eth0", // Should not be overridden
			},
			configFilePath: configFile,
			expected: &Config{
				Logging:              5,                // From base (higher priority)
				EnableARP:            true,             // From file
				EnableServices:       true,             // From file
				Address:              "192.168.1.200",  // From file
				Port:                 8443,             // From base (higher priority)
				Interface:            "eth0",           // From base (higher priority)
				Namespace:            "test-namespace", // From file
				PrometheusHTTPServer: ":3000",          // From file
				KubernetesLeaderElection: KubernetesLeaderElection{
					LeaseName:     "file-lease", // From file
					LeaseDuration: 20,           // From file
				},
			},
			wantErr: false,
		},
		{
			name: "Empty config file path",
			baseConfig: &Config{
				Logging: 1,
			},
			configFilePath: "",
			expected: &Config{
				Logging: 1, // Unchanged
			},
			wantErr: false,
		},
		{
			name: "Non-existent config file",
			baseConfig: &Config{
				Logging: 1,
			},
			configFilePath: "/non/existent/file.yaml",
			expected: &Config{
				Logging: 1,
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := MergeConfigFromFile(tt.baseConfig, tt.configFilePath)

			if tt.wantErr {
				if err == nil {
					t.Errorf("MergeConfigFromFile() expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Errorf("MergeConfigFromFile() unexpected error = %v", err)
				return
			}

			// Compare key fields
			if tt.baseConfig.Logging != tt.expected.Logging {
				t.Errorf("Logging = %v, expected %v", tt.baseConfig.Logging, tt.expected.Logging)
			}
			if tt.baseConfig.EnableARP != tt.expected.EnableARP {
				t.Errorf("EnableARP = %v, expected %v", tt.baseConfig.EnableARP, tt.expected.EnableARP)
			}
			if tt.baseConfig.Address != tt.expected.Address {
				t.Errorf("Address = %v, expected %v", tt.baseConfig.Address, tt.expected.Address)
			}
			if tt.baseConfig.Port != tt.expected.Port {
				t.Errorf("Port = %v, expected %v", tt.baseConfig.Port, tt.expected.Port)
			}
			if tt.baseConfig.Interface != tt.expected.Interface {
				t.Errorf("Interface = %v, expected %v", tt.baseConfig.Interface, tt.expected.Interface)
			}
			if tt.baseConfig.Namespace != tt.expected.Namespace {
				t.Errorf("Namespace = %v, expected %v", tt.baseConfig.Namespace, tt.expected.Namespace)
			}
			if tt.baseConfig.PrometheusHTTPServer != tt.expected.PrometheusHTTPServer {
				t.Errorf("PrometheusHTTPServer = %v, expected %v", tt.baseConfig.PrometheusHTTPServer, tt.expected.PrometheusHTTPServer)
			}
		})
	}
}

func TestMergeConfigValues(t *testing.T) {
	tests := []struct {
		name         string
		baseConfig   *Config
		fileConfig   *Config
		expectedBase *Config
	}{
		{
			name: "Merge basic configuration",
			baseConfig: &Config{
				Logging: 5, // Should not be overridden
				Port:    0, // Should be overridden
			},
			fileConfig: &Config{
				Logging:   2,
				Port:      6443,
				Interface: "eth0",
				Address:   "192.168.1.100",
			},
			expectedBase: &Config{
				Logging:   5,               // From base (non-zero)
				Port:      6443,            // From file (base was zero)
				Interface: "eth0",          // From file (base was empty)
				Address:   "192.168.1.100", // From file (base was empty)
			},
		},
		{
			name: "Merge boolean flags",
			baseConfig: &Config{
				EnableARP: true, // Should not be overridden
			},
			fileConfig: &Config{
				EnableARP:       false, // Should not override true
				EnableBGP:       true,  // Should be set
				EnableServices:  true,  // Should be set
				EnableWireguard: false, // Should not be set (false doesn't override false)
			},
			expectedBase: &Config{
				EnableARP:       true,  // From base (true has priority)
				EnableBGP:       true,  // From file
				EnableServices:  true,  // From file
				EnableWireguard: false, // Remains false
			},
		},
		{
			name: "Merge BGP configuration",
			baseConfig: &Config{
				BGPConfig: bgp.Config{
					RouterID: "1.1.1.1", // Should not be overridden
				},
			},
			fileConfig: &Config{
				BGPConfig: bgp.Config{
					RouterID:          "2.2.2.2", // Should not override
					AS:                65000,     // Should be set
					SourceIF:          "eth0",    // Should be set
					HoldTime:          30,        // Should be set
					KeepaliveInterval: 10,        // Should be set
				},
			},
			expectedBase: &Config{
				BGPConfig: bgp.Config{
					RouterID:          "1.1.1.1", // From base (non-empty)
					AS:                65000,     // From file (base was zero)
					SourceIF:          "eth0",    // From file (base was empty)
					HoldTime:          30,        // From file (base was zero)
					KeepaliveInterval: 10,        // From file (base was zero)
				},
			},
		},
		{
			name: "Merge leader election configuration",
			baseConfig: &Config{
				KubernetesLeaderElection: KubernetesLeaderElection{
					LeaseName: "base-lease", // Should not be overridden
				},
			},
			fileConfig: &Config{
				KubernetesLeaderElection: KubernetesLeaderElection{
					LeaseName:     "file-lease", // Should not override
					LeaseDuration: 15,           // Should be set
					RenewDeadline: 10,           // Should be set
					RetryPeriod:   2,            // Should be set
				},
			},
			expectedBase: &Config{
				KubernetesLeaderElection: KubernetesLeaderElection{
					LeaseName:     "base-lease", // From base (non-empty)
					LeaseDuration: 15,           // From file (base was zero)
					RenewDeadline: 10,           // From file (base was zero)
					RetryPeriod:   2,            // From file (base was zero)
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mergeConfigValues(tt.baseConfig, tt.fileConfig)

			// Compare results
			if tt.baseConfig.Logging != tt.expectedBase.Logging {
				t.Errorf("Logging = %v, expected %v", tt.baseConfig.Logging, tt.expectedBase.Logging)
			}
			if tt.baseConfig.Port != tt.expectedBase.Port {
				t.Errorf("Port = %v, expected %v", tt.baseConfig.Port, tt.expectedBase.Port)
			}
			if tt.baseConfig.Interface != tt.expectedBase.Interface {
				t.Errorf("Interface = %v, expected %v", tt.baseConfig.Interface, tt.expectedBase.Interface)
			}
			if tt.baseConfig.EnableARP != tt.expectedBase.EnableARP {
				t.Errorf("EnableARP = %v, expected %v", tt.baseConfig.EnableARP, tt.expectedBase.EnableARP)
			}
			if tt.baseConfig.EnableBGP != tt.expectedBase.EnableBGP {
				t.Errorf("EnableBGP = %v, expected %v", tt.baseConfig.EnableBGP, tt.expectedBase.EnableBGP)
			}
			if tt.baseConfig.BGPConfig.RouterID != tt.expectedBase.BGPConfig.RouterID {
				t.Errorf("BGPConfig.RouterID = %v, expected %v", tt.baseConfig.BGPConfig.RouterID, tt.expectedBase.BGPConfig.RouterID)
			}
			if tt.baseConfig.BGPConfig.AS != tt.expectedBase.BGPConfig.AS {
				t.Errorf("BGPConfig.AS = %v, expected %v", tt.baseConfig.BGPConfig.AS, tt.expectedBase.BGPConfig.AS)
			}
			if tt.baseConfig.KubernetesLeaderElection.LeaseName != tt.expectedBase.KubernetesLeaderElection.LeaseName {
				t.Errorf("KubernetesLeaderElection.LeaseName = %v, expected %v", tt.baseConfig.KubernetesLeaderElection.LeaseName, tt.expectedBase.KubernetesLeaderElection.LeaseName)
			}
		})
	}
}

func TestLoadConfigFromFile_FileNotExists(t *testing.T) {
	_, err := LoadConfigFromFile("/non/existent/path/config.yaml")
	if err == nil {
		t.Error("LoadConfigFromFile() expected error for non-existent file, got nil")
	}
	if !containsString(err.Error(), "config file does not exist") {
		t.Errorf("LoadConfigFromFile() error = %v, expected to contain 'config file does not exist'", err)
	}
}

// Helper function to check if a string contains a substring
func containsString(str, substr string) bool {
	return len(str) >= len(substr) && (str == substr || len(substr) == 0 ||
		(len(substr) > 0 && func() bool {
			for i := 0; i <= len(str)-len(substr); i++ {
				if str[i:i+len(substr)] == substr {
					return true
				}
			}
			return false
		}()))
}
