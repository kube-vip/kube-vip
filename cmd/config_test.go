package cmd

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func TestRuntimeCommandConfigPrecedence(t *testing.T) {
	for _, commandName := range []string{"manager", "service"} {
		t.Run(commandName, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			content := `enableARP: true
port: 1111
prometheusHTTPServer: file
lbClassNameLegacyHandling: false
`
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("config_file", path)
			t.Setenv("vip_arp", "false")
			t.Setenv("port", "2222")
			t.Setenv("prometheus_server", "")

			cmd := newRuntimeConfigTestCommand(commandName)
			if err := cmd.ParseFlags([]string{"--port=0", "--lbClassNameLegacyHandling=true"}); err != nil {
				t.Fatal(err)
			}
			if err := loadRuntimeConfig(cmd); err != nil {
				t.Fatal(err)
			}

			if initConfig.EnableARP || initConfig.Port != 0 || initConfig.PrometheusHTTPServer != "" || !initConfig.LoadBalancerClassLegacyHandling {
				t.Fatalf("wrong precedence result: %#v", initConfig)
			}
			if initConfig.ConfigFile != path {
				t.Fatalf("ConfigFile = %q, want %q", initConfig.ConfigFile, path)
			}
		})
	}
}

func TestRuntimeCommandConfigFileFlagWins(t *testing.T) {
	envPath := writeRuntimeConfig(t, "port: 1111\n")
	flagPath := writeRuntimeConfig(t, "port: 2222\n")
	t.Setenv("config_file", envPath)

	cmd := newRuntimeConfigTestCommand("manager")
	if err := cmd.ParseFlags([]string{"--config-file=" + flagPath}); err != nil {
		t.Fatal(err)
	}
	if err := loadRuntimeConfig(cmd); err != nil {
		t.Fatal(err)
	}
	if initConfig.Port != 2222 || initConfig.ConfigFile != flagPath {
		t.Fatalf("flag config file did not win: %#v", initConfig)
	}
}

func TestRuntimeCommandDefaultsWhenSourcesOmitted(t *testing.T) {
	cmd := newRuntimeConfigTestCommand("service")
	if err := loadRuntimeConfig(cmd); err != nil {
		t.Fatal(err)
	}
	if initConfig.Port != 6443 || initConfig.DHCPMode != "ipv4" || initConfig.LoadBalancerClassName != kubevip.LBClassName || !initConfig.EgressWithNftables {
		t.Fatalf("defaults changed: %#v", initConfig)
	}
}

func TestRuntimeCommandAbsentEnvironmentPreservesFile(t *testing.T) {
	path := writeRuntimeConfig(t, "enableARP: true\nport: 1234\nprometheusHTTPServer: file\n")
	cmd := newRuntimeConfigTestCommand("manager")
	if err := cmd.ParseFlags([]string{"--config-file=" + path}); err != nil {
		t.Fatal(err)
	}
	if err := loadRuntimeConfig(cmd); err != nil {
		t.Fatal(err)
	}
	if !initConfig.EnableARP || initConfig.Port != 1234 || initConfig.PrometheusHTTPServer != "file" {
		t.Fatalf("absent environment overwrote file: %#v", initConfig)
	}
}

func TestRuntimeCommandDerivesDHCPFromFileDNSMode(t *testing.T) {
	t.Setenv("dhcp_mode", "")
	os.Unsetenv("dhcp_mode")
	t.Setenv("dns_mode", "")
	os.Unsetenv("dns_mode")
	path := writeRuntimeConfig(t, "dnsDualStackMode: dual\n")
	cmd := newRuntimeConfigTestCommand("manager")
	if err := cmd.ParseFlags([]string{"--config-file=" + path}); err != nil {
		t.Fatal(err)
	}
	if err := loadRuntimeConfig(cmd); err != nil {
		t.Fatal(err)
	}
	if initConfig.DNSMode != "dual" || initConfig.DHCPMode != "dual" {
		t.Fatalf("file DNS mode did not derive DHCP mode: DNS=%q DHCP=%q", initConfig.DNSMode, initConfig.DHCPMode)
	}
}

func TestRuntimeCommandExplicitEmptyPeers(t *testing.T) {
	path := writeRuntimeConfig(t, "bgpConfig:\n  peers:\n    - address: 192.0.2.1\n      as: 65001\n")
	cmd := newRuntimeConfigTestCommand("manager")
	cmd.Flags().StringSliceVar(&initConfig.BGPPeers, "bgppeers", nil, "")
	if err := cmd.ParseFlags([]string{"--config-file=" + path, "--bgppeers="}); err != nil {
		t.Fatal(err)
	}
	if err := loadRuntimeConfig(cmd); err != nil {
		t.Fatal(err)
	}
	if len(initConfig.BGPConfig.Peers) != 0 {
		t.Fatalf("explicit empty peers did not clear file peers: %#v", initConfig.BGPConfig.Peers)
	}
}

