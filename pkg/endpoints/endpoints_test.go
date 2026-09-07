package endpoints

import (
	"context"
	"sync"
	"testing"

	"github.com/kube-vip/kube-vip/pkg/cluster"
	"github.com/kube-vip/kube-vip/pkg/endpoints/providers"
	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/route"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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
	processHook   func()
}

type annotationUpdate struct {
	endpoint     string
	endpointIPv6 string
}

type recordingProvider struct {
	providers.Provider
	updates []annotationUpdate
}

func (p *recordingProvider) UpdateServiceAnnotation(_ context.Context, endpoint, endpointIPv6 string,
	_ *v1.Service, _ *kubernetes.Clientset) error {
	p.updates = append(p.updates, annotationUpdate{endpoint: endpoint, endpointIPv6: endpointIPv6})
	return nil
}

func TestUpdateAnnotationsZeroEndpointsThenSameEndpoint(t *testing.T) {
	for _, enableEndpoints := range []bool{true, false} {
		providerName := "EndpointSlices"
		provider := providers.NewEndpointslices()
		if enableEndpoints {
			providerName = "Endpoints"
			provider = providers.NewEndpoints()
		}

		for _, family := range []struct {
			name       string
			endpoint   string
			other      string
			egressIPv6 bool
		}{
			{name: "IPv4", endpoint: "10.0.0.1", other: "fd00::1"},
			{name: "IPv6", endpoint: "fd00::1", other: "10.0.0.1", egressIPv6: true},
		} {
			t.Run(providerName+"/"+family.name, func(t *testing.T) {
				annotations := map[string]string{kubevip.Egress: "true"}
				if family.egressIPv6 {
					annotations[kubevip.EgressIPv6] = "true"
				}
				if !enableEndpoints {
					if family.egressIPv6 {
						annotations[kubevip.ActiveEndpoint] = family.other
						annotations[kubevip.ActiveEndpointIPv6] = family.endpoint
					} else {
						annotations[kubevip.ActiveEndpoint] = family.endpoint
						annotations[kubevip.ActiveEndpointIPv6] = family.other
					}
				} else {
					annotations[kubevip.ActiveEndpoint] = family.endpoint
				}
				service := &v1.Service{ObjectMeta: metav1.ObjectMeta{
					Name: "test-service", Namespace: "default", UID: "test-uid", Annotations: annotations,
				}}
				serviceInstance := &instance.Instance{ServiceSnapshot: service.DeepCopy()}
				instances := []*instance.Instance{serviceInstance}
				recorder := &recordingProvider{Provider: provider}
				processor := &Processor{
					config:    &kubevip.Config{EnableEndpoints: enableEndpoints},
					provider:  recorder,
					instances: &instances,
				}

				noEndpoint := ""
				updated, changed := processor.updateAnnotations(service, serviceInstance, &noEndpoint, nil)
				if changed {
					serviceInstance.ServiceSnapshot = updated
				}
				repopulatedEndpoint := family.endpoint
				updated, changed = processor.updateAnnotations(service, serviceInstance, &repopulatedEndpoint, nil)
				if changed {
					serviceInstance.ServiceSnapshot = updated
				}

				cleared := annotationUpdate{}
				repopulated := annotationUpdate{endpoint: family.endpoint}
				if !enableEndpoints {
					if family.egressIPv6 {
						cleared = annotationUpdate{endpoint: family.other}
						repopulated = annotationUpdate{endpoint: family.other, endpointIPv6: family.endpoint}
					} else {
						cleared = annotationUpdate{endpointIPv6: family.other}
						repopulated = annotationUpdate{endpoint: family.endpoint, endpointIPv6: family.other}
					}
				}
				want := []annotationUpdate{cleared, repopulated}
				if len(recorder.updates) != len(want) {
					t.Fatalf("annotation updates = %+v, want %+v", recorder.updates, want)
				}
				for index := range want {
					if recorder.updates[index] != want[index] {
						t.Errorf("annotation update %d = %+v, want %+v", index, recorder.updates[index], want[index])
					}
				}
			})
		}
	}
}

