//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"text/template"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestRenderKubeVipWorkerManifest(t *testing.T) {
	tmpl, err := template.ParseFiles("kube-vip.yaml.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "worker.yaml")
	values := KubevipManifestValues{
		ConfigPath:           "/etc/kubernetes/kubelet.conf",
		KubeletPKIPath:       "/var/lib/kubelet/pki",
		PrometheusHTTPServer: ":2112",
	}
	if err := renderKubeVipManifest(tmpl, path, values); err != nil {
		t.Fatal(err)
	}
	manifest, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`- --prometheusHTTPServer`,
		`- ":2112"`,
		`path: "/etc/kubernetes/kubelet.conf"`,
		`mountPath: /var/lib/kubelet/pki`,
		`path: /var/lib/kubelet/pki`,
		`hostNetwork: true`,
	} {
		if !strings.Contains(string(manifest), want) {
			t.Errorf("rendered worker manifest does not contain %q", want)
		}
	}
}

func TestWaitForKubeVipReady(t *testing.T) {
	readyPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "kube-vip-worker", Namespace: "kube-system", Labels: map[string]string{"app": "kube-vip"}},
		Spec:       corev1.PodSpec{NodeName: "worker"},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "kube-vip", Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}},
		},
	}
	client := fake.NewClientset(readyPod)
	if err := WaitForKubeVipReady(context.Background(), client, []string{"worker"}); err != nil {
		t.Fatalf("WaitForKubeVipReady() error = %v", err)
	}
	if err := WaitForKubeVipReady(context.Background(), client, []string{"worker", "worker2"}); err == nil || !strings.Contains(err.Error(), "worker2") {
		t.Fatalf("WaitForKubeVipReady() error = %v, want missing worker2 diagnostic", err)
	}
}
