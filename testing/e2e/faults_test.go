//go:build e2e

package e2e

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

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
