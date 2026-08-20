package providers

import (
	"context"
	"testing"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestEndpointslicesTracksAndDeletesSlices(t *testing.T) {
	provider := NewEndpointslices().(*Endpointslices)
	serving := true
	nodeName := "node-1"

	slice1 := &discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Name: "slice-1"},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints: []discoveryv1.Endpoint{{
			Addresses:  []string{"10.0.0.1"},
			Conditions: discoveryv1.EndpointConditions{Serving: &serving},
			NodeName:   &nodeName,
		}},
	}
	slice2 := &discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Name: "slice-2"},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints: []discoveryv1.Endpoint{{
			Addresses:  []string{"10.0.0.2"},
			Conditions: discoveryv1.EndpointConditions{Serving: &serving},
			NodeName:   &nodeName,
		}},
	}

	for _, slice := range []*discoveryv1.EndpointSlice{slice1, slice2} {
		if err := provider.LoadObject(slice, func() {}); err != nil {
			t.Fatalf("LoadObject returned error: %v", err)
		}
	}

	assertEndpoints(t, provider, []string{"10.0.0.1", "10.0.0.2"})
	assertLocalEndpoints(t, provider, nodeName, []string{"10.0.0.1", "10.0.0.2"})

	if err := provider.DeleteObject(slice1); err != nil {
		t.Fatalf("DeleteObject returned error: %v", err)
	}
	assertEndpoints(t, provider, []string{"10.0.0.2"})
	assertLocalEndpoints(t, provider, nodeName, []string{"10.0.0.2"})

	if err := provider.DeleteObject(slice2); err != nil {
		t.Fatalf("DeleteObject returned error: %v", err)
	}
	assertEndpoints(t, provider, nil)
	assertLocalEndpoints(t, provider, nodeName, nil)
}

func TestEndpointslicesReplacingSliceUpdatesState(t *testing.T) {
	provider := NewEndpointslices().(*Endpointslices)
	first := &discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Name: "slice-1"},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints:   []discoveryv1.Endpoint{{Addresses: []string{"10.0.0.1"}}},
	}
	replacement := first.DeepCopy()
	replacement.Endpoints[0].Addresses = []string{"10.0.0.2"}

	if err := provider.LoadObject(first, context.CancelFunc(func() {})); err != nil {
		t.Fatalf("LoadObject returned error: %v", err)
	}
	if err := provider.LoadObject(replacement, context.CancelFunc(func() {})); err != nil {
		t.Fatalf("LoadObject returned error: %v", err)
	}

	assertEndpoints(t, provider, []string{"10.0.0.2"})
}

func assertEndpoints(t *testing.T, provider *Endpointslices, want []string) {
	t.Helper()
	got, err := provider.GetAllEndpoints()
	if err != nil {
		t.Fatalf("GetAllEndpoints returned error: %v", err)
	}
	assertStringSet(t, got, want)
}

func assertLocalEndpoints(t *testing.T, provider *Endpointslices, nodeName string, want []string) {
	t.Helper()
	got, err := provider.GetLocalEndpoints(nodeName, &kubevip.Config{})
	if err != nil {
		t.Fatalf("GetLocalEndpoints returned error: %v", err)
	}
	assertStringSet(t, got, want)
}

func assertStringSet(t *testing.T, got, want []string) {
	t.Helper()
	counts := map[string]int{}
	for _, value := range got {
		counts[value]++
	}
	for _, value := range want {
		counts[value]--
	}
	for value, count := range counts {
		if count != 0 {
			t.Fatalf("endpoint set mismatch for %q: got %v, want %v", value, got, want)
		}
	}
}
