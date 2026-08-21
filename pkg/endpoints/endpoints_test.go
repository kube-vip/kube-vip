package endpoints

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kube-vip/kube-vip/pkg/endpoints/providers"
	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/metrics"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	"github.com/prometheus/client_golang/prometheus/testutil"
	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
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

type annotationUpdate struct {
	endpoint     string
	endpointIPv6 string
}

type recordingProvider struct {
	providers.Provider
	updates   []annotationUpdate
	updateErr error
}

func (p *recordingProvider) UpdateServiceAnnotation(_ context.Context, endpoint, endpointIPv6 string,
	_ *v1.Service, _ *kubernetes.Clientset) error {
	p.updates = append(p.updates, annotationUpdate{endpoint: endpoint, endpointIPv6: endpointIPv6})
	return p.updateErr
}

func TestUpdateAnnotations(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name               string
		enableEndpoints    bool
		annotations        map[string]string
		selectedEndpoint   string
		providerError      bool
		wantUpdate         *annotationUpdate
		wantEgressCallback bool
	}{
		{
			name:               "Endpoints writes initial IPv4 endpoint",
			enableEndpoints:    true,
			annotations:        map[string]string{kubevip.Egress: "true"},
			selectedEndpoint:   "10.0.0.2",
			wantUpdate:         &annotationUpdate{endpoint: "10.0.0.2"},
			wantEgressCallback: true,
		},
		{
			name:            "EndpointSlices updates IPv4 and preserves IPv6",
			enableEndpoints: false,
			annotations: map[string]string{
				kubevip.Egress:             "true",
				kubevip.ActiveEndpoint:     "10.0.0.1",
				kubevip.ActiveEndpointIPv6: "fd00::1",
			},
			selectedEndpoint:   "10.0.0.2",
			wantUpdate:         &annotationUpdate{endpoint: "10.0.0.2", endpointIPv6: "fd00::1"},
			wantEgressCallback: true,
		},
		{
			name:            "EndpointSlices updates IPv6 and preserves IPv4",
			enableEndpoints: false,
			annotations: map[string]string{
				kubevip.Egress:             "true",
				kubevip.EgressIPv6:         "true",
				kubevip.ActiveEndpoint:     "10.0.0.1",
				kubevip.ActiveEndpointIPv6: "fd00::1",
			},
			selectedEndpoint:   "fd00::2",
			wantUpdate:         &annotationUpdate{endpoint: "10.0.0.1", endpointIPv6: "fd00::2"},
			wantEgressCallback: true,
		},
		{
			name:            "EndpointSlices clears IPv4 and preserves IPv6",
			enableEndpoints: false,
			annotations: map[string]string{
				kubevip.Egress:             "true",
				kubevip.ActiveEndpoint:     "10.0.0.1",
				kubevip.ActiveEndpointIPv6: "fd00::1",
			},
			wantUpdate:         &annotationUpdate{endpointIPv6: "fd00::1"},
			wantEgressCallback: true,
		},
		{
			name:            "EndpointSlices clears IPv6 and preserves IPv4",
			enableEndpoints: false,
			annotations: map[string]string{
				kubevip.Egress:             "true",
				kubevip.EgressIPv6:         "true",
				kubevip.ActiveEndpoint:     "10.0.0.1",
				kubevip.ActiveEndpointIPv6: "fd00::1",
			},
			wantUpdate:         &annotationUpdate{endpoint: "10.0.0.1"},
			wantEgressCallback: true,
		},
		{
			name:            "unchanged endpoint does not write or reconfigure egress",
			enableEndpoints: true,
			annotations: map[string]string{
				kubevip.Egress:         "true",
				kubevip.ActiveEndpoint: "10.0.0.1",
			},
			selectedEndpoint: "10.0.0.1",
		},
		{
			name:             "failed annotation update does not reconfigure egress",
			enableEndpoints:  true,
			annotations:      map[string]string{kubevip.Egress: "true"},
			selectedEndpoint: "10.0.0.2",
			providerError:    true,
			wantUpdate:       &annotationUpdate{endpoint: "10.0.0.2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := providers.NewEndpointslices()
			if tt.enableEndpoints {
				provider = providers.NewEndpoints()
			}
			recorder := &recordingProvider{Provider: provider}
			if tt.providerError {
				recorder.updateErr = errors.New("update failed")
			}

			p := &Processor{
				config:   &kubevip.Config{EnableEndpoints: tt.enableEndpoints},
				provider: recorder,
			}
			service := &v1.Service{ObjectMeta: metav1.ObjectMeta{
				Name: "test-service", Namespace: "default", Annotations: tt.annotations,
			}}

			callbackCalls := 0
			var callbackService *v1.Service
			p.updateAnnotations(service, &tt.selectedEndpoint, nil, func(_ context.Context, svc *v1.Service) error {
				callbackCalls++
				callbackService = svc
				return nil
			})

			if tt.wantUpdate == nil {
				if len(recorder.updates) != 0 {
					t.Fatalf("got %d annotation updates, want none", len(recorder.updates))
				}
			} else {
				if len(recorder.updates) != 1 {
					t.Fatalf("got %d annotation updates, want one", len(recorder.updates))
				}
				if got := recorder.updates[0]; got != *tt.wantUpdate {
					t.Fatalf("annotation update = %+v, want %+v", got, *tt.wantUpdate)
				}
			}

			wantCallbackCalls := 0
			if tt.wantEgressCallback {
				wantCallbackCalls = 1
			}
			if callbackCalls != wantCallbackCalls {
				t.Fatalf("egress callback called %d times, want %d", callbackCalls, wantCallbackCalls)
			}
			if callbackService != nil && tt.wantUpdate != nil {
				if got := callbackService.Annotations[kubevip.ActiveEndpoint]; got != tt.wantUpdate.endpoint {
					t.Errorf("callback IPv4 endpoint = %q, want %q", got, tt.wantUpdate.endpoint)
				}
				if got := callbackService.Annotations[kubevip.ActiveEndpointIPv6]; got != tt.wantUpdate.endpointIPv6 {
					t.Errorf("callback IPv6 endpoint = %q, want %q", got, tt.wantUpdate.endpointIPv6)
				}
			}
		})
	}
}

