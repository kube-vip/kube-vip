package endpoints

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kube-vip/kube-vip/pkg/endpoints/providers"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
)

func TestShouldAllowReconcileWithoutEndpoints(t *testing.T) {
	if shouldAllowReconcileWithoutEndpoints(nil) {
		t.Fatal("nil service should not be allowed")
	}

	clusterOptIn := &v1.Service{
		Spec:       v1.ServiceSpec{ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeCluster},
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{kubevip.AllowReconcileWithoutEndpoints: "true"}},
	}
	if !shouldAllowReconcileWithoutEndpoints(clusterOptIn) {
		t.Fatal("cluster service with opt-in annotation should be allowed")
	}

	localOptIn := &v1.Service{
		Spec:       v1.ServiceSpec{ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeLocal},
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{kubevip.AllowReconcileWithoutEndpoints: "true"}},
	}
	if shouldAllowReconcileWithoutEndpoints(localOptIn) {
		t.Fatal("local service should not be allowed")
	}
}

type fakeWorker struct {
	endpoints     []string
	clearCalled   bool
	processCalled bool
}

func (f *fakeWorker) processInstance(_ *servicecontext.Context, _ *v1.Service) error {
	f.processCalled = true
	return nil
}

func (f *fakeWorker) clear(_ *servicecontext.Context, _ *string, _ *v1.Service) {
	f.clearCalled = true
}

func (f *fakeWorker) getEndpoints(_ *v1.Service, _ string) ([]string, error) { return f.endpoints, nil }
func (f *fakeWorker) removeEgress(_ *v1.Service, _ *string)                  {}
func (f *fakeWorker) delete(_ context.Context, _ *v1.Service, _ string) error {
	return nil
}
func (f *fakeWorker) setInstanceEndpointsStatus(_ context.Context, _ *v1.Service, _ []string) error {
	return nil
}

func TestAddOrModify_ZeroEndpointsBehavior(t *testing.T) {
	t.Parallel()

	run := func(t *testing.T, service *v1.Service, presetSignalled bool, expectReady bool, expectClear bool, expectProcess bool) {
		t.Helper()

		worker := &fakeWorker{endpoints: []string{}}
		p := &Processor{
			config:   &kubevip.Config{},
			provider: providers.NewEndpointslices(),
			worker:   worker,
		}

		svcCtx := servicecontext.New(context.Background())
		if presetSignalled {
			svcCtx.SignalReadiness()
		}

		restart, err := p.AddOrModify(
			svcCtx,
			watch.Event{Type: watch.Modified, Object: &discoveryv1.EndpointSlice{}},
			new(string),
			service,
			"node-1",
			func(*servicecontext.Context, *v1.Service, *sync.WaitGroup, bool) error { return nil },
			&sync.WaitGroup{},
			nil,
			nil,
		)
		if err != nil {
			t.Fatalf("AddOrModify returned error: %v", err)
		}
		if restart {
			t.Fatal("AddOrModify unexpectedly requested restart")
		}

		if ready := svcCtx.Signalled.Load(); ready != expectReady {
			t.Fatalf("readiness mismatch: expected %v, got %v", expectReady, ready)
		}
		if worker.clearCalled != expectClear {
			t.Fatalf("clearCalled mismatch: expected %v, got %v", expectClear, worker.clearCalled)
		}
		if worker.processCalled != expectProcess {
			t.Fatalf("processCalled mismatch: expected %v, got %v", expectProcess, worker.processCalled)
		}
	}

	t.Run("cluster opt-in keeps readiness and skips clear", func(t *testing.T) {
		service := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{kubevip.AllowReconcileWithoutEndpoints: "true"}},
			Spec:       v1.ServiceSpec{ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeCluster},
		}
		run(t, service, false, true, false, true)
	})

	t.Run("cluster without opt-in resets and clears when pre-signalled", func(t *testing.T) {
		service := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{}},
			Spec:       v1.ServiceSpec{ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeCluster},
		}
		run(t, service, true, false, true, false)
	})

	t.Run("local opt-in still resets and clears when pre-signalled", func(t *testing.T) {
		service := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{kubevip.AllowReconcileWithoutEndpoints: "true"}},
			Spec:       v1.ServiceSpec{ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeLocal},
		}
		run(t, service, true, false, true, false)
	})
}

// TestAddOrModify_ServicesElectionStartsLeaderElectionOnce guards against a
// regression where startServiceHandlingIfNeeded spawns a new, permanently
// running startLeaderElection goroutine on every AddOrModify call that sees
// non-zero endpoints, instead of exactly once per service lifetime. Before
// the fix, repeated endpoint churn (endpoints flipping non-zero -> zero ->
// non-zero, e.g. during a backend pod's rolling restart) accumulates
// duplicate restart-loop goroutines racing on the same shared Lease; one of
// the observed effects in production was a service Lease that never
// reacquired a holder after a legitimate teardown, even though a healthy
// endpoint had come back.
func TestAddOrModify_ServicesElectionStartsLeaderElectionOnce(t *testing.T) {
	service := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "svc-under-test", Namespace: "default"},
	}

	leaseNamespace, leaseName := lease.ServiceName(service)
	config := &kubevip.Config{EnableServicesElection: true}
	id := lease.NewID(config.LeaderElectionType, leaseNamespace, leaseName)

	leaseMgr := lease.NewManager()
	l := leaseMgr.Add(context.Background(), id)

	worker := &fakeWorker{endpoints: []string{"10.0.0.5"}}
	p := &Processor{
		config:   config,
		provider: providers.NewEndpointslices(),
		worker:   worker,
		leaseMgr: leaseMgr,
	}

	svcCtx := servicecontext.New(l.Ctx)

	var calls int32
	started := make(chan struct{}, 8)
	block := make(chan struct{})
	// Stands in for services.StartServicesLeaderElection: records that a
	// leader-election attempt started, then blocks - simulating a goroutine
	// that has become (or is trying to become) the active leader and won't
	// return until it loses leadership or is torn down.
	serviceFunc := func(*servicecontext.Context, *v1.Service, *sync.WaitGroup, bool) error {
		atomic.AddInt32(&calls, 1)
		started <- struct{}{}
		<-block
		return nil
	}

	wg := &sync.WaitGroup{}
	lastKnownGoodEndpoint := new(string)

	// Simulate the endpoint being (re-)observed as non-zero three times in a
	// row, as happens across repeated EndpointSlice add/modify/resync events
	// for the same still-healthy service.
	for range 3 {
		if _, err := p.AddOrModify(
			svcCtx,
			watch.Event{Type: watch.Modified, Object: &discoveryv1.EndpointSlice{}},
			lastKnownGoodEndpoint,
			service,
			"node-1",
			serviceFunc,
			wg,
			nil,
			nil,
		); err != nil {
			t.Fatalf("AddOrModify returned error: %v", err)
		}
	}

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the first leader-election attempt to start")
	}

	select {
	case <-started:
		t.Fatalf("a second leader-election restart-loop goroutine started for the same service (got %d serviceFunc calls) - startServiceHandlingIfNeeded must only spawn it once per service lifetime", atomic.LoadInt32(&calls))
	case <-time.After(200 * time.Millisecond):
		// No second spawn observed - expected.
	}

	close(block)
	svcCtx.Cancel()
	wg.Wait()

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected exactly 1 leader-election attempt across 3 AddOrModify calls, got %d", got)
	}
}
