package services

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestServiceCIDRsFromItems(t *testing.T) {
	serviceCIDR := serviceCIDRsFromItems([]networkingv1.ServiceCIDR{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "kubernetes"},
			Spec: networkingv1.ServiceCIDRSpec{
				CIDRs: []string{"10.96.0.0/16", "fd00:10:96::/112"},
			},
		},
	})

	if serviceCIDR != "10.96.0.0/16,fd00:10:96::/112" {
		t.Fatalf("serviceCIDR = %q", serviceCIDR)
	}
}

func TestPodCIDRsFromNodes(t *testing.T) {
	tests := []struct {
		name  string
		nodes []corev1.Node
		want  string
	}{
		{
			name: "dual stack across nodes",
			nodes: []corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "node-a"},
					Spec: corev1.NodeSpec{
						PodCIDRs: []string{"10.244.0.0/24", "fd00:10:244::/64"},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "node-b"},
					Spec: corev1.NodeSpec{
						PodCIDRs: []string{"10.244.1.0/24", "fd00:10:244:1::/64"},
					},
				},
			},
			want: "10.244.0.0/24,fd00:10:244::/64,10.244.1.0/24,fd00:10:244:1::/64",
		},
		{
			name: "legacy singular CIDR",
			nodes: []corev1.Node{
				{Spec: corev1.NodeSpec{PodCIDR: "10.244.0.0/24"}},
			},
			want: "10.244.0.0/24",
		},
		{
			name: "duplicate CIDRs",
			nodes: []corev1.Node{
				{Spec: corev1.NodeSpec{PodCIDRs: []string{"10.244.0.0/16"}}},
				{Spec: corev1.NodeSpec{PodCIDRs: []string{"10.244.0.0/16"}}},
			},
			want: "10.244.0.0/16",
		},
		{
			name:  "missing CNI CIDRs",
			nodes: []corev1.Node{{}},
			want:  "",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := podCIDRsFromNodes(test.nodes); got != test.want {
				t.Fatalf("podCIDRsFromNodes() = %q, want %q", got, test.want)
			}
		})
	}
}
