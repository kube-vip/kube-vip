package providers

import (
	"context"
	"reflect"
	"testing"

	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestEndpointSlicesAggregateAllSlices(t *testing.T) {
	provider := NewEndpointslices()

	objects := []*discoveryv1.EndpointSlice{
		{
			ObjectMeta:  metav1.ObjectMeta{Name: "slice-v4-a"},
			AddressType: discoveryv1.AddressTypeIPv4,
			Endpoints:   []discoveryv1.Endpoint{{Addresses: []string{"10.0.0.1"}}},
		},
		{
			ObjectMeta:  metav1.ObjectMeta{Name: "slice-v4-b"},
			AddressType: discoveryv1.AddressTypeIPv4,
			Endpoints:   []discoveryv1.Endpoint{{Addresses: []string{"10.0.0.2"}}},
		},
		{
			ObjectMeta:  metav1.ObjectMeta{Name: "slice-v6-a"},
			AddressType: discoveryv1.AddressTypeIPv6,
			Endpoints:   []discoveryv1.Endpoint{{Addresses: []string{"2001:db8::1"}}},
		},
	}
	for _, object := range objects {
		if err := provider.LoadObject(object, func() {}); err != nil {
			t.Fatalf("LoadObject() error = %v", err)
		}
	}

	got, err := provider.GetAllEndpoints()
	if err != nil {
		t.Fatalf("GetAllEndpoints() error = %v", err)
	}
	want := map[string]struct{}{
		"10.0.0.1":    {},
		"10.0.0.2":    {},
		"2001:db8::1": {},
	}
	if actual := endpointSet(got); !reflect.DeepEqual(actual, want) {
		t.Fatalf("EndpointSlice endpoints = %v, want %v", actual, want)
	}
}

func TestEndpointslicesRetainsEndpointsFromMultipleSlices(t *testing.T) {
	provider := NewEndpointslices().(*Endpointslices)
	for name, address := range map[string]string{
		"slice-1": "10.0.0.1",
		"slice-2": "10.0.0.2",
	} {
		slice := &discoveryv1.EndpointSlice{
			ObjectMeta:  metav1.ObjectMeta{Name: name},
			AddressType: discoveryv1.AddressTypeIPv4,
			Endpoints:   []discoveryv1.Endpoint{{Addresses: []string{address}}},
		}
		if err := provider.LoadObject(slice, context.CancelFunc(func() {})); err != nil {
			t.Fatalf("LoadObject(%s) failed: %v", name, err)
		}
	}

	endpoints, err := provider.GetAllEndpoints()
	if err != nil {
		t.Fatalf("GetAllEndpoints failed: %v", err)
	}
	if len(endpoints) != 2 {
		t.Fatalf("GetAllEndpoints returned %v, want both EndpointSlice addresses", endpoints)
	}
}

func endpointSet(endpoints []string) map[string]struct{} {
	result := make(map[string]struct{}, len(endpoints))
	for _, endpoint := range endpoints {
		result[endpoint] = struct{}{}
	}
	return result
}
