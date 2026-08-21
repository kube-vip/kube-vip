package deployment

import "testing"

func TestBackendLabelsExcludeKubeVIPDaemonSet(t *testing.T) {
	const namespace = "kube-vip-egress"

	backend := backendLabels(namespace)
	daemonSet := buildKVDsDaemonSet(namespace, "kube-vip:test", "", false)

	if backend["app.kubernetes.io/instance"] != namespace {
		t.Fatalf("backend instance label = %q, want %q", backend["app.kubernetes.io/instance"], namespace)
	}

	matches := true
	for key, value := range backend {
		if daemonSet.Spec.Template.Labels[key] != value {
			matches = false
			break
		}
	}
	if matches {
		t.Fatal("backend selector unexpectedly matches kube-vip DaemonSet pods")
	}
}
