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

func TestKubeVipPodsReady(t *testing.T) {
	t.Parallel()
	ready := corev1.PodCondition{Type: corev1.PodReady, Status: corev1.ConditionTrue}
	client := fake.NewSimpleClientset(
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "kube-vip-a", Namespace: "kube-system", Labels: map[string]string{"app": "kube-vip"}}, Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{ready}}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: "kube-system"}, Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{ready}}},
	)

	if err := kubeVipPodsReady(context.Background(), client, 1); err != nil {
		t.Fatalf("kubeVipPodsReady() error = %v", err)
	}
	if err := kubeVipPodsReady(context.Background(), client, 2); err == nil || !strings.Contains(err.Error(), "1/2") {
		t.Fatalf("kubeVipPodsReady() error = %v, want readiness diagnostics", err)
	}
}

func TestRenderKubeVipControlPlaneManifest(t *testing.T) {
	tmpl, err := template.ParseFiles("kube-vip.yaml.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "control-plane.yaml")
	values := KubevipManifestValues{
		ConfigPath:                 "/etc/kubernetes/admin.conf",
		PrometheusHTTPServer:       ":2112",
		SvcElectionEnable:          "false",
		PerServiceElectionOnDemand: "true",
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
		`mountPath: /etc/kubernetes/admin.conf`,
		`path: "/etc/kubernetes/admin.conf"`,
		"hostnames:\n      - kubernetes\n      ip: 127.0.0.1",
		`hostNetwork: true`,
		"- name: svc_election\n      value: \"false\"",
		"- name: per_service_election_on_demand\n      value: \"true\"",
	} {
		if !strings.Contains(string(manifest), want) {
			t.Errorf("rendered control-plane manifest does not contain %q", want)
		}
	}
	for _, obsolete := range []string{"kubelet.conf", "kubelet-pki", "/var/lib/kubelet/pki", "kubernetes.default.svc"} {
		if strings.Contains(string(manifest), obsolete) {
			t.Errorf("rendered control-plane manifest contains obsolete worker credential %q", obsolete)
		}
	}
}

func TestRenderKubeVipWorkerManifest(t *testing.T) {
	tmpl, err := template.ParseFiles("kube-vip.yaml.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "worker.yaml")
	values := KubevipManifestValues{
		ConfigPath:                 "/etc/kubernetes/kubelet.conf",
		KubeletPKIPath:             "/var/lib/kubelet/pki",
		PrometheusHTTPServer:       ":2112",
		SvcElectionEnable:          "false",
		PerServiceElectionOnDemand: "true",
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
		"- name: svc_election\n      value: \"false\"",
		"- name: per_service_election_on_demand\n      value: \"true\"",
	} {
		if !strings.Contains(string(manifest), want) {
			t.Errorf("rendered worker manifest does not contain %q", want)
		}
	}
}

func TestWaitForKubeVipReady(t *testing.T) {
	readyPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "kube-vip-control-plane", Namespace: "kube-system", Labels: map[string]string{"app": "kube-vip"}},
		Spec:       corev1.PodSpec{NodeName: "control-plane"},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "kube-vip", Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}},
		},
	}
	client := fake.NewClientset(readyPod)
	if err := WaitForKubeVipReady(context.Background(), client, []string{"control-plane"}); err != nil {
		t.Fatalf("WaitForKubeVipReady() error = %v", err)
	}
	if err := WaitForKubeVipReady(context.Background(), client, []string{"control-plane", "control-plane2"}); err == nil || !strings.Contains(err.Error(), "control-plane2") {
		t.Fatalf("WaitForKubeVipReady() error = %v, want missing control-plane2 diagnostic", err)
	}
}
