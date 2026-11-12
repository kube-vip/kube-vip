package services

import (
	"context"
	"testing"

	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestChangedSinceWatched is a regression test for a node that has lost (or never won) the
// service election. Its instance has been removed from the manager, but its watchers still
// run with the Service they were started with. A traffic policy change must still be detected
// against that copy, otherwise the node keeps treating a Local service as Cluster and keeps
// campaigning for the lease without a local endpoint.
func TestChangedSinceWatched(t *testing.T) {
	policy := v1.IPFamilyPolicySingleStack
	cluster := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "example", Namespace: "default", UID: "service-uid"},
		Spec: v1.ServiceSpec{
			Type:                  v1.ServiceTypeLoadBalancer,
			LoadBalancerIP:        "192.0.2.10",
			ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeCluster,
			IPFamilyPolicy:        &policy,
		},
	}
	local := cluster.DeepCopy()
	local.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyTypeLocal

	watchedCtx := func(svc *v1.Service) *servicecontext.Context {
		svcCtx := servicecontext.New(context.Background())
		svcCtx.SetWatchedService(svc)
		return svcCtx
	}

	tests := []struct {
		name     string
		instance *instance.Instance
		svcCtx   *servicecontext.Context
		svc      *v1.Service
		want     bool
	}{
		{
			name:     "instance present, policy changed",
			instance: &instance.Instance{ServiceSnapshot: cluster},
			svcCtx:   watchedCtx(cluster),
			svc:      local,
			want:     true,
		},
		{
			name:     "instance present, unchanged",
			instance: &instance.Instance{ServiceSnapshot: local},
			svcCtx:   watchedCtx(cluster),
			svc:      local,
			want:     false,
		},
		{
			name:   "instance removed, watcher started with old policy",
			svcCtx: watchedCtx(cluster),
			svc:    local,
			want:   true,
		},
		{
			name:   "instance removed, watcher already on current policy",
			svcCtx: watchedCtx(local),
			svc:    local,
			want:   false,
		},
		{
			name:   "no watcher started yet",
			svcCtx: servicecontext.New(context.Background()),
			svc:    local,
			want:   false,
		},
		{
			name: "no instance and no context",
			svc:  local,
			want: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := changedSinceWatched(test.instance, test.svcCtx, test.svc); got != test.want {
				t.Fatalf("changedSinceWatched() = %v, want %v", got, test.want)
			}
		})
	}
}

// TestChangedSinceWatchedForcePerServiceElection covers a Service moving between
// the regular and the on-demand election watcher. The watcher the Service now
// matches finds the instance the other one started, so the change must be
// detected or the election never starts (or never stops) until kube-vip restarts.
func TestChangedSinceWatchedForcePerServiceElection(t *testing.T) {
	regular := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "example", Namespace: "default", UID: "service-uid"},
		Spec: v1.ServiceSpec{
			Type:                  v1.ServiceTypeLoadBalancer,
			LoadBalancerIP:        "192.0.2.10",
			ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeLocal,
		},
	}
	forced := regular.DeepCopy()
	forced.Annotations = map[string]string{kubevip.ForcePerServiceElection: "true"}
	notForced := regular.DeepCopy()
	notForced.Annotations = map[string]string{kubevip.ForcePerServiceElection: "false"}

	tests := []struct {
		name         string
		watched, svc *v1.Service
		want         bool
	}{
		{name: "annotation added", watched: regular, svc: forced, want: true},
		{name: "annotation removed", watched: forced, svc: regular, want: true},
		{name: "annotation set to false", watched: forced, svc: notForced, want: true},
		{name: "false is the same as absent", watched: regular, svc: notForced, want: false},
		{name: "still forced", watched: forced, svc: forced.DeepCopy(), want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := changedSinceWatched(&instance.Instance{ServiceSnapshot: test.watched}, nil, test.svc); got != test.want {
				t.Fatalf("changedSinceWatched() with instance = %v, want %v", got, test.want)
			}
			svcCtx := servicecontext.New(context.Background())
			svcCtx.SetWatchedService(test.watched)
			if got := changedSinceWatched(nil, svcCtx, test.svc); got != test.want {
				t.Fatalf("changedSinceWatched() with watched Service = %v, want %v", got, test.want)
			}
		})
	}
}
