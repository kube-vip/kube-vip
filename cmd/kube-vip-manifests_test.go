package cmd

import (
	"errors"
	"os/exec"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

func TestManifestCommandsReturnErrors(t *testing.T) {
	for _, command := range []string{"pod", "daemonset"} {
		t.Run(command, func(t *testing.T) {
			for _, test := range []struct {
				name string
				args []string
				want string
			}{
				{"invalid config", []string{"--logFormat=yaml"}, "expected text or json"},
				{"missing control-plane address", []string{"--controlplane"}, "no address is specified"},
				{"invalid image", []string{"--image=invalid image"}, "unable to create manifest"},
			} {
				t.Run(test.name, func(t *testing.T) {
					args := append([]string{"manifest", command, "--interface=eth0", "--arp"}, test.args...)
					stdout, stderr, err := runLoggingProcess(t, "command", nil, args...)
					var exitErr *exec.ExitError
					if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 || stdout != "" || !strings.Contains(stderr, test.want) {
						t.Fatalf("expected exit code 1 and %q on stderr: err=%v stdout=%q stderr=%q", test.want, err, stdout, stderr)
					}
				})
			}
		})
	}
}

func TestManifestCommandsWriteYAML(t *testing.T) {
	for _, test := range []struct {
		command string
		kind    string
	}{
		{"pod", "Pod"},
		{"daemonset", "DaemonSet"},
	} {
		t.Run(test.command, func(t *testing.T) {
			stdout, stderr, err := runLoggingProcess(t, "command", nil, "manifest", test.command, "--interface=eth0", "--arp", "--services", "--inCluster")
			if err != nil || stderr != "" {
				t.Fatalf("manifest generation failed: err=%v stderr=%q", err, stderr)
			}
			var manifest metav1.TypeMeta
			if err := yaml.Unmarshal([]byte(stdout), &manifest); err != nil {
				t.Fatalf("manifest is not valid YAML: %v", err)
			}
			if manifest.Kind != test.kind {
				t.Fatalf("manifest kind = %q, want %q", manifest.Kind, test.kind)
			}
		})
	}
}
