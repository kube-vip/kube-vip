package providers

import (
	"reflect"
	"testing"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func TestEndpointProvidersConformance(t *testing.T) {
	t.Parallel()

	nodeA := "node-a"
	nodeB := "node-b"
	serving := true

	portName := "web"
	port := int32(8080)

	//nolint:staticcheck // this test deliberately covers the legacy provider too
	legacy := &v1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: "default"},
		//nolint:staticcheck // deprecated legacy Endpoints API is under test
		Subsets: []v1.EndpointSubset{{
			Addresses: []v1.EndpointAddress{
				{IP: "10.0.0.1", NodeName: &nodeA},
				{IP: "10.0.0.2", NodeName: &nodeB},
				{IP: "2001:db8::1", NodeName: &nodeA},
			},
			Ports: []v1.EndpointPort{{Name: portName, Port: port}},
		}},
	}

	sliceV4A := &discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Name: "slice-v4-a", Namespace: "default"},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints: []discoveryv1.Endpoint{{
			Addresses:  []string{"10.0.0.1"},
			NodeName:   &nodeA,
			Conditions: discoveryv1.EndpointConditions{Serving: &serving},
		}},
		Ports: []discoveryv1.EndpointPort{{Name: &portName, Port: &port}},
	}
	sliceV4B := &discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Name: "slice-v4-b", Namespace: "default"},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints: []discoveryv1.Endpoint{{
			Addresses:  []string{"10.0.0.2"},
			NodeName:   &nodeB,
			Hostname:   stringPtr(nodeB),
			Conditions: discoveryv1.EndpointConditions{Serving: &serving},
		}},
		Ports: []discoveryv1.EndpointPort{{Name: &portName, Port: &port}},
	}
	sliceV6 := &discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Name: "slice-v6", Namespace: "default"},
		AddressType: discoveryv1.AddressTypeIPv6,
		Endpoints: []discoveryv1.Endpoint{{
			Addresses:  []string{"2001:db8::1"},
			NodeName:   &nodeA,
			Conditions: discoveryv1.EndpointConditions{Serving: &serving},
		}},
		Ports: []discoveryv1.EndpointPort{{Name: &portName, Port: &port}},
	}

	tests := []struct {
		name     string
		provider Provider
		objects  []runtime.Object
	}{
		{name: "legacy Endpoints", provider: NewEndpoints(), objects: []runtime.Object{legacy}},
		{name: "EndpointSlices", provider: NewEndpointslices(), objects: []runtime.Object{sliceV4A, sliceV4B, sliceV6}},
	}

	wantAll := endpointSet([]string{"10.0.0.1", "10.0.0.2", "2001:db8::1"})
	wantLocal := endpointSet([]string{"10.0.0.1", "2001:db8::1"})
	servicePort := v1.ServicePort{Port: 80, TargetPort: intstr.FromString("web")}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			for _, object := range tt.objects {
				if err := tt.provider.LoadObject(object, func() {}); err != nil {
					t.Fatalf("LoadObject(%T) error = %v", object, err)
				}
			}

			all, err := tt.provider.GetAllEndpoints()
			if err != nil {
				t.Fatalf("GetAllEndpoints() error = %v", err)
			}
			if got := endpointSet(all); !reflect.DeepEqual(got, wantAll) {
				t.Errorf("all endpoints = %v, want %v", got, wantAll)
			}

			local, err := tt.provider.GetLocalEndpoints(nodeA, &kubevip.Config{})
			if err != nil {
				t.Fatalf("GetLocalEndpoints() error = %v", err)
			}
			if got := endpointSet(local); !reflect.DeepEqual(got, wantLocal) {
				t.Errorf("local endpoints = %v, want %v", got, wantLocal)
			}

			if got := tt.provider.ResolvePort(servicePort); got != port {
				t.Errorf("ResolvePort() = %d, want %d", got, port)
			}
		})
	}
}