func TestRuntimeCommandSliceFlagPrecedence(t *testing.T) {
	path := writeRuntimeConfig(t, "etcd:\n  endpoints: [file:2379]\n")
	cmd := newRuntimeConfigTestCommand("manager")
	cmd.Flags().StringSliceVar(&initConfig.Etcd.Endpoints, "etcdEndpoints", nil, "")
	if err := cmd.ParseFlags([]string{"--config-file=" + path, "--etcdEndpoints=flag-a:2379,flag-b:2379"}); err != nil {
		t.Fatal(err)
	}
	if err := loadRuntimeConfig(cmd); err != nil {
		t.Fatal(err)
	}
	want := []string{"flag-a:2379", "flag-b:2379"}
	if !slices.Equal(initConfig.Etcd.Endpoints, want) {
		t.Fatalf("Etcd.Endpoints = %#v, want %#v", initConfig.Etcd.Endpoints, want)
	}
}

func newRuntimeConfigTestCommand(name string) *cobra.Command {
	initConfig = kubevip.Config{ArpBroadcastRate: 3000}
	cmd := &cobra.Command{Use: name}
	cmd.Flags().Uint16Var(&initConfig.Port, "port", 6443, "")
	cmd.Flags().BoolVar(&initConfig.EnableARP, "arp", false, "")
	cmd.Flags().StringVar(&initConfig.DHCPMode, "dhcpMode", "ipv4", "")
	cmd.Flags().StringVar(&initConfig.PrometheusHTTPServer, "prometheusHTTPServer", ":2112", "")
	cmd.Flags().BoolVar(&initConfig.LoadBalancerClassLegacyHandling, "lbClassNameLegacyHandling", true, "")
	cmd.Flags().StringVar(&initConfig.LoadBalancerClassName, "lbClassName", kubevip.LBClassName, "")
	cmd.Flags().BoolVar(&initConfig.EgressWithNftables, "egressWithNftables", true, "")
	cmd.Flags().StringVar(&initConfig.ConfigFile, "config-file", "", "")
	cmd.Flags().StringVar(&initConfig.BGPPeerConfig.Address, "peerAddress", "", "")
	cmd.Flags().Uint32Var(&initConfig.BGPPeerConfig.AS, "peerAS", 65000, "")
	cmd.Flags().Int32Var(&initConfig.Logging, "log", 0, "")
	cmd.Flags().StringVar(&initConfig.LogFormat, "logFormat", "text", "")
	cmd.Flags().StringVar(&initConfig.LogTimestampFormat, "logTimestampFormat", "rfc3339", "")
	return cmd
}

func TestRuntimeLoggingConfigPrecedence(t *testing.T) {
	tests := []struct {
		name       string
		file       string
		env        map[string]string
		flags      []string
		wantFormat string
		wantLevel  int32
	}{
		{name: "defaults", wantFormat: "text"},
		{name: "file", file: "logFormat: json\nlogging: -4\n", wantFormat: "json", wantLevel: -4},
		{name: "environment", file: "logFormat: text\nlogging: 8\n", env: map[string]string{"vip_logformat": "json", "vip_loglevel": "-4"}, wantFormat: "json", wantLevel: -4},
		{name: "flags", file: "logFormat: json\nlogging: -4\n", env: map[string]string{"vip_logformat": "json", "vip_loglevel": "8"}, flags: []string{"--logFormat=text", "--log=0"}, wantFormat: "text"},
		{name: "JSON flag", env: map[string]string{"vip_logformat": "text"}, flags: []string{"--logFormat=json"}, wantFormat: "json"},
		{name: "empty environment", file: "logFormat: json\n", env: map[string]string{"vip_logformat": ""}},
		{name: "empty flag", env: map[string]string{"vip_logformat": "json"}, flags: []string{"--logFormat="}},
		{name: "overridden invalid file", file: "logFormat: invalid\n", env: map[string]string{"vip_logformat": "json"}, wantFormat: "json"},
		{name: "overridden invalid environment", env: map[string]string{"vip_logformat": "invalid"}, flags: []string{"--logFormat=json"}, wantFormat: "json"},
	}
	for _, commandName := range []string{"manager", "service"} {
		for _, tt := range tests {
			t.Run(commandName+"/"+tt.name, func(t *testing.T) {
				cmd := newRuntimeConfigTestCommand(commandName)
				if tt.file != "" {
					t.Setenv("config_file", writeRuntimeConfig(t, tt.file))
				}
				for name, value := range tt.env {
					t.Setenv(name, value)
				}
				if err := cmd.ParseFlags(tt.flags); err != nil {
					t.Fatal(err)
				}
				if err := loadRuntimeConfig(cmd); err != nil {
					t.Fatal(err)
				}
				if err := initConfig.Validate(); err != nil {
					t.Fatal(err)
				}
				if initConfig.LogFormat != tt.wantFormat || initConfig.Logging != tt.wantLevel {
					t.Fatalf("logging = (%q, %d), want (%q, %d)", initConfig.LogFormat, initConfig.Logging, tt.wantFormat, tt.wantLevel)
				}
			})
		}
	}
}

