package endpoints

import (
	"context"
	"sync"
	"testing"

	"github.com/kube-vip/kube-vip/pkg/endpoints/providers"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	v1 "k8s.io/api/core/v1"
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
	endpoints      []string
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
			provider: providers.NewEndpoints(),
			worker:   worker,
		}

		svcCtx := servicecontext.New(context.Background())
		if presetSignalled {
			svcCtx.SignalReadiness()
		}

		restart, err := p.AddOrModify(
			svcCtx,
			watch.Event{Type: watch.Modified, Object: &v1.Endpoints{}},
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