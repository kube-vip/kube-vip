package cmd

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

func TestKubeadmJoinMountsValidatedKubeconfig(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "admin.conf")
	if err := os.WriteFile(configPath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	symlinkPath := filepath.Join(t.TempDir(), "admin.conf")
	if err := os.Symlink(configPath, symlinkPath); err != nil {
		t.Fatal(err)
	}
	workingDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relativePath, err := filepath.Rel(workingDir, configPath)
	if err != nil {
		t.Fatal(err)
	}
	missingPath := filepath.Join(t.TempDir(), "missing.conf")
	tests := []struct {
		name         string
		flags        []string
		env          map[string]string
		wantHostPath string
	}{
		{name: "join flag", flags: []string{"--config", configPath}, wantHostPath: configPath},
		{name: "relative join flag", flags: []string{"--config", relativePath}, wantHostPath: configPath},
		{name: "symlink", flags: []string{"--config", symlinkPath}, wantHostPath: symlinkPath},
		{name: "persistent flag", flags: []string{"--k8sConfigPath", configPath}, wantHostPath: configPath},
		{name: "environment", env: map[string]string{"k8s_config_file": configPath}, wantHostPath: configPath},
		{
			name:         "join flag takes precedence",
			flags:        []string{"--config", configPath, "--k8sConfigPath", missingPath},
			env:          map[string]string{"k8s_config_file": missingPath},
			wantHostPath: configPath,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := []string{"kubeadm", "join", "--interface", "eth0", "--address", "192.0.2.100"}
			stdout, stderr, err := runLoggingProcess(t, "command", tt.env, append(args, tt.flags...)...)
			if err != nil || stderr != "" {
				t.Fatalf("join failed: err=%v stderr=%q", err, stderr)
			}
			var pod corev1.Pod
			if err := yaml.Unmarshal([]byte(stdout), &pod); err != nil {
				t.Fatalf("invalid Pod manifest: %v", err)
			}
			if len(pod.Spec.Volumes) != 1 || pod.Spec.Volumes[0].Name != "kubeconfig" ||
				pod.Spec.Volumes[0].HostPath == nil || pod.Spec.Volumes[0].HostPath.Path != tt.wantHostPath {
				t.Fatalf("expected kubeconfig volume for %q, got %+v", tt.wantHostPath, pod.Spec.Volumes)
			}
			if len(pod.Spec.Containers) != 1 {
				t.Fatalf("unexpected Pod containers: %+v", pod.Spec.Containers)
			}
			mounts := pod.Spec.Containers[0].VolumeMounts
			if len(mounts) != 1 || mounts[0].Name != "kubeconfig" || mounts[0].MountPath != "/etc/kubernetes/admin.conf" {
				t.Fatalf("unexpected kubeconfig mount: %+v", mounts)
			}
		})
	}
}

func TestKubeadmJoinRejectsInvalidConfiguration(t *testing.T) {
	configDir := t.TempDir()
	missingPath := filepath.Join(configDir, "missing.conf")
	requiredFlags := []string{"--interface", "eth0", "--address", "192.0.2.100"}
	tests := []struct {
		name       string
		flags      []string
		diagnostic string
	}{
		{name: "missing interface", flags: []string{"--address", "192.0.2.100"}, diagnostic: "no interface"},
		{name: "missing address", flags: []string{"--interface", "eth0"}, diagnostic: "no address"},
		{name: "missing kubeconfig", flags: append(requiredFlags, "--k8sConfigPath", missingPath), diagnostic: missingPath},
		{name: "kubeconfig directory", flags: append(requiredFlags, "--config", configDir), diagnostic: "must be a regular file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := append([]string{"kubeadm", "join"}, tt.flags...)
			stdout, stderr, err := runLoggingProcess(t, "command", nil, args...)
			if err == nil || stdout != "" || !strings.Contains(stderr, tt.diagnostic) {
				t.Fatalf("expected join failure with %q: err=%v stdout=%q stderr=%q", tt.diagnostic, err, stdout, stderr)
			}
		})
	}
}

func TestKubeadmInitReturnsErrors(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{"invalid config", []string{"--interface=eth0", "--vip=192.0.2.100", "--logFormat=yaml"}, "expected text or json"},
		{"missing interface", []string{"--vip=192.0.2.100"}, "no interface"},
		{"missing address", []string{"--interface=eth0"}, "no address"},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := append([]string{"kubeadm", "init"}, test.args...)
			stdout, stderr, err := runLoggingProcess(t, "command", nil, args...)
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 || stdout != "" || !strings.Contains(stderr, test.want) {
				t.Fatalf("expected exit code 1 and %q on stderr: err=%v stdout=%q stderr=%q", test.want, err, stdout, stderr)
			}
		})
	}
}

func TestKubeadmInitWritesManifest(t *testing.T) {
	stdout, stderr, err := runLoggingProcess(t, "command", nil, "kubeadm", "init", "--interface=eth0", "--vip=192.0.2.100", "--controlplane")
	if err != nil || stderr != "" {
		t.Fatalf("init failed: err=%v stderr=%q", err, stderr)
	}
	var pod corev1.Pod
	if err := yaml.Unmarshal([]byte(stdout), &pod); err != nil {
		t.Fatalf("invalid Pod manifest: %v", err)
	}
	if pod.Kind != "Pod" || len(pod.Spec.Containers) != 1 {
		t.Fatalf("unexpected Pod manifest: %+v", pod)
	}
}
