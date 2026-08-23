package providers

import (
	"testing"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
)

func TestEndpointProvidersDoNotOverrideNodeNameWithHostname(t *testing.T) {
	nodeA := "node-a"
	nodeB := "node-b"

	t.Run("legacy Endpoints", func(t *testing.T) {
		provider := NewEndpoints()
		//nolint:staticcheck // the legacy provider is deliberately under test
		if err := provider.LoadObject(&v1.Endpoints{
			Subsets: []v1.EndpointSubset{{
				Addresses: []v1.EndpointAddress{{
					IP:       "10.0.0.1",
					NodeName: &nodeB,
					Hostname: nodeA,
				}},
			}},
		}, func() {}); err != nil {
			t.Fatalf("LoadObject() error = %v", err)
		}

		got, err := provider.GetLocalEndpoints(nodeA, &kubevip.Config{})
		if err != nil {
			t.Fatalf("GetLocalEndpoints() error = %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("legacy local endpoints = %v, want none", got)
		}
	})

	t.Run("EndpointSlice", func(t *testing.T) {
		serving := true
		provider := NewEndpointslices()
		if err := provider.LoadObject(&discoveryv1.EndpointSlice{
			AddressType: discoveryv1.AddressTypeIPv4,
			Endpoints: []discoveryv1.Endpoint{{
				Addresses:  []string{"10.0.0.1"},
				NodeName:   &nodeB,
				Hostname:   stringPtr(nodeA),
				Conditions: discoveryv1.EndpointConditions{Serving: &serving},
			}},
		}, func() {}); err != nil {
			t.Fatalf("LoadObject() error = %v", err)
		}

		got, err := provider.GetLocalEndpoints(nodeA, &kubevip.Config{})
		if err != nil {
			t.Fatalf("GetLocalEndpoints() error = %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("EndpointSlice local endpoints = %v, want none", got)
		}
	})
}

func stringPtr(value string) *string {
	return &value
}
