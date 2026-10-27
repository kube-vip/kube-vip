package services

import (
	"context"
	"testing"
	"time"

	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	coordinationv1 "k8s.io/api/coordination/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
)

func TestServiceSnapshotNewerUsesGenerationOnlyForSameUID(t *testing.T) {
	watched := &v1.Service{ObjectMeta: metav1.ObjectMeta{UID: "uid-a", Generation: 12, ResourceVersion: "opaque-new"}}
	if !serviceSnapshotNewer(watched, &v1.Service{ObjectMeta: metav1.ObjectMeta{UID: "uid-a", Generation: 11, ResourceVersion: "opaque-old"}}) {
		t.Fatal("expected delayed generation to be rejected")
	}
	if serviceSnapshotNewer(watched, &v1.Service{ObjectMeta: metav1.ObjectMeta{UID: "uid-a", Generation: 12, ResourceVersion: "opaque-other"}}) {
		t.Fatal("equal generation should remain admissible")
	}
	if serviceSnapshotNewer(watched, &v1.Service{ObjectMeta: metav1.ObjectMeta{UID: "uid-b", Generation: 99, ResourceVersion: "opaque-recreated"}}) {
		t.Fatal("a recreated Service UID must remain admissible")
	}
}

func TestWasForcedServiceUsesWatchedGeneration(t *testing.T) {
	uid := types.UID("forced-uid")
	ctx := servicecontext.New(context.Background())
	ctx.SetWatchedService(&v1.Service{ObjectMeta: metav1.ObjectMeta{
		UID:         uid,
		Annotations: map[string]string{kubevip.ForcePerServiceElection: "true"},
	}})
	p := &Processor{serviceLock: NewServiceLock()}
	p.svcMap.Store(uid, ctx)
	if !p.wasForcedService(uid) {
		t.Fatal("expected watched forced service to be recognized")
	}
	ctx.SetWatchedService(&v1.Service{ObjectMeta: metav1.ObjectMeta{UID: uid}})
	if p.wasForcedService(uid) {
		t.Fatal("ordinary replacement must not be treated as forced ownership")
	}
}

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

func TestCurrentLeaseHolderIgnoresExpiredLease(t *testing.T) {
	now := time.Now()
	lease := func(renewed time.Time) *coordinationv1.Lease {
		return &coordinationv1.Lease{Spec: coordinationv1.LeaseSpec{
			HolderIdentity:       ptr.To("node-a"),
			LeaseDurationSeconds: ptr.To(int32(15)),
			RenewTime:            &metav1.MicroTime{Time: renewed},
		}}
	}
	if got := currentLeaseHolder(lease(now), now); got != "node-a" {
		t.Fatalf("current lease holder = %q, want node-a", got)
	}
	if got := currentLeaseHolder(lease(now.Add(-time.Minute)), now); got != "" {
		t.Fatalf("expired lease holder = %q, want empty", got)
	}
	if got := currentLeaseHolder(&coordinationv1.Lease{}, now); got != "" {
		t.Fatalf("lease without holder = %q, want empty", got)
	}
}

func TestServiceSnapshotChangedDetectsForcedElectionToggle(t *testing.T) {
	ordinary := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "example", Namespace: "default", UID: "service-uid"},
		Spec:       v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer, LoadBalancerIP: "192.0.2.10"},
	}
	forced := ordinary.DeepCopy()
	forced.Annotations = map[string]string{kubevip.ForcePerServiceElection: "true"}

	if !serviceSnapshotChanged(ordinary, forced) {
		t.Fatal("enabling forced per-service election was not treated as a lifecycle change")
	}
	if !serviceSnapshotChanged(forced, ordinary) {
		t.Fatal("disabling forced per-service election was not treated as a lifecycle change")
	}
	if serviceSnapshotChanged(forced, forced.DeepCopy()) {
		t.Fatal("unchanged forced Service was treated as a lifecycle change")
	}
}
