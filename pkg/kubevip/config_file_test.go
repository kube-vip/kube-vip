package kubevip

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMergeConfigFromFilePresence(t *testing.T) {
	tests := []struct {
		name    string
		ext     string
		content string
		check   func(*testing.T, *Config)
	}{
		{
			name: "omitted values retain defaults",
			ext:  ".yaml",
			content: `enableARP: true
`,
			check: func(t *testing.T, config *Config) {
				if config.Port != 6443 || !config.EnableARP {
					t.Fatalf("Port = %d, EnableARP = %t", config.Port, config.EnableARP)
				}
			},
		},
		{
			name: "explicit zero false and empty override defaults",
			ext:  ".yaml",
			content: `port: 0
lbClassNameLegacyHandling: false
prometheusHTTPServer: ""
`,
			check: func(t *testing.T, config *Config) {
				if config.Port != 0 || config.LoadBalancerClassLegacyHandling || config.PrometheusHTTPServer != "" {
					t.Fatalf("explicit values not retained: %#v", config)
				}
			},
		},
		{
			name:    "JSON external names",
			ext:     ".json",
			content: `{"enableBGP":true,"dnsDualStackMode":"dual","bgpConfig":{"routerID":"192.0.2.1","as":65000}}`,
			check: func(t *testing.T, config *Config) {
				if !config.EnableBGP || config.DNSMode != "dual" || config.BGPConfig.RouterID != "192.0.2.1" {
					t.Fatalf("JSON names not decoded: %#v", config)
				}
			},
		},
		{
			name: "standalone BGP peer is consumed by runtime",
			ext:  ".yaml",
			content: `bgpPeerConfig:
  address: 192.0.2.2
  as: 65001
  multiHop: true
`,
			check: func(t *testing.T, config *Config) {
				if len(config.BGPConfig.Peers) != 1 || config.BGPConfig.Peers[0].Address != "192.0.2.2" || !config.BGPConfig.Peers[0].MultiHop {
					t.Fatalf("standalone peer not normalized: %#v", config.BGPConfig)
				}
			},
		},
		{
			name: "YAML aliases",
			ext:  ".yaml",
			content: `bgpConfig:
  peers:
    - &peer
      address: 192.0.2.3
      as: 65003
    - *peer
`,
			check: func(t *testing.T, config *Config) {
				if len(config.BGPConfig.Peers) != 2 || config.BGPConfig.Peers[1].Address != "192.0.2.3" {
					t.Fatalf("YAML alias not decoded: %#v", config.BGPConfig.Peers)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config"+test.ext)
			if err := os.WriteFile(path, []byte(test.content), 0600); err != nil {
				t.Fatal(err)
			}
			config := &Config{Port: 6443, LoadBalancerClassLegacyHandling: true, PrometheusHTTPServer: ":2112"}
			if err := MergeConfigFromFile(config, path); err != nil {
				t.Fatal(err)
			}
			if config.ConfigFile != path {
				t.Fatalf("ConfigFile = %q, want %q", config.ConfigFile, path)
			}
			test.check(t, config)
		})
	}
}

func TestMergeConfigFromFileRejectsUnknownAndExcludedFields(t *testing.T) {
	tests := []struct {
		name string
		key  string
	}{
		{name: "unknown", key: "unknownSetting"},
		{name: "runtime-derived dual stack", key: "isDualStack"},
		{name: "bootstrap config path", key: "configFile"},
		{name: "generator-only load balancers", key: "loadBalancers"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(test.key+": true\n"), 0600); err != nil {
				t.Fatal(err)
			}
			err := MergeConfigFromFile(&Config{}, path)
			if err == nil || !strings.Contains(err.Error(), "field "+test.key+" not found") {
				t.Fatalf("error = %v, want unknown-field error", err)
			}
		})
	}
}

func TestLoadConfigFromFileErrors(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
	}{
		{name: "empty", want: "config file path is empty"},
		{name: "missing", path: filepath.Join(t.TempDir(), "missing.yaml"), want: "config file does not exist"},
		{name: "unsupported", path: filepath.Join(t.TempDir(), "config.txt"), want: "unsupported config file format"},
	}
	if err := os.WriteFile(tests[2].path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := LoadConfigFromFile(test.path)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}
