package services

import (
	"context"
	"testing"

	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
)

func TestAddOrModifyStopsTrackedServiceWhenTypeChanges(t *testing.T) {
	for _, ignored := range []bool{false, true} {
		name := "normal"
		if ignored {
			name = "ignored"
		}
		t.Run(name, func(t *testing.T) {
			annotations := map[string]string{}
			if ignored {
				annotations[kubevip.LoadbalancerIgnore] = "true"
			}

			uid := types.UID("service-uid")
			tracked := &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "example",
					Namespace:   "default",
					UID:         uid,
					Annotations: map[string]string{},
				},
				Spec: v1.ServiceSpec{
					Type:           v1.ServiceTypeLoadBalancer,
					LoadBalancerIP: "192.0.2.10",
				},
			}
			modified := tracked.DeepCopy()
			modified.Spec.Type = v1.ServiceTypeClusterIP
			modified.Annotations = annotations

			p := &Processor{
				config:           &kubevip.Config{},
				leaseMgr:         lease.NewManager(),
				ServiceInstances: []*instance.Instance{{ServiceSnapshot: tracked}},
			}
			svcCtx := servicecontext.New(context.Background())
			p.svcMap.Store(uid, svcCtx)

			if err := p.AddOrModify(context.Background(), watch.Event{Type: watch.Modified, Object: modified}, nil, false, nil, nil); err != nil {
				t.Fatalf("AddOrModify returned error: %v", err)
			}

			if svcCtx.Ctx.Err() == nil {
				t.Fatal("tracked service context was not cancelled")
			}
			if _, ok := p.svcMap.Load(uid); ok {
				t.Fatal("tracked service context was not removed from svcMap")
			}
			if len(p.ServiceInstances) != 0 {
				t.Fatalf("tracked service instance count = %d, want 0", len(p.ServiceInstances))
			}
		})
	}
}

// TestDropCancelledServiceContext is a regression test for the lease/svcMap desync that
// permanently stops a LoadBalancer VIP from being advertised.
//
// AddOrModify only calls leaseMgr.Add inside its `if svcCtx == nil` branch, while the
// in-memory lease is removed independently by the cleanup goroutine in
// StartServicesLeaderElection (leaseMgr.Delete once svcCtx.Ctx is done). Paths that cancel
// the service context without also removing it from svcMap - the deferred close(stopChan)
// in watchEndpoint, and the utils.PanicError branch in AddOrModify - therefore leave a
// cancelled context behind. Every later watch event then reuses it, skips leaseMgr.Add, and
// StartServicesLeaderElection fails with "no existing lease found for service ..." forever.
//
// Dropping a cancelled context restores the invariant that a service context in svcMap
// always has a matching lease in the lease manager.
func TestDropCancelledServiceContext(t *testing.T) {
	newProcessor := func() *Processor {
		return &Processor{
			config:   &kubevip.Config{},
			leaseMgr: lease.NewManager(),
		}
	}

	uid := types.UID("service-uid")

	t.Run("cancelled context is dropped and removed from svcMap", func(t *testing.T) {
		p := newProcessor()

		ctx, cancel := context.WithCancel(context.Background())
		svcCtx := servicecontext.New(ctx)
		p.svcMap.Store(uid, svcCtx)
		cancel()

		if got := p.dropCancelledServiceContext(uid, svcCtx); got != nil {
			t.Fatalf("expected a cancelled service context to be dropped, got %v", got)
		}
		if _, ok := p.svcMap.Load(uid); ok {
			t.Fatal("expected the cancelled service context to be removed from svcMap")
		}
	})

	t.Run("live context is kept", func(t *testing.T) {
		p := newProcessor()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		svcCtx := servicecontext.New(ctx)
		p.svcMap.Store(uid, svcCtx)

		if got := p.dropCancelledServiceContext(uid, svcCtx); got != svcCtx {
			t.Fatalf("expected a live service context to be kept, got %v", got)
		}
		if _, ok := p.svcMap.Load(uid); !ok {
			t.Fatal("expected a live service context to stay in svcMap")
		}
	})

	t.Run("nil context is a no-op", func(t *testing.T) {
		p := newProcessor()
		if got := p.dropCancelledServiceContext(uid, nil); got != nil {
			t.Fatalf("expected nil to be returned for a nil service context, got %v", got)
		}
	})
}

// TestDropCancelledServiceContextAllowsLeaseRecreation shows the consequence of the fix: once the
// cancelled context has been dropped, the caller takes the `svcCtx == nil` branch and a lease is
// created again, so StartServicesLeaderElection no longer fails with "no existing lease found".
func TestDropCancelledServiceContextAllowsLeaseRecreation(t *testing.T) {
	p := &Processor{
		config:   &kubevip.Config{},
		leaseMgr: lease.NewManager(),
	}

	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "example",
			Namespace: "default",
			UID:       types.UID("service-uid"),
		},
	}

	leaseNamespace, serviceLease := lease.ServiceName(svc)
	id := lease.NewID(p.config.LeaderElectionType, leaseNamespace, serviceLease)

	// A previous election created a lease and a service context, then both the lease and the
	// service context went away - but only the lease was removed from the manager.
	ctx, cancel := context.WithCancel(context.Background())
	svcCtx := servicecontext.New(ctx)
	p.svcMap.Store(svc.UID, svcCtx)
	cancel()

	if p.leaseMgr.Get(id) != nil {
		t.Fatal("precondition failed: the lease manager should not hold a lease yet")
	}

	if got := p.dropCancelledServiceContext(svc.UID, svcCtx); got != nil {
		t.Fatalf("expected the stale service context to be dropped, got %v", got)
	}

	// This mirrors the `if svcCtx == nil` branch in AddOrModify.
	p.leaseMgr.Add(context.Background(), id)

	if p.leaseMgr.Get(id) == nil {
		t.Fatal("expected a new lease to be created once the cancelled service context was dropped")
	}
}

func TestOnStoppedLeadingDoesNotDeleteReplacementContext(t *testing.T) {
	p := &Processor{
		config:   &kubevip.Config{},
		leaseMgr: lease.NewManager(),
	}

	service := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "example",
			Namespace: "default",
			UID:       types.UID("service-uid"),
		},
	}

	oldCtx := servicecontext.New(context.Background())
	replacementCtx := servicecontext.New(context.Background())
	p.svcMap.Store(service.UID, replacementCtx)
	replacementInstance := &instance.Instance{ServiceSnapshot: service.DeepCopy()}
	p.ServiceInstances = []*instance.Instance{replacementInstance}

	leaseNamespace, serviceLease := lease.ServiceName(service)
	svcLease := p.leaseMgr.Add(context.Background(), lease.NewID(p.config.LeaderElectionType, leaseNamespace, serviceLease))

	if err := p.onStoppedLeading(oldCtx, svcLease, service); err != nil {
		t.Fatalf("onStoppedLeading returned an error: %v", err)
	}
	if got, err := p.getServiceContext(service.UID); err != nil || got != replacementCtx {
		t.Fatalf("replacement context was changed: got %v, err %v", got, err)
	}
	if len(p.ServiceInstances) != 1 || p.ServiceInstances[0] != replacementInstance {
		t.Fatal("replacement service instance was removed by superseded cleanup")
	}
}
