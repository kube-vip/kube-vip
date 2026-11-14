package kubevip

import (
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

func TestValidateLogFormat(t *testing.T) {
	for _, format := range []string{"", "text", "json", "JSON", "yaml", " json", "json\n"} {
		t.Run(format, func(t *testing.T) {
			config := &Config{LogFormat: format}
			err := config.Validate()
			valid := format == "" || format == "text" || format == "json"
			if (err == nil) != valid {
				t.Fatalf("Validate() = %v for logFormat %q", err, format)
			}
			if err != nil && !strings.Contains(err.Error(), "logFormat") {
				t.Fatalf("error does not identify logFormat: %v", err)
			}
		})
	}
}

func TestLogFormatConfigFile(t *testing.T) {
	for ext, content := range map[string]string{
		".yaml": "logFormat: json\nlogging: -4\nlogTimestampFormat: unix-ms\n",
		".json": `{"logFormat":"json","logging":-4,"logTimestampFormat":"unix-ms"}`,
	} {
		t.Run(ext, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config"+ext)
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			config, err := LoadConfigFromFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if config.LogFormat != "json" || config.Logging != -4 || config.LogTimestampFormat != "unix-ms" {
				t.Fatalf("logging = (%q, %d, %q), want (json, -4, unix-ms)", config.LogFormat, config.Logging, config.LogTimestampFormat)
			}
		})
	}
}

func TestGeneratedManifestLogFormat(t *testing.T) {
	tests := []struct {
		name      string
		format    string
		timestamp string
	}{
		{name: "defaults"},
		{name: "text", format: "text"},
		{name: "json", format: "json"},
		{name: "json RFC3339", format: "json", timestamp: "rfc3339"},
		{name: "json Unix milliseconds", format: "json", timestamp: "unix-ms"},
		{name: "text ignores timestamp", format: "text", timestamp: "unix-ms"},
	}
	for _, tt := range tests {
		for _, kind := range []string{"Pod", "DaemonSet"} {
			t.Run(kind+"/"+tt.name, func(t *testing.T) {
				config := &Config{Interface: "eth0", EnableARP: true, EnableServices: true, LogFormat: tt.format, LogTimestampFormat: tt.timestamp}
				var spec corev1.PodSpec
				if kind == "Pod" {
					manifest, err := GeneratePodManifestFromConfig(config, "kube-vip", "test", false)
					if err != nil {
						t.Fatal(err)
					}
					var pod corev1.Pod
					if err := yaml.Unmarshal([]byte(manifest), &pod); err != nil {
						t.Fatal(err)
					}
					spec = pod.Spec
				} else {
					manifest, err := GenerateDaemonsetManifestFromConfig(config, "kube-vip", "test", true, false)
					if err != nil {
						t.Fatal(err)
					}
					var daemonset appsv1.DaemonSet
					if err := yaml.Unmarshal([]byte(manifest), &daemonset); err != nil {
						t.Fatal(err)
					}
					spec = daemonset.Spec.Template.Spec
				}
				got := map[string]string{}
				for _, env := range spec.Containers[0].Env {
					if env.Name == vipLogFormat || env.Name == vipLogTimestampFormat {
						if _, found := got[env.Name]; found {
							t.Fatalf("duplicate generated environment variable %q", env.Name)
						}
						got[env.Name] = env.Value
					}
				}
				want := map[string]string{}
				if tt.format == "json" {
					want[vipLogFormat] = "json"
				}
				if tt.timestamp == "unix-ms" {
					want[vipLogTimestampFormat] = "unix-ms"
				}
				if !maps.Equal(got, want) {
					t.Fatalf("generated logging environment = %#v, want %#v", got, want)
				}
			})
		}
	}
}

func TestValidateLogTimestampFormat(t *testing.T) {
	for _, format := range []string{"", "rfc3339", "unix-ms", "RFC3339", "unix", "unix_ms", "epoch", "unix-ms ", "unix-ms\n"} {
		for _, logFormat := range []string{"", "text", "json"} {
			t.Run(logFormat+"/"+format, func(t *testing.T) {
				config := &Config{LogFormat: logFormat, LogTimestampFormat: format}
				err := config.Validate()
				valid := format == "" || format == "rfc3339" || format == "unix-ms"
				if (err == nil) != valid {
					t.Fatalf("Validate() = %v for logTimestampFormat %q", err, format)
				}
				if err != nil && !strings.Contains(err.Error(), "logTimestampFormat") {
					t.Fatalf("error does not identify logTimestampFormat: %v", err)
				}
			})
		}
	}
}
