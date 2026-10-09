//go:build e2e

package e2e_test

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/kube-vip/kube-vip/testing/e2e/matrix"
)

func TestMatrixLeaderCapability(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		combo matrix.Combo
		want  bool
		lease string
	}{
		{name: "ARP control plane", combo: matrix.Combo{Mode: matrix.ModeARP, Function: matrix.FunctionCP, Election: matrix.ElectionGlobal}, want: true, lease: "plndr-cp-lock"},
		{name: "RT control plane has no election", combo: matrix.Combo{Mode: matrix.ModeRT, Function: matrix.FunctionCP, Election: matrix.ElectionNone}},
		{name: "RT global service", combo: matrix.Combo{Mode: matrix.ModeRT, Function: matrix.FunctionSvc, Election: matrix.ElectionGlobal}, want: true, lease: "plndr-svcs-lock"},
		{name: "BGP global service", combo: matrix.Combo{Mode: matrix.ModeBGP, Function: matrix.FunctionSvc, Election: matrix.ElectionGlobal}, want: true, lease: "plndr-svcs-lock"},
		{name: "BGP per-service", combo: matrix.Combo{Mode: matrix.ModeBGP, Function: matrix.FunctionSvc, Election: matrix.ElectionPerService}, want: true, lease: "kubevip-test"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := shouldAssertMatrixLeader(test.combo, test.combo.Function != matrix.FunctionCP); got != test.want {
				t.Fatalf("shouldAssertMatrixLeader() = %t, want %t", got, test.want)
			}
			if test.want && matrixLeaderLease(test.combo, "test") != test.lease {
				t.Fatalf("matrixLeaderLease() = %q, want %q", matrixLeaderLease(test.combo, "test"), test.lease)
			}
		})
	}
}

func TestMatrixBackendsReady(t *testing.T) {
	t.Parallel()
	ready := true
	tests := []struct {
		name     string
		provider matrix.Provider
		client   *fake.Clientset
	}{
		{
			name: "endpoints", provider: matrix.ProviderEndpoints,
			client: fake.NewSimpleClientset(&corev1.Endpoints{ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: "default"}, Subsets: []corev1.EndpointSubset{{Addresses: []corev1.EndpointAddress{{IP: "10.0.0.1"}}}}}),
		},
		{
			name: "slices", provider: matrix.ProviderSlices,
			client: fake.NewSimpleClientset(&discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Name: "service-1", Namespace: "default", Labels: map[string]string{discoveryv1.LabelServiceName: "service"}}, Endpoints: []discoveryv1.Endpoint{{Addresses: []string{"10.0.0.1"}, Conditions: discoveryv1.EndpointConditions{Ready: &ready}}}}),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := matrixBackendsReady(context.Background(), test.client, "default", "service", test.provider); err != nil {
				t.Fatalf("matrixBackendsReady() error = %v", err)
			}
		})
	}

	if err := matrixBackendsReady(context.Background(), fake.NewSimpleClientset(), "default", "missing", matrix.ProviderSlices); err == nil {
		t.Fatal("matrixBackendsReady() succeeded without EndpointSlices")
	}
}
