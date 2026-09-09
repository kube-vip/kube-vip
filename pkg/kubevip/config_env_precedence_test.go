package kubevip

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestParseEnvironmentPreservesConfiguredARPAndDHCPValues(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		set  func(*Config) string
		want string
	}{
		{
			name: "ARP broadcast rate",
			cfg:  Config{ArpBroadcastRate: 1234},
			set: func(c *Config) string {
				return strconv.FormatInt(c.ArpBroadcastRate, 10)
			},
			want: "1234",
		},
		{
			name: "DHCP mode",
			cfg:  Config{DNSMode: "first", DHCPMode: "ipv6"},
			set: func(c *Config) string {
				return c.DHCPMode
			},
			want: "ipv6",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(vipArpRate, "")
			t.Setenv(dhcpMode, "")
			t.Setenv(dnsMode, "")

			config := tt.cfg
			if err := ParseEnvironment(&config); err != nil {
				t.Fatalf("ParseEnvironment() error = %v", err)
			}

			if got := tt.set(&config); got != tt.want {
				t.Fatalf("configured value was overwritten: got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseEnvironmentDefaultsARPAndDHCPValues(t *testing.T) {
	t.Setenv(vipArpRate, "")
	t.Setenv(dhcpMode, "")
	t.Setenv(dnsMode, "")

	config := Config{DNSMode: "first"}
	if err := ParseEnvironment(&config); err != nil {
		t.Fatalf("ParseEnvironment() error = %v", err)
	}

	if config.ArpBroadcastRate != 3000 {
		t.Fatalf("ArpBroadcastRate = %d, want 3000", config.ArpBroadcastRate)
	}
	if config.DHCPMode != "ipv4" {
		t.Fatalf("DHCPMode = %q, want %q", config.DHCPMode, "ipv4")
	}
}

func TestParseEnvironmentOverridesConfiguredARPAndDHCPValues(t *testing.T) {
	t.Setenv(vipArpRate, "5678")
	t.Setenv(dhcpMode, "dual")

	config := Config{ArpBroadcastRate: 1234, DHCPMode: "ipv6"}
	if err := ParseEnvironment(&config); err != nil {
		t.Fatalf("ParseEnvironment() error = %v", err)
	}

	if config.ArpBroadcastRate != 5678 {
		t.Fatalf("ArpBroadcastRate = %d, want 5678", config.ArpBroadcastRate)
	}
	if config.DHCPMode != "dual" {
		t.Fatalf("DHCPMode = %q, want %q", config.DHCPMode, "dual")
	}
}

func TestParseEnvironmentPreservesConfigFileARPRate(t *testing.T) {
	t.Setenv(vipArpRate, "")

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("arpBroadcastRate: 1234\n"), 0o600); err != nil {
		t.Fatalf("write config file: %v", err)
	}

	config := Config{}
	if err := MergeConfigFromFile(&config, path); err != nil {
		t.Fatalf("MergeConfigFromFile() error = %v", err)
	}
	if err := ParseEnvironment(&config); err != nil {
		t.Fatalf("ParseEnvironment() error = %v", err)
	}

	if config.ArpBroadcastRate != 1234 {
		t.Fatalf("ArpBroadcastRate = %d, want 1234", config.ArpBroadcastRate)
	}
}

func TestLoadConfigFromFileDHCPAndDNSModes(t *testing.T) {
	tests := []struct {
		name    string
		ext     string
		content string
	}{
		{
			name:    "YAML",
			ext:     ".yaml",
			content: "dnsDualStackMode: dual\ndhcpDualStackMode: ipv6\n",
		},
		{
			name:    "JSON",
			ext:     ".json",
			content: `{"dnsDualStackMode":"dual","dhcpDualStackMode":"ipv6"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(dhcpMode, "")
			t.Setenv(dnsMode, "")

			path := filepath.Join(t.TempDir(), "config"+tt.ext)
			if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
				t.Fatalf("write config file: %v", err)
			}

			config, err := LoadConfigFromFile(path)
			if err != nil {
				t.Fatalf("LoadConfigFromFile() error = %v", err)
			}
			if err := ParseEnvironment(config); err != nil {
				t.Fatalf("ParseEnvironment() error = %v", err)
			}
			if config.DNSMode != "dual" {
				t.Fatalf("DNSMode = %q, want %q", config.DNSMode, "dual")
			}
			if config.DHCPMode != "ipv6" {
				t.Fatalf("DHCPMode = %q, want %q", config.DHCPMode, "ipv6")
			}
		})
	}
}
