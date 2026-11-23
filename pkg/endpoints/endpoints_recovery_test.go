package endpoints

import (
	"context"
	"sync"
	"testing"

	"github.com/kube-vip/kube-vip/pkg/endpoints/providers"
	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
)

// Retain real provider selection and endpoint processing, but simulate the
// network teardown so the regression can run without changing host SNAT rules.
type clearingEndpointWorker struct {
	endpointWorker
	clearCalled     bool
	clearedEndpoint string
}

func (w *clearingEndpointWorker) clear(_ context.Context, _ *sync.Map, lastKnownGoodEndpoint *string,
	_ *v1.Service, _ *instance.Instance) {
	w.clearCalled = true
	w.clearedEndpoint = *lastKnownGoodEndpoint
	*lastKnownGoodEndpoint = ""
}

func TestReconcilePersistsZeroEndpointTransition(t *testing.T) {
	for _, test := range []struct {
		name            string
		legacyEndpoints bool
		ipv6            bool
	}{
		{name: "Endpoints IPv4", legacyEndpoints: true},
		{name: "Endpoints IPv6", legacyEndpoints: true, ipv6: true},
		{name: "EndpointSlices IPv4"},
		{name: "EndpointSlices IPv6", ipv6: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			endpoint := "10.0.0.1"
			endpointKey := kubevip.ActiveEndpoint
			addressType := discoveryv1.AddressTypeIPv4
			if test.ipv6 {
				endpoint = "fd00::1"
				addressType = discoveryv1.AddressTypeIPv6
				if !test.legacyEndpoints {
					endpointKey = kubevip.ActiveEndpointIPv6
				}
			}
			service := &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name: "egress-test", Namespace: "default", UID: "egress-test",
					Annotations: map[string]string{kubevip.Egress: "true", endpointKey: endpoint},
				},
				Spec: v1.ServiceSpec{ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyLocal},
			}
			if test.ipv6 {
				service.Annotations[kubevip.EgressIPv6] = "true"
			}
			config := &kubevip.Config{
				EnableEndpoints: test.legacyEndpoints, EnableRoutingTable: true,
			}
			provider := providers.NewEndpointslices()
			if test.legacyEndpoints {
				provider = providers.NewEndpoints()
			}
			recorder := &recordingProvider{Provider: provider}
			worker := &clearingEndpointWorker{endpointWorker: newRoutingTable(newGeneric(config, recorder), nil)}
			inst := &instance.Instance{ServiceUID: service.UID, ServiceSnapshot: service.DeepCopy()}
			processor := &Processor{
				config: config, provider: recorder, worker: worker, serviceLocks: noOpServiceLocker{},
				findServiceInstance: func(*v1.Service) *instance.Instance { return inst },
			}
			svcCtx := servicecontext.New(context.Background())
			defer svcCtx.Cancel()
			svcCtx.SignalReadiness()
			lastKnown := endpoint
			var callbacks []string
			reconcile := func(present bool) {
				t.Helper()
				metadata := metav1.ObjectMeta{Name: service.Name, Namespace: service.Namespace}
				event := watch.Event{Type: watch.Modified}
				nodeName := "node-1"
				if test.legacyEndpoints {
					//nolint:staticcheck // Deliberately exercise the legacy Endpoints provider.
					object := &v1.Endpoints{ObjectMeta: metadata}
					if present {
						//nolint:staticcheck // Deliberately exercise the legacy Endpoints provider.
						object.Subsets = []v1.EndpointSubset{{Addresses: []v1.EndpointAddress{{IP: endpoint, NodeName: &nodeName}}}}
					}
					event.Object = object
				} else {
					object := &discoveryv1.EndpointSlice{ObjectMeta: metadata, AddressType: addressType}
					if present {
						object.Endpoints = []discoveryv1.Endpoint{{Addresses: []string{endpoint}, NodeName: &nodeName}}
					}
					event.Object = object
				}
				_, err := processor.Reconcile(svcCtx, event, &lastKnown, service, nodeName, &sync.WaitGroup{}, nil,
					func(_ context.Context, updated *v1.Service, _ *instance.Instance) error {
						callbacks = append(callbacks, updated.Annotations[endpointKey])
						inst.ServiceSnapshot = updated
						return nil
					})
				if err != nil {
					t.Fatalf("Reconcile(present=%t) failed: %v", present, err)
				}
			}

			reconcile(false)
			if !worker.clearCalled || worker.clearedEndpoint != endpoint || lastKnown != "" || svcCtx.IsReady() {
				t.Fatalf("endpoint teardown did not complete: cleared=%t removed=%q endpoint=%q ready=%t",
					worker.clearCalled, worker.clearedEndpoint, lastKnown, svcCtx.IsReady())
			}
			if len(recorder.updates) != 1 || recorder.updates[0] != (annotationUpdate{}) {
				t.Errorf("the removed endpoint was not cleared from the Service: updates=%+v", recorder.updates)
			}

			reconcile(true)
			if !svcCtx.IsReady() || lastKnown != endpoint {
				t.Fatalf("the endpoint did not become ready again: ready=%t endpoint=%q", svcCtx.IsReady(), lastKnown)
			}
			if len(callbacks) != 2 || callbacks[0] != "" || callbacks[1] != endpoint {
				t.Fatalf("egress callbacks = %q, want an empty endpoint followed by %q", callbacks, endpoint)
			}
			want := annotationUpdate{endpoint: endpoint}
			if endpointKey == kubevip.ActiveEndpointIPv6 {
				want = annotationUpdate{endpointIPv6: endpoint}
			}
			if len(recorder.updates) != 2 || recorder.updates[1] != want {
				t.Fatalf("the restored endpoint was not persisted: updates=%+v", recorder.updates)
			}
		})
	}
}
