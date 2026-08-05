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
	"github.com/kube-vip/kube-vip/pkg/metrics"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	"github.com/prometheus/client_golang/prometheus/testutil"
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

// TestAddOrModify_ServicesElectionStartsOnce asserts that repeated endpoint events
// for the same service start the leader-election restart loop exactly once.
//
// AddOrModify runs on every EndpointSlice add/modify/resync event, and the loop it
// starts only returns once the service context is cancelled. Starting it per event
// therefore accumulates duplicate goroutines that all contend on the same lease.
//
// See https://github.com/kube-vip/kube-vip/issues/1665.
func TestAddOrModify_ServicesElectionStartsOnce(t *testing.T) {
	config := &kubevip.Config{
		EnableServicesElection: true,
		LeaderElectionType:     "kubernetes",
	}

	service := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "test-svc", Namespace: "default", UID: "test-uid"},
		Spec:       v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	leaseMgr := lease.NewManager()
	leaseNamespace, serviceLease := lease.ServiceName(service)
	svcLease := leaseMgr.Add(ctx, lease.NewID(config.LeaderElectionType, leaseNamespace, serviceLease))

	svcCtx := servicecontext.New(svcLease.Ctx)

	// The started loops only return once the service context is cancelled, so it has
	// to be cancelled before waiting on them.
	wg := &sync.WaitGroup{}
	defer wg.Wait()
	defer svcCtx.Cancel()

	p := &Processor{
		config:   config,
		provider: providers.NewEndpointslices(),
		worker:   &fakeWorker{endpoints: []string{"10.0.0.1"}},
		leaseMgr: leaseMgr,
	}

	// starts counts the restart loops. The real StartServicesLeaderElection blocks
	// until the service context is cancelled, so each loop parks in a single call.
	var starts atomic.Int64
	serviceFunc := func(svcCtx *servicecontext.Context, _ *v1.Service, _ *sync.WaitGroup, _ bool) error {
		starts.Add(1)
		<-svcCtx.Ctx.Done()
		return nil
	}

	// Three endpoint events, as a flapping backend pod would produce.
	for range 3 {
		restart, err := p.AddOrModify(svcCtx, watch.Event{Type: watch.Modified, Object: &discoveryv1.EndpointSlice{}},
			new(string), service, "node-1", serviceFunc, wg, nil, nil)
		if err != nil {
			t.Fatalf("AddOrModify returned error: %v", err)
		}
		if restart {
			t.Fatal("AddOrModify unexpectedly requested restart")
		}
	}

	// Give every loop that is going to start a chance to reach serviceFunc.
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		if starts.Load() > 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if got := starts.Load(); got != 1 {
		t.Errorf("leader election started %d times, want 1", got)
	}

	// The gauge the e2e fault tests assert on has to agree with the call count.
	if got := testutil.ToFloat64(metrics.ServiceElectionLoops.WithLabelValues(service.Namespace, service.Name)); got != 1 {
		t.Errorf("kube_vip_service_election_loops is %v, want 1", got)
	}
}