func TestRuntimeLogTimestampConfigPrecedence(t *testing.T) {
	tests := []struct {
		name  string
		file  string
		env   map[string]string
		flags []string
		want  string
	}{
		{name: "default", want: "rfc3339"},
		{name: "file", file: "logTimestampFormat: unix-ms\n", want: "unix-ms"},
		{name: "environment", file: "logTimestampFormat: rfc3339\n", env: map[string]string{"vip_logtimestampformat": "unix-ms"}, want: "unix-ms"},
		{name: "flag", file: "logTimestampFormat: unix-ms\n", env: map[string]string{"vip_logtimestampformat": "unix-ms"}, flags: []string{"--logTimestampFormat=rfc3339"}, want: "rfc3339"},
		{name: "empty environment", file: "logTimestampFormat: unix-ms\n", env: map[string]string{"vip_logtimestampformat": ""}},
		{name: "empty flag", env: map[string]string{"vip_logtimestampformat": "unix-ms"}, flags: []string{"--logTimestampFormat="}},
		{name: "invalid file overridden", file: "logTimestampFormat: invalid\n", env: map[string]string{"vip_logtimestampformat": "unix-ms"}, want: "unix-ms"},
		{name: "invalid environment overridden", env: map[string]string{"vip_logtimestampformat": "invalid"}, flags: []string{"--logTimestampFormat=unix-ms"}, want: "unix-ms"},
	}
	for _, commandName := range []string{"manager", "service"} {
		for _, tt := range tests {
			t.Run(commandName+"/"+tt.name, func(t *testing.T) {
				cmd := newRuntimeConfigTestCommand(commandName)
				if tt.file != "" {
					t.Setenv("config_file", writeRuntimeConfig(t, tt.file))
				}
				for name, value := range tt.env {
					t.Setenv(name, value)
				}
				if err := cmd.ParseFlags(tt.flags); err != nil {
					t.Fatal(err)
				}
				if err := loadRuntimeConfig(cmd); err != nil {
					t.Fatal(err)
				}
				if err := initConfig.Validate(); err != nil {
					t.Fatal(err)
				}
				if initConfig.LogTimestampFormat != tt.want {
					t.Fatalf("LogTimestampFormat = %q, want %q", initConfig.LogTimestampFormat, tt.want)
				}
			})
		}
	}
}

func writeRuntimeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRuntimeCommandFilePreservesFlagPeerDefaults(t *testing.T) {
	path := writeRuntimeConfig(t, "port: 1234\n")
	cmd := newRuntimeConfigTestCommand("manager")
	if err := cmd.ParseFlags([]string{"--config-file=" + path, "--peerAddress=192.0.2.10"}); err != nil {
		t.Fatal(err)
	}
	if err := loadRuntimeConfig(cmd); err != nil {
		t.Fatal(err)
	}
	if len(initConfig.BGPConfig.Peers) != 1 {
		t.Fatalf("BGP peers = %#v, want one peer from --peerAddress", initConfig.BGPConfig.Peers)
	}
	if peer := initConfig.BGPConfig.Peers[0]; peer.Address != "192.0.2.10" || peer.AS != 65000 {
		t.Fatalf("BGP peer = %#v, want address 192.0.2.10 with default AS 65000", peer)
	}
}

func TestLoseLeadershipTimeoutFlagNames(t *testing.T) {
	const flagName, deprecatedName = "loseLeadershipTimeoutSeconds", "loseLeadershiptTimeoutSeconds"

	deprecated := kubeVipCmd.PersistentFlags().Lookup(deprecatedName)
	if deprecated == nil || deprecated.Deprecated == "" {
		t.Fatalf("--%s should remain available as a deprecated flag", deprecatedName)
	}

	for _, name := range []string{flagName, deprecatedName} {
		t.Run(name, func(t *testing.T) {
			flag := kubeVipCmd.PersistentFlags().Lookup(name)
			if flag == nil {
				t.Fatalf("--%s is not registered", name)
			}
			original := initConfig.LoseLeadershipTimeoutSeconds
			t.Cleanup(func() {
				initConfig.LoseLeadershipTimeoutSeconds = original
				flag.Changed = false
			})

			flags := pflag.NewFlagSet(name, pflag.ContinueOnError)
			flags.SetOutput(io.Discard)
			flags.AddFlag(flag)
			if err := flags.Parse([]string{"--" + name + "=10"}); err != nil {
				t.Fatal(err)
			}
			if initConfig.LoseLeadershipTimeoutSeconds != 10 {
				t.Fatalf("LoseLeadershipTimeoutSeconds = %d, want 10", initConfig.LoseLeadershipTimeoutSeconds)
			}
		})
	}
}
