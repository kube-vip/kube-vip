package kubevip

import (
	"strings"
	"testing"

	"github.com/kube-vip/kube-vip/pkg/metrics"
)

func TestValidate_HealthCheckAddress(t *testing.T) {
	tests := []struct {
		name    string
		address string
		wantErr bool
	}{
		{"empty address (disabled)", "", false},
		{"valid http URL", "http://localhost:6443/livez", false},
		{"valid https URL", "https://localhost:6443/livez", false},
		{"https with path", "https://127.0.0.1:6443/readyz?verbose", false},
		{"invalid URL", "not-a-url", true},
		{"ftp scheme", "ftp://localhost/file", true},
		{"tcp scheme", "tcp://localhost:6443", true},
		{"missing scheme", "localhost:6443/livez", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Config{ControlPlaneHealthCheck: HealthCheck{Address: tt.address}}
			err := c.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidate_HealthCheckGateVIP(t *testing.T) {
	gated := func(address string, bgp, leaderElection bool) *Config {
		return &Config{
			EnableBGP:               bgp,
			EnableLeaderElection:    leaderElection,
			ControlPlaneHealthCheck: HealthCheck{Address: address, GateVIP: true},
		}
	}
	const unsupported = "control_plane_health_check_gate_vip requires bgp_enable and control_plane_health_check_address, without vip_leaderelection"
	tests := []struct {
		name    string
		config  *Config
		wantErr string
	}{
		{
			name:   "disabled",
			config: &Config{ControlPlaneHealthCheck: HealthCheck{Address: "https://localhost:6443/livez"}},
		},
		{
			name:   "bgp without leader election",
			config: gated("https://localhost:6443/livez", true, false),
		},
		{
			name:    "without address",
			config:  gated("", true, false),
			wantErr: unsupported,
		},
		{
			name:    "without bgp",
			config:  gated("https://localhost:6443/livez", false, false),
			wantErr: unsupported,
		},
		{
			name:    "with leader election",
			config:  gated("https://localhost:6443/livez", true, true),
			wantErr: unsupported,
		},
		{
			name: "IP VIPs",
			config: &Config{
				EnableBGP:               true,
				Address:                 "192.0.2.10, 2001:db8::10",
				ControlPlaneHealthCheck: HealthCheck{Address: "https://localhost:6443/livez", GateVIP: true},
			},
		},
		{
			name: "hostname VIP",
			config: &Config{
				EnableBGP:               true,
				VIP:                     "cp.example.com",
				ControlPlaneHealthCheck: HealthCheck{Address: "https://localhost:6443/livez", GateVIP: true},
			},
			wantErr: `control_plane_health_check_gate_vip requires the VIP to be an IP address, got "cp.example.com"`,
		},
		{
			name: "hostname in address list",
			config: &Config{
				EnableBGP:               true,
				Address:                 "192.0.2.10,cp.example.com",
				ControlPlaneHealthCheck: HealthCheck{Address: "https://localhost:6443/livez", GateVIP: true},
			},
			wantErr: `control_plane_health_check_gate_vip requires the VIP to be an IP address, got "cp.example.com"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotErr := ""
			if err := tt.config.Validate(); err != nil {
				gotErr = err.Error()
			}
			if gotErr != tt.wantErr {
				t.Errorf("Validate() error = %q, want %q", gotErr, tt.wantErr)
			}
		})
	}
}

func TestValidate_Pprof(t *testing.T) {
	tests := []struct {
		name        string
		enablePprof bool
		addr        string
		wantErr     bool
	}{
		{name: "disabled without address"},
		{name: "disabled with address", addr: "127.0.0.1:6060"},
		{name: "enabled with address", enablePprof: true, addr: "127.0.0.1:6060"},
		{name: "enabled with default address", enablePprof: true, addr: metrics.DefaultPprofHTTPServer},
		{name: "enabled without address", enablePprof: true, wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := &Config{EnablePprof: test.enablePprof, PprofHTTPServer: test.addr}
			if err := config.Validate(); (err != nil) != test.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %t", err, test.wantErr)
			}
		})
	}
}

func TestValidate_HealthCheckClientCertificate(t *testing.T) {
	tests := []struct {
		name    string
		check   HealthCheck
		wantErr bool
	}{
		{name: "no client certificate", check: HealthCheck{}},
		{name: "certificate and key", check: HealthCheck{ClientCertPath: "/tls/client.crt", ClientKeyPath: "/tls/client.key"}},
		{name: "certificate without key", check: HealthCheck{ClientCertPath: "/tls/client.crt"}, wantErr: true},
		{name: "key without certificate", check: HealthCheck{ClientKeyPath: "/tls/client.key"}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := (&Config{ControlPlaneHealthCheck: tt.check}).Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %t", err, tt.wantErr)
			}
		})
	}
}

func TestValidate_InstanceName(t *testing.T) {
	tests := []struct {
		name         string
		instanceName string
		wantErr      bool
	}{
		{name: "empty uses legacy default", instanceName: "", wantErr: false},
		{name: "letters digits and separators", instanceName: "release_01.prod-a", wantErr: false},
		{name: "exact maximum length", instanceName: strings.Repeat("a", instanceNameMaxLength), wantErr: false},
		{name: "exceeds maximum length", instanceName: strings.Repeat("a", instanceNameMaxLength+1), wantErr: true},
		{name: "space", instanceName: "release a", wantErr: true},
		{name: "slash", instanceName: "namespace/release", wantErr: true},
		{name: "dollar sign", instanceName: "release$a", wantErr: true},
		{name: "at sign", instanceName: "release@a", wantErr: true},
		{name: "newline", instanceName: "release\na", wantErr: true},
		{name: "null byte", instanceName: "release\x00a", wantErr: true},
		{name: "unicode", instanceName: "rilascio-à", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := &Config{InstanceName: tt.instanceName}
			err := config.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %t", err, tt.wantErr)
			}
		})
	}
}

func TestValidate_RoutingProtocol(t *testing.T) {
	tests := []struct {
		name     string
		protocol int
		wantErr  bool
	}{
		{name: "unset", protocol: 0, wantErr: false},
		{name: "kube-vip default", protocol: 248, wantErr: false},
		{name: "maximum byte value", protocol: 255, wantErr: false},
		{name: "truncated on the wire", protocol: 256, wantErr: true},
		{name: "negative", protocol: -1, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := &Config{RoutingProtocol: tt.protocol}
			err := config.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %t", err, tt.wantErr)
			}
		})
	}
}

func TestValidate_BMP(t *testing.T) {
	tests := []struct {
		name    string
		bmp     BMPConfig
		wantErr string
	}{
		{name: "unset"},
		{name: "valid", bmp: BMPConfig{Servers: []string{"192.0.2.20", "[2001:db8::20]:5000"}, Policy: BMPPolicyAll, StatisticsTimeout: 15}},
		{name: "longest statistics timeout", bmp: BMPConfig{Servers: []string{"192.0.2.20"}, StatisticsTimeout: 65535}},
		{name: "invalid server", bmp: BMPConfig{Servers: []string{"192.0.2.20:x"}}, wantErr: `BMP server "192.0.2.20:x" is invalid`},
		{name: "invalid policy", bmp: BMPConfig{Policy: "both"}, wantErr: `BMP route monitoring policy "both" is invalid`},
		{name: "statistics timeout too short", bmp: BMPConfig{StatisticsTimeout: 14}, wantErr: "BMP statistics timeout 14 is invalid"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := &Config{BGPConfig: BGPConfig{BMP: tt.bmp}}
			err := config.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() error = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestInstanceNameLimitReservesNftablesPrefixAndFamilySuffix(t *testing.T) {
	name := strings.Repeat("a", instanceNameMaxLength)
	if got := len(egressNftablesTablePrefix + name + egressNftablesTableSuffix); got != nftablesNameMaxLength {
		t.Fatalf("family-specific table name length = %d, want %d", got, nftablesNameMaxLength)
	}
}
