//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"

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
