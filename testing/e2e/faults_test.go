//go:build e2e

package e2e

import (
	"errors"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestKillAndSuppressKubeVip(t *testing.T) {
	tests := []struct {
		name        string
		failAt      int
		pidOutput   string
		wantPID     string
		wantError   bool
		wantRestore bool
	}{
		{name: "kills exact process after stopping kubelet", pidOutput: "123\n", wantPID: "123"},
		{name: "restores after stop reports failure", failAt: 1, wantError: true, wantRestore: true},
		{name: "restores after inactive check fails", failAt: 2, wantError: true, wantRestore: true},
		{name: "restores after process lookup fails", failAt: 3, wantError: true, wantRestore: true},
		{name: "restores when multiple processes exist", pidOutput: "123\n456\n", wantError: true, wantRestore: true},
		{name: "restores after kill fails", failAt: 4, pidOutput: "123\n", wantError: true, wantRestore: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var calls [][]string
			call := 0
			run := func(_ string, args ...string) error {
				call++
				calls = append(calls, append([]string(nil), args...))
				if call == test.failAt {
					return errors.New("injected failure")
				}
				return nil
			}
			output := func(_ string, args ...string) (string, error) {
				call++
				calls = append(calls, append([]string(nil), args...))
				if call == test.failAt {
					return "", errors.New("injected failure")
				}
				return test.pidOutput, nil
			}

			pid, err := killAndSuppressKubeVip("cluster", "node", run, output)
			if (err != nil) != test.wantError {
				t.Fatalf("killAndSuppressKubeVip() error = %v, wantError %t", err, test.wantError)
			}
			if pid != test.wantPID {
				t.Fatalf("killAndSuppressKubeVip() PID = %q, want %q", pid, test.wantPID)
			}
			if !reflect.DeepEqual(calls[0], []string{"exec", "node", "systemctl", "stop", "kubelet"}) {
				t.Fatalf("first call = %v, want kubelet stop", calls[0])
			}
			restored := false
			for _, got := range calls {
				if reflect.DeepEqual(got, []string{"exec", "node", "systemctl", "start", "kubelet"}) {
					restored = true
				}
			}
			if restored != test.wantRestore {
				t.Fatalf("kubelet restored = %t, want %t; calls: %v", restored, test.wantRestore, calls)
			}
			if !test.wantError && !reflect.DeepEqual(calls[len(calls)-1], []string{"exec", "node", "kill", "-KILL", "123"}) {
				t.Fatalf("last call = %v, want exact PID kill", calls[len(calls)-1])
			}
		})
	}
}

func TestSinglePID(t *testing.T) {
	tests := []struct {
		name      string
		output    string
		want      string
		wantError bool
	}{
		{name: "one process", output: "123\n", want: "123"},
		{name: "no process", wantError: true},
		{name: "multiple processes", output: "123\n456\n", wantError: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := singlePID(test.output)
			if (err != nil) != test.wantError {
				t.Fatalf("singlePID() error = %v, wantError %t", err, test.wantError)
			}
			if got != test.want {
				t.Fatalf("singlePID() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestRestoreKubelet(t *testing.T) {
	tests := []struct {
		name      string
		failAt    int
		err       error
		wantError bool
		wantCalls int
	}{
		{name: "starts active kubelet", wantCalls: 2},
		{name: "already deleted before start", failAt: 1, err: errors.New("docker exec: No such container: node"), wantCalls: 1},
		{name: "deleted before active check", failAt: 2, err: errors.New("No such container: node"), wantCalls: 2},
		{name: "start failure", failAt: 1, err: errors.New("injected failure"), wantError: true, wantCalls: 1},
		{name: "inactive kubelet", failAt: 2, err: errors.New("inactive"), wantError: true, wantCalls: 2},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			run := func(_ string, _ ...string) error {
				calls++
				if calls == test.failAt {
					return test.err
				}
				return nil
			}

			err := restoreKubelet("cluster", "node", run)
			if (err != nil) != test.wantError {
				t.Fatalf("restoreKubelet() error = %v, wantError %t", err, test.wantError)
			}
			if calls != test.wantCalls {
				t.Fatalf("restoreKubelet() calls = %d, want %d", calls, test.wantCalls)
			}
		})
	}
}

func TestPodManifestPaths(t *testing.T) {
	tests := []struct {
		name      string
		wantSrc   string
		wantDst   string
		wantError bool
	}{
		{name: "kube-apiserver.yaml", wantSrc: "/etc/kubernetes/manifests/kube-apiserver.yaml", wantDst: "/tmp/kube-apiserver.yaml"},
		{name: "", wantError: true},
		{name: "../manifest.yaml", wantError: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			src, dst, err := podManifestPaths(test.name)
			if (err != nil) != test.wantError {
				t.Fatalf("podManifestPaths() error = %v, wantError %t", err, test.wantError)
			}
			if src != test.wantSrc || dst != test.wantDst {
				t.Fatalf("podManifestPaths() = (%q, %q), want (%q, %q)", src, dst, test.wantSrc, test.wantDst)
			}
		})
	}
}

func TestNodeIsReady(t *testing.T) {
	if nodeIsReady(&corev1.Node{}) {
		t.Fatal("nodeIsReady() = true without a Ready condition")
	}
	node := &corev1.Node{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	if !nodeIsReady(node) {
		t.Fatal("nodeIsReady() = false for Ready=True")
	}
}