func TestUpdateAnnotationsEndpointSlicesClearsConfiguredFamily(t *testing.T) {
	for _, test := range []struct {
		name       string
		egressIPv6 bool
		want       annotationUpdate
	}{
		{name: "IPv4", want: annotationUpdate{endpointIPv6: "fd00::1"}},
		{name: "IPv6", egressIPv6: true, want: annotationUpdate{endpoint: "10.0.0.1"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			annotations := map[string]string{
				kubevip.Egress:             "true",
				kubevip.ActiveEndpoint:     "10.0.0.1",
				kubevip.ActiveEndpointIPv6: "fd00::1",
			}
			if test.egressIPv6 {
				annotations[kubevip.EgressIPv6] = "true"
			}
			service := &v1.Service{ObjectMeta: metav1.ObjectMeta{
				Name: "test-service", Namespace: "default", UID: "test-uid", Annotations: annotations,
			}}
			instances := []*instance.Instance{{ServiceSnapshot: service.DeepCopy()}}
			recorder := &recordingProvider{Provider: providers.NewEndpointslices()}
			processor := &Processor{
				config:    &kubevip.Config{EnableEndpoints: false},
				provider:  recorder,
				instances: &instances,
			}

			noEndpoint := ""
			processor.updateAnnotations(service, instances[0], &noEndpoint, nil)

			if len(recorder.updates) != 1 || recorder.updates[0] != test.want {
				t.Fatalf("annotation updates = %+v, want [%+v]", recorder.updates, test.want)
			}
		})
	}
}

func TestUpdateAnnotationsValidatesEndpointFamily(t *testing.T) {
	tests := []struct {
		name       string
		endpoint   string
		egressIPv6 bool
		want       annotationUpdate
		wantUpdate bool
	}{
		{name: "invalid address", endpoint: "not-an-ip"},
		{name: "IPv6 endpoint for IPv4 egress", endpoint: "fd00::1"},
		{name: "IPv4 endpoint for IPv6 egress", endpoint: "10.0.0.1", egressIPv6: true},
		{name: "IPv4 endpoint", endpoint: "10.0.0.2", want: annotationUpdate{endpoint: "10.0.0.2", endpointIPv6: "fd00::1"}, wantUpdate: true},
		{name: "IPv6 endpoint", endpoint: "fd00::2", egressIPv6: true, want: annotationUpdate{endpoint: "10.0.0.1", endpointIPv6: "fd00::2"}, wantUpdate: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			annotations := map[string]string{
				kubevip.Egress:             "true",
				kubevip.ActiveEndpoint:     "10.0.0.1",
				kubevip.ActiveEndpointIPv6: "fd00::1",
			}
			if test.egressIPv6 {
				annotations[kubevip.EgressIPv6] = "true"
			}
			service := &v1.Service{ObjectMeta: metav1.ObjectMeta{
				Name: "test-service", Namespace: "default", Annotations: annotations,
			}}
			recorder := &recordingProvider{Provider: providers.NewEndpointslices()}
			processor := &Processor{
				config:   &kubevip.Config{EnableEndpoints: false},
				provider: recorder,
			}

			processor.updateAnnotations(service, nil, &test.endpoint, nil)

			if !test.wantUpdate {
				if len(recorder.updates) != 0 {
					t.Fatalf("annotation updates = %+v, want none", recorder.updates)
				}
				return
			}
			if len(recorder.updates) != 1 || recorder.updates[0] != test.want {
				t.Fatalf("annotation updates = %+v, want [%+v]", recorder.updates, test.want)
			}
		})
	}
}

func (f *fakeWorker) processInstance(_ context.Context, _ *sync.Map, _ *v1.Service, _ *instance.Instance) error {
	if f.processHook != nil {
		f.processHook()
	}
	f.processCalled = true
	return nil
}

func (f *fakeWorker) clear(_ context.Context, _ *sync.Map, _ *string, _ *v1.Service, _ *instance.Instance) {
	f.clearCalled = true
}

func (f *fakeWorker) getEndpoints(_ *v1.Service, _ string) ([]string, error) { return f.endpoints, nil }
func (f *fakeWorker) removeEgress(_ *v1.Service, _ *string)                  {}
func (f *fakeWorker) setInstanceEndpointsStatus(_ *v1.Service, _ *instance.Instance, _ []string) error {
	return nil
}

func noOpServiceLock(types.UID) func() {
	return func() {}
}

