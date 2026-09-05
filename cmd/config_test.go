package cmd

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/spf13/cobra"
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
	return cmd
}

func writeRuntimeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