func TestEndpointProviderDeletionRemovesOnlyDeletedObject(t *testing.T) {
	t.Parallel()

	firstPortName := "first"
	firstPort := int32(8081)
	secondPortName := "second"
	secondPort := int32(8082)
	provider := NewEndpointslices()
	first := &discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Name: "slice-a"},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints:   []discoveryv1.Endpoint{{Addresses: []string{"10.0.0.1"}}},
		Ports:       []discoveryv1.EndpointPort{{Name: &firstPortName, Port: &firstPort}},
	}
	second := &discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Name: "slice-b"},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints:   []discoveryv1.Endpoint{{Addresses: []string{"10.0.0.2"}}},
		Ports:       []discoveryv1.EndpointPort{{Name: &secondPortName, Port: &secondPort}},
	}

	for _, object := range []*discoveryv1.EndpointSlice{first, second} {
		if err := provider.LoadObject(object, func() {}); err != nil {
			t.Fatalf("LoadObject() error = %v", err)
		}
	}

	if err := provider.DeleteObject(first); err != nil {
		t.Fatalf("DeleteObject() error = %v", err)
	}
	got, err := provider.GetAllEndpoints()
	if err != nil {
		t.Fatalf("GetAllEndpoints() error = %v", err)
	}
	if want := endpointSet([]string{"10.0.0.2"}); !reflect.DeepEqual(endpointSet(got), want) {
		t.Fatalf("endpoints after deleting first slice = %v, want %v", got, want)
	}
	if got := provider.ResolvePort(v1.ServicePort{Port: 80, TargetPort: intstr.FromString(firstPortName)}); got != 80 {
		t.Fatalf("deleted slice port = %d, want service-port fallback 80", got)
	}
	if got := provider.ResolvePort(v1.ServicePort{Port: 80, TargetPort: intstr.FromString(secondPortName)}); got != secondPort {
		t.Fatalf("retained slice port = %d, want %d", got, secondPort)
	}

	replacement := second.DeepCopy()
	replacement.Endpoints = []discoveryv1.Endpoint{{Addresses: []string{"10.0.0.3"}}}
	replacementPort := int32(8083)
	replacement.Ports = []discoveryv1.EndpointPort{{Name: &secondPortName, Port: &replacementPort}}
	if err := provider.LoadObject(replacement, func() {}); err != nil {
		t.Fatalf("LoadObject(replacement) error = %v", err)
	}
	got, err = provider.GetAllEndpoints()
	if err != nil {
		t.Fatalf("GetAllEndpoints() after replacement error = %v", err)
	}
	if want := endpointSet([]string{"10.0.0.3"}); !reflect.DeepEqual(endpointSet(got), want) {
		t.Fatalf("endpoints after replacing second slice = %v, want %v", got, want)
	}
	if got := provider.ResolvePort(v1.ServicePort{Port: 80, TargetPort: intstr.FromString(secondPortName)}); got != replacementPort {
		t.Fatalf("replaced slice port = %d, want %d", got, replacementPort)
	}
}

func TestEndpointProviderDeleteObjectClearsState(t *testing.T) {
	t.Parallel()

	//nolint:staticcheck // the legacy provider is part of the common interface contract
	legacyObject := &v1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{Name: "service"},
		//nolint:staticcheck // deprecated legacy Endpoints API is under test
		Subsets: []v1.EndpointSubset{{
			Addresses: []v1.EndpointAddress{{IP: "10.0.0.1"}},
		}},
	}
	sliceObject := &discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Name: "slice"},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints:   []discoveryv1.Endpoint{{Addresses: []string{"10.0.0.1"}}},
	}

	tests := []struct {
		name     string
		provider Provider
		object   runtime.Object
	}{
		{name: "legacy Endpoints", provider: NewEndpoints(), object: legacyObject},
		{name: "EndpointSlice", provider: NewEndpointslices(), object: sliceObject},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := tt.provider.LoadObject(tt.object, func() {}); err != nil {
				t.Fatalf("LoadObject() error = %v", err)
			}
			if err := tt.provider.DeleteObject(tt.object); err != nil {
				t.Fatalf("DeleteObject() error = %v", err)
			}

			all, err := tt.provider.GetAllEndpoints()
			if err != nil {
				t.Fatalf("GetAllEndpoints() after delete error = %v", err)
			}
			if len(all) != 0 {
				t.Fatalf("GetAllEndpoints() after delete = %v, want empty", all)
			}
		})
	}
}

func TestEndpointSlicesFilterNonServingLocalEndpoints(t *testing.T) {
	provider := NewEndpointslices()
	node := "node-a"
	serving := true
	notServing := false
	if err := provider.LoadObject(&discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Name: "slice"},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints: []discoveryv1.Endpoint{
			{Addresses: []string{"10.0.0.1"}, NodeName: &node, Conditions: discoveryv1.EndpointConditions{Serving: &serving}},
			{Addresses: []string{"10.0.0.2"}, NodeName: &node, Conditions: discoveryv1.EndpointConditions{Serving: &notServing}},
		},
	}, func() {}); err != nil {
		t.Fatalf("LoadObject() error = %v", err)
	}

	local, err := provider.GetLocalEndpoints(node, &kubevip.Config{})
	if err != nil {
		t.Fatalf("GetLocalEndpoints() error = %v", err)
	}
	if want := endpointSet([]string{"10.0.0.1"}); !reflect.DeepEqual(endpointSet(local), want) {
		t.Fatalf("local endpoints = %v, want %v", local, want)
	}
}

func endpointSet(endpoints []string) map[string]struct{} {
	result := make(map[string]struct{}, len(endpoints))
	for _, endpoint := range endpoints {
		result[endpoint] = struct{}{}
	}
	return result
}

func stringPtr(value string) *string {
	return &value
}