// TestReconcile_RecomputesRemainingEndpoints asserts that deleting one EndpointSlice
// reconciles against the endpoints that remain, instead of assuming the service
// lost all of them.
func TestReconcile_RecomputesRemainingEndpoints(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		remaining         []string
		lastKnown         string
		expectReady       bool
		expectClear       bool
		expectProcess     bool
		expectedLastKnown string
	}{
		{
			name:              "remaining endpoints keep the service up",
			remaining:         []string{"10.0.0.2"},
			lastKnown:         "10.0.0.2",
			expectReady:       true,
			expectProcess:     true,
			expectedLastKnown: "10.0.0.2",
		},
		{
			name:              "stale last known endpoint moves to a survivor",
			remaining:         []string{"10.0.0.2"},
			lastKnown:         "10.0.0.1",
			expectReady:       true,
			expectProcess:     true,
			expectedLastKnown: "10.0.0.2",
		},
		{
			name:        "last endpoint removed tears the service down",
			remaining:   nil,
			lastKnown:   "10.0.0.1",
			expectReady: false,
			expectClear: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			worker := &fakeWorker{endpoints: test.remaining}
			p := &Processor{
				config:      &kubevip.Config{},
				provider:    providers.NewEndpointslices(),
				worker:      worker,
				lockService: noOpServiceLock,
			}

			svcCtx := servicecontext.New(context.Background())
			svcCtx.SignalReadiness()

			lastKnown := test.lastKnown
			restart, err := p.Reconcile(
				svcCtx,
				watch.Event{
					Type:   watch.Deleted,
					Object: &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Name: "slice-1"}},
				},
				&lastKnown,
				&v1.Service{Spec: v1.ServiceSpec{ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeLocal}},
				"node-1",
				&sync.WaitGroup{},
				nil,
				nil,
			)
			if err != nil {
				t.Fatalf("Reconcile returned error: %v", err)
			}
			if restart {
				t.Fatal("Reconcile unexpectedly requested restart")
			}

			if ready := svcCtx.IsReady(); ready != test.expectReady {
				t.Fatalf("readiness mismatch: expected %v, got %v", test.expectReady, ready)
			}
			if worker.clearCalled != test.expectClear {
				t.Fatalf("clearCalled mismatch: expected %v, got %v", test.expectClear, worker.clearCalled)
			}
			if worker.processCalled != test.expectProcess {
				t.Fatalf("processCalled mismatch: expected %v, got %v", test.expectProcess, worker.processCalled)
			}
			if test.expectedLastKnown != "" && lastKnown != test.expectedLastKnown {
				t.Fatalf("lastKnownGoodEndpoint mismatch: expected %q, got %q", test.expectedLastKnown, lastKnown)
			}
		})
	}
}

func TestReconcile_ZeroEndpointsBehavior(t *testing.T) {
	t.Parallel()

	run := func(t *testing.T, service *v1.Service, presetSignalled bool, expectReady bool, expectClear bool, expectProcess bool) {
		t.Helper()

		worker := &fakeWorker{endpoints: []string{}}
		p := &Processor{
			config:      &kubevip.Config{},
			provider:    providers.NewEndpointslices(),
			worker:      worker,
			lockService: noOpServiceLock,
		}

		svcCtx := servicecontext.New(context.Background())
		if presetSignalled {
			svcCtx.SignalReadiness()
		}

		restart, err := p.Reconcile(
			svcCtx,
			watch.Event{Type: watch.Modified, Object: &discoveryv1.EndpointSlice{}},
			new(string),
			service,
			"node-1",
			&sync.WaitGroup{},
			nil,
			nil,
		)
		if err != nil {
			t.Fatalf("Reconcile returned error: %v", err)
		}
		if restart {
			t.Fatal("Reconcile unexpectedly requested restart")
		}

		if ready := svcCtx.IsReady(); ready != expectReady {
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

func TestHandleNoEndpointsStopsGlobalRoutingTableWorkers(t *testing.T) {
	config := &kubevip.Config{EnableRoutingTable: true, KubernetesLeaderElection: kubevip.KubernetesLeaderElection{EnableLeaderElection: true}}
	serviceCluster, err := cluster.InitCluster(&kubevip.Config{}, true, nil, nil, route.NewManager(), nil)
	if err != nil {
		t.Fatalf("initializing Service cluster: %v", err)
	}
	var workers sync.WaitGroup
	if err := serviceCluster.StartLoadBalancerService(context.Background(), config, nil, "service", &workers); err != nil {
		t.Fatalf("starting Service cluster: %v", err)
	}
	instance := &instance.Instance{Clusters: []*cluster.Cluster{serviceCluster}}
	processor := &Processor{config: config, worker: &fakeWorker{}}

	processor.handleNoEndpoints(servicecontext.New(context.Background()), &v1.Service{}, instance, new(string))
	if err := serviceCluster.StartLoadBalancerService(context.Background(), config, nil, "service", &workers); err != nil {
		t.Fatalf("starting Service cluster after endpoint loss: %v", err)
	}
	serviceCluster.StopAndWait()
	workers.Wait()
}

func TestReconcileIPv6EgressWithoutIPv6EndpointsClearsReadiness(t *testing.T) {
	worker := &fakeWorker{endpoints: []string{"192.0.2.10"}}
	processor := &Processor{
		config:      &kubevip.Config{},
		provider:    providers.NewEndpointslices(),
		worker:      worker,
		lockService: noOpServiceLock,
	}
	service := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{kubevip.EgressIPv6: "true"}},
		Spec:       v1.ServiceSpec{ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeLocal},
	}
	svcCtx := servicecontext.New(context.Background())
	svcCtx.SignalReadiness()

	restart, err := processor.Reconcile(svcCtx, watch.Event{Type: watch.Modified, Object: &discoveryv1.EndpointSlice{}},
		new(string), service, "node", &sync.WaitGroup{}, nil, nil)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if restart {
		t.Fatal("Reconcile requested a restart for an unusable endpoint family")
	}
	if svcCtx.IsReady() {
		t.Fatal("IPv6 egress remained ready with only IPv4 endpoints")
	}
	if !worker.clearCalled {
		t.Fatal("IPv6 egress did not clear the worker with only IPv4 endpoints")
	}
}

func TestSharedServiceVIPRequiresClusterPolicyAndElectionLease(t *testing.T) {
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "first", Namespace: "default", Annotations: map[string]string{kubevip.ServiceLease: "shared"},
	}, Spec: v1.ServiceSpec{LoadBalancerIP: "192.0.2.10", ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeCluster}}
	candidate := service.DeepCopy()
	candidate.Name = "second"
	first := &instance.Instance{ServiceSnapshot: service}
	second := &instance.Instance{ServiceSnapshot: candidate}

	if len(sharedServiceVIPs(&kubevip.Config{EnableServicesElection: true}, first, []*instance.Instance{second})) == 0 {
		t.Fatal("shared lease Services with Cluster traffic policy did not share their VIP")
	}
	if len(sharedServiceVIPs(&kubevip.Config{}, first, []*instance.Instance{second})) == 0 {
		t.Fatal("Cluster policy Services without per-Service election did not share their VIP")
	}
	candidate.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyTypeLocal
	if len(sharedServiceVIPs(&kubevip.Config{}, first, []*instance.Instance{second})) != 0 {
		t.Fatal("Local traffic policy Services shared a VIP")
	}
	candidate.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyTypeCluster
	candidate.Annotations[kubevip.ServiceLease] = "different"
	if len(sharedServiceVIPs(&kubevip.Config{EnableServicesElection: true}, first, []*instance.Instance{second})) != 0 {
		t.Fatal("Services with different leases shared a VIP")
	}
}

