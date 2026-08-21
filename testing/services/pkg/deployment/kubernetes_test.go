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

func TestNamespacedKubeVIPDaemonSetEgressIsolation(t *testing.T) {
	const namespace = "kube-vip-egress"

	daemonSet := buildKVDsDaemonSet(namespace, "kube-vip:test", "", false)
	env := daemonSet.Spec.Template.Spec.Containers[0].Env

	want := map[string]string{
		"svc_namespace":  namespace,
		"egress_podcidr": "10.244.0.0/16,fd00:10:244::/56",
	}
	for _, variable := range env {
		value, ok := want[variable.Name]
		if !ok {
			continue
		}
		if variable.Value != value {
			t.Errorf("%s = %q, want %q", variable.Name, variable.Value, value)
		}
		delete(want, variable.Name)
	}
	for name := range want {
		t.Errorf("%s environment variable not found", name)
	}
}
