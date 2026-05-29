package endpoints

import (
	"testing"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestShouldAllowReconcileWithoutEndpoints(t *testing.T) {
	tests := []struct {
		name     string
		service  *v1.Service
		expected bool
	}{
		{
			name:     "nil service",
			expected: false,
		},
		{
			name: "cluster policy with opt-in annotation",
			service: &v1.Service{
				Spec:       v1.ServiceSpec{ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeCluster},
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{kubevip.AllowReconcileWithoutEndpoints: "true"}},
			},
			expected: true,
		},
		{
			name: "cluster policy with case-insensitive opt-in",
			service: &v1.Service{
				Spec:       v1.ServiceSpec{ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeCluster},
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{kubevip.AllowReconcileWithoutEndpoints: "TRUE"}},
			},
			expected: true,
		},
		{
			name: "cluster policy without opt-in",
			service: &v1.Service{
				Spec:       v1.ServiceSpec{ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeCluster},
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{}},
			},
			expected: false,
		},
		{
			name: "local policy with opt-in",
			service: &v1.Service{
				Spec:       v1.ServiceSpec{ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeLocal},
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{kubevip.AllowReconcileWithoutEndpoints: "true"}},
			},
			expected: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			actual := shouldAllowReconcileWithoutEndpoints(test.service)
			if actual != test.expected {
				t.Fatalf("expected %v, got %v", test.expected, actual)
			}
		})
	}
}