func TestSharedServiceVIPsReturnsOnlyOverlappingAddresses(t *testing.T) {
	first := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "first", Namespace: "default", Annotations: map[string]string{kubevip.ServiceLease: "shared"},
	}, Spec: v1.ServiceSpec{LoadBalancerIP: "192.0.2.10", ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeCluster}}
	second := first.DeepCopy()
	second.Name = "second"
	second.Spec.LoadBalancerIP = "192.0.2.11"
	firstInstance := &instance.Instance{ServiceSnapshot: first, ServiceAddresses: []string{"192.0.2.10", "2001:db8::10"}}
	secondInstance := &instance.Instance{ServiceSnapshot: second, ServiceAddresses: []string{"192.0.2.10", "2001:db8::11"}}

	shared := sharedServiceVIPs(&kubevip.Config{EnableServicesElection: true}, firstInstance, []*instance.Instance{secondInstance})
	if len(shared) != 1 {
		t.Fatalf("shared VIPs = %v, want exactly one", shared)
	}
	if _, found := shared["192.0.2.10"]; !found {
		t.Fatalf("shared VIPs = %v, missing overlapping IPv4 address", shared)
	}
}

// TestReconcileServicesElectionDoesNotStartElectionLoop asserts endpoint events
// only update readiness. The services coordinator owns the election loop.
func TestReconcileServicesElectionDoesNotStartElectionLoop(t *testing.T) {
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

	wg := &sync.WaitGroup{}
	defer wg.Wait()
	defer svcCtx.Cancel()

	p := &Processor{
		config:      config,
		provider:    providers.NewEndpointslices(),
		worker:      &fakeWorker{endpoints: []string{"10.0.0.1"}},
		leaseMgr:    leaseMgr,
		lockService: noOpServiceLock,
	}

	// Three endpoint events, as a flapping backend pod would produce.
	for range 3 {
		restart, err := p.Reconcile(svcCtx, watch.Event{Type: watch.Modified, Object: &discoveryv1.EndpointSlice{}},
			new(string), service, "node-1", wg, nil, nil)
		if err != nil {
			t.Fatalf("Reconcile returned error: %v", err)
		}
		if restart {
			t.Fatal("Reconcile unexpectedly requested restart")
		}
	}

	generation, ready, lost, isReady := svcCtx.ReadinessState()
	if generation != 1 || !isReady {
		t.Fatalf("readiness state = generation %d, ready %t; want generation 1 ready", generation, isReady)
	}
	select {
	case <-ready:
	default:
		t.Fatal("endpoint reconciliation did not signal readiness")
	}
	select {
	case <-lost:
		t.Fatal("endpoint reconciliation unexpectedly reset readiness")
	default:
	}
}
