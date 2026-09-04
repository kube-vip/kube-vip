package kubevip

import (
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
		// DEFECT: pkg/kubevip/config_environment.go:317-328 resets a
		// non-zero config-file/flag ARP rate to 3000 whenever vip_arpRate is
		// absent, even though absent environment variables must not override
		// lower-priority configuration.
		{
			name: "ARP broadcast rate",
			cfg:  Config{ArpBroadcastRate: 1234},
			set: func(c *Config) string {
				return strconv.FormatInt(c.ArpBroadcastRate, 10)
			},
			want: "1234",
		},
		// DEFECT: pkg/kubevip/config_environment.go:425-435 derives a
		// default DHCP mode and overwrites a value loaded from the config file
		// whenever dhcp_mode is absent.
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