func TestUpdateAnnotations_ZeroEndpointsThenSameEndpoint(t *testing.T) {
	t.Parallel()

	for _, enableEndpoints := range []bool{true, false} {
		name := "EndpointSlices"
		if enableEndpoints {
			name = "Endpoints"
		}
		t.Run(name, func(t *testing.T) {
			const endpoint = "10.0.0.1"
			service := &v1.Service{ObjectMeta: metav1.ObjectMeta{
				Name: "test-service", Namespace: "default", UID: "test-uid",
				Annotations: map[string]string{
					kubevip.Egress:         "true",
					kubevip.ActiveEndpoint: endpoint,
				},
			}}
			serviceInstance := &instance.Instance{ServiceSnapshot: service.DeepCopy()}
			instances := []*instance.Instance{serviceInstance}
			provider := providers.NewEndpointslices()
			if enableEndpoints {
				provider = providers.NewEndpoints()
			}
			recorder := &recordingProvider{Provider: provider}
			p := &Processor{
				config:    &kubevip.Config{EnableEndpoints: enableEndpoints},
				provider:  recorder,
				instances: &instances,
			}

			updateSnapshot := func(_ context.Context, svc *v1.Service) error {
				serviceInstance.ServiceSnapshot = svc
				return nil
			}

			noEndpoint := ""
			p.updateAnnotations(service, &noEndpoint, nil, updateSnapshot)
			repopulatedEndpoint := endpoint
			p.updateAnnotations(service, &repopulatedEndpoint, nil, updateSnapshot)

			want := []annotationUpdate{{}, {endpoint: endpoint}}
			if len(recorder.updates) != len(want) {
				t.Fatalf("got %d annotation updates, want %d", len(recorder.updates), len(want))
			}
			for i := range want {
				if recorder.updates[i] != want[i] {
					t.Errorf("annotation update %d = %+v, want %+v", i, recorder.updates[i], want[i])
				}
			}
		})
	}
}

func TestUpdateLastKnownGoodEndpoint_SelectsConfiguredFamily(t *testing.T) {
	t.Parallel()

	endpoints := []string{"fd00::20", "172.30.2.40"}
	for _, tt := range []struct {
		name        string
		annotations map[string]string
		want        string
	}{
		{
			name:        "IPv4 egress on dual-stack pods",
			annotations: map[string]string{kubevip.Egress: "true"},
			want:        "172.30.2.40",
		},
		{
			name:        "IPv6 egress on dual-stack pods",
			annotations: map[string]string{kubevip.Egress: "true", kubevip.EgressIPv6: "true"},
			want:        "fd00::20",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := &Processor{worker: &fakeWorker{}}
			service := &v1.Service{ObjectMeta: metav1.ObjectMeta{Annotations: tt.annotations}}
			selected := ""
			p.updateLastKnownGoodEndpoint(&selected, endpoints, service)
			if selected != tt.want {
				t.Fatalf("selected endpoint = %q, want %q", selected, tt.want)
			}
		})
	}
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
