package kubevip

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

type configFieldSource struct {
	category string
	reason   string
}

const (
	configSourceMergeable = "mergeable"
	configSourceDerived   = "derived"
	configSourceBootstrap = "bootstrap"
	configSourceGenerator = "generator"
)

var nonMergeableConfigFields = map[string]configFieldSource{
	"AddPeersAsBackends": {
		category: configSourceGenerator,
		reason:   "controls generated control-plane manifests rather than manager startup",
	},
	"BGPPeers": {
		category: configSourceGenerator,
		reason:   "legacy CLI input is parsed into BGPConfig.Peers before runtime use",
	},
	"ConfigFile": {
		category: configSourceBootstrap,
		reason:   "selects the file to load and cannot be selected by that same file",
	},
	"IsDualStack": {
		category: configSourceDerived,
		reason:   "computed from a Service IP family at reconciliation time",
	},
	"LoadBalancers": {
		category: configSourceGenerator,
		reason:   "describes generated IPVS manifests rather than manager configuration",
	},
	"RequireDualStack": {
		category: configSourceDerived,
		reason:   "computed from a Service IP family policy at reconciliation time",
	},
	"SingleNode": {
		category: configSourceGenerator,
		reason:   "controls generated control-plane manifests rather than manager startup",
	},
	"StartAsLeader": {
		category: configSourceGenerator,
		reason:   "controls generated control-plane manifests rather than manager startup",
	},
}

func TestConfigFieldsHaveSourceSemantics(t *testing.T) {
	typ := reflect.TypeOf(Config{})
	counts := map[string]int{}

	for name, source := range nonMergeableConfigFields {
		switch source.category {
		case configSourceDerived, configSourceBootstrap, configSourceGenerator:
		default:
			t.Errorf("Config.%s has invalid source category %q", name, source.category)
		}
		if source.reason == "" {
			t.Errorf("Config.%s classification must include category and reason", name)
		}
		if _, ok := typ.FieldByName(name); !ok {
			t.Errorf("stale Config.%s classification", name)
		}
	}

	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		source := configSourceForField(field.Name)
		if source.category == "" || source.reason == "" {
			t.Errorf("Config.%s has incomplete source semantics", field.Name)
		}
		counts[source.category]++
	}
	for _, category := range []string{configSourceMergeable, configSourceDerived, configSourceBootstrap, configSourceGenerator} {
		if counts[category] == 0 {
			t.Errorf("no Config fields classified as %s", category)
		}
	}
}

func configSourceForField(name string) configFieldSource {
	if source, ok := nonMergeableConfigFields[name]; ok {
		return source
	}
	return configFieldSource{
		category: configSourceMergeable,
		reason:   "accepted from file configuration and overlaid by higher-priority sources",
	}
}

func TestConfigFileLoaderUsesExternalNames(t *testing.T) {
	path := writeConfigSourceFile(t, ".json", `{
  "enableBGP": true,
  "bgpConfig": {"routerID": "192.0.2.1", "as": 65000}
}`)

	config, err := LoadConfigFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !config.EnableBGP || config.BGPConfig.RouterID != "192.0.2.1" || config.BGPConfig.AS != 65000 {
		t.Fatalf("decoded config = %#v", config)
	}
}

func TestConfigFileMergePopulatesDestination(t *testing.T) {
	path := writeConfigSourceFile(t, ".yaml", `enableARP: true
port: 7443
interface: eth1
`)
	config := &Config{}

	if err := MergeConfigFromFile(config, path); err != nil {
		t.Fatal(err)
	}
	if !config.EnableARP || config.Port != 7443 || config.Interface != "eth1" {
		t.Fatalf("merged config = %#v", config)
	}
}

func TestConfigEnvironmentOverridesLoadedFile(t *testing.T) {
	path := writeConfigSourceFile(t, ".yaml", `enableARP: true
port: 7443
`)
	config := &Config{}

	if err := MergeConfigFromFile(config, path); err != nil {
		t.Fatal(err)
	}
	t.Setenv(vipArp, "false")
	t.Setenv(port, "8443")
	if err := ParseEnvironment(config); err != nil {
		t.Fatal(err)
	}
	if config.EnableARP || config.Port != 8443 {
		t.Fatalf("environment did not override file values: %#v", config)
	}
}

func writeConfigSourceFile(t *testing.T, extension, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config"+extension)
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
