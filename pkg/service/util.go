package service

import (
	v1 "k8s.io/api/core/v1"
)

// it will first fetch from annotations kube-vip.io/loadbalancerIPs, then from spec.loadbalancerIP
func FetchServiceAddress(s *v1.Service) string {
	if s.Annotations != nil {
		if v, ok := s.Annotations[loadbalancerIPAnnotation]; ok {
			return v
		}
	}
	return s.Spec.LoadBalancerIP
}
