package providers

import (
	"reflect"
	"testing"

	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestEndpointNodeMappings(t *testing.T) {
	node, hostname := "node-a", "pod-hostname"
	ready, notReady := true, false
	//nolint:staticcheck // Explicitly test the legacy Endpoints provider.
	legacy := &v1.Endpoints{Subsets: []v1.EndpointSubset{{
		Addresses: []v1.EndpointAddress{
			{IP: "10.0.0.1", NodeName: &node},
			{IP: "10.0.0.2", Hostname: hostname},
		},
		NotReadyAddresses: []v1.EndpointAddress{{IP: "10.0.0.3", NodeName: &node}},
	}}}
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{Name: "slice"},
		Endpoints: []discoveryv1.Endpoint{
			{Addresses: []string{"10.0.0.1"}, NodeName: &node, Conditions: discoveryv1.EndpointConditions{Ready: &ready}},
			{Addresses: []string{"10.0.0.2"}, Hostname: &hostname},
			{Addresses: []string{"10.0.0.3"}, NodeName: &node, Conditions: discoveryv1.EndpointConditions{Ready: &notReady}},
		},
	}
	for _, provider := range []Provider{NewEndpoints(), NewEndpointslices()} {
		t.Run(provider.GetLabel(), func(t *testing.T) {
			mapping := provider.(EndpointNodeProvider)
			if len(mapping.GetEndpointNodes()) != 0 {
				t.Fatal("new provider should have no node mappings")
			}
			if provider.GetLabel() == "endpoints" {
				if err := provider.LoadObject(legacy, func() {}); err != nil {
					t.Fatal(err)
				}
			} else if err := provider.LoadObject(slice, func() {}); err != nil {
				t.Fatal(err)
			}
			want := map[string][]string{node: {"10.0.0.1"}}
			if got := mapping.GetEndpointNodes(); !reflect.DeepEqual(got, want) {
				t.Fatalf("node mappings = %v, want %v", got, want)
			}
			if provider.GetLabel() == "endpoints" {
				if err := provider.DeleteObject(legacy); err != nil {
					t.Fatal(err)
				}
			} else if err := provider.DeleteObject(slice); err != nil {
				t.Fatal(err)
			}
			if len(mapping.GetEndpointNodes()) != 0 {
				t.Fatal("deleted endpoint objects retained node mappings")
			}
		})
	}
}
