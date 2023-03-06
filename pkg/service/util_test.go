package service

import (
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func Test_FetchServiceAddress(t *testing.T) {

	tests := []struct {
		name         string
		svc          v1.Service
		expectedLBIP string
	}{
		{
			name: "service with both annotation and spec.loadbalancerIP, get ip from annotations",
			svc: v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "test",
					Name:      "name",
					Annotations: map[string]string{
						"kube-vip.io/loadbalancerIPs": "192.168.1.1",
					},
				},
				Spec: v1.ServiceSpec{
					LoadBalancerIP: "192.168.1.2",
				},
			},
			expectedLBIP: "192.168.1.1",
		},
		{
			name: "service with only annotations",
			svc: v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "test",
					Name:      "name",
					Annotations: map[string]string{
						"kube-vip.io/loadbalancerIPs": "192.168.1.1",
					},
				},
				Spec: v1.ServiceSpec{},
			},
			expectedLBIP: "192.168.1.1",
		},
		{
			name: "service with only spec.loadbalancerIp",
			svc: v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "test",
					Name:      "name",
				},
				Spec: v1.ServiceSpec{
					LoadBalancerIP: "192.168.1.2",
				},
			},
			expectedLBIP: "192.168.1.2",
		},
		{
			name: "service without spec.loadbalancerIp and annotations",
			svc: v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "test",
					Name:      "name",
				},
				Spec: v1.ServiceSpec{},
			},
			expectedLBIP: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ip := FetchServiceAddress(&tt.svc)
			if ip != tt.expectedLBIP {
				t.Errorf("got ip '%s', expected: '%s'", ip, tt.expectedLBIP)
				return
			}
		})
	}
}
