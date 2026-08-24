package providers

import (
	"net"
	"reflect"
	"testing"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
)

func TestEndpointProvidersParityForLocalAndAllEndpoints(t *testing.T) {
	t.Parallel()

	nodeA := "node-a"
	nodeB := "node-b"
	serving := true
	v4Addresses := []discoveryv1.Endpoint{
		{Addresses: []string{"10.0.0.1"}, NodeName: &nodeA, Conditions: discoveryv1.EndpointConditions{Serving: &serving}},
		{Addresses: []string{"10.0.0.2"}, NodeName: &nodeB, Conditions: discoveryv1.EndpointConditions{Serving: &serving}},
	}
	v6Addresses := []discoveryv1.Endpoint{
		{Addresses: []string{"2001:db8::1"}, NodeName: &nodeA, Conditions: discoveryv1.EndpointConditions{Serving: &serving}},
		{Addresses: []string{"2001:db8::2"}, NodeName: &nodeB, Conditions: discoveryv1.EndpointConditions{Serving: &serving}},
	}

	legacy := NewEndpoints()
	//nolint:staticcheck // this test covers the deprecated legacy Endpoints provider on purpose
	if err := legacy.LoadObject(&v1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: "default"},
		//nolint:staticcheck // deprecated legacy Endpoints API is the subject under test
		Subsets: []v1.EndpointSubset{{
			Addresses: []v1.EndpointAddress{
				{IP: "10.0.0.1", NodeName: &nodeA},
				{IP: "10.0.0.2", NodeName: &nodeB},
				{IP: "2001:db8::1", NodeName: &nodeA},
				{IP: "2001:db8::2", NodeName: &nodeB},
			},
		}},
	}, func() {}); err != nil {
		t.Fatalf("loading legacy Endpoints: %v", err)
	}

	slices := NewEndpointslices()
	if err := slices.LoadObject(&discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Name: "service-v4", Namespace: "default"},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints:   v4Addresses,
	}, func() {}); err != nil {
		t.Fatalf("loading IPv4 EndpointSlice: %v", err)
	}
	if err := slices.LoadObject(&discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Name: "service-v6", Namespace: "default"},
		AddressType: discoveryv1.AddressTypeIPv6,
		Endpoints:   v6Addresses,
	}, func() {}); err != nil {
		t.Fatalf("loading IPv6 EndpointSlice: %v", err)
	}

	legacyAll, err := legacy.GetAllEndpoints()
	if err != nil {
		t.Fatalf("legacy GetAllEndpoints() error = %v", err)
	}
	sliceAll, err := slices.GetAllEndpoints()
	if err != nil {
		t.Fatalf("EndpointSlice GetAllEndpoints() error = %v", err)
	}
	wantAll := endpointSet([]string{"10.0.0.1", "10.0.0.2", "2001:db8::1", "2001:db8::2"})
	if got := endpointSet(legacyAll); !reflect.DeepEqual(got, wantAll) {
		t.Errorf("legacy all endpoints = %v, want %v", got, wantAll)
	}
	if got := endpointSet(sliceAll); !reflect.DeepEqual(got, wantAll) {
		t.Errorf("EndpointSlice all endpoints = %v, want %v", got, wantAll)
	}

	legacyLocal, err := legacy.GetLocalEndpoints(nodeA, &kubevip.Config{})
	if err != nil {
		t.Fatalf("legacy GetLocalEndpoints() error = %v", err)
	}
	sliceLocal, err := slices.GetLocalEndpoints(nodeA, &kubevip.Config{})
	if err != nil {
		t.Fatalf("EndpointSlice GetLocalEndpoints() error = %v", err)
	}
	wantLocal := endpointSet([]string{"10.0.0.1", "2001:db8::1"})
	if got := endpointSet(legacyLocal); !reflect.DeepEqual(got, wantLocal) {
		t.Errorf("legacy local endpoints = %v, want %v", got, wantLocal)
	}
	if got := endpointSet(sliceLocal); !reflect.DeepEqual(got, wantLocal) {
		t.Errorf("EndpointSlice local endpoints = %v, want %v", got, wantLocal)
	}

	assertEndpointFamilies(t, legacyAll, 2, 2)
	assertEndpointFamilies(t, sliceAll, 2, 2)
}

func TestEndpointSlicesLocalFilteringRequiresServingEndpoint(t *testing.T) {
	t.Parallel()

	node := "node-a"
	serving := true
	notServing := false
	provider := NewEndpointslices()
	if err := provider.LoadObject(&discoveryv1.EndpointSlice{
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints: []discoveryv1.Endpoint{
			{Addresses: []string{"10.0.0.1"}, NodeName: &node, Conditions: discoveryv1.EndpointConditions{Serving: &serving}},
			{Addresses: []string{"10.0.0.2"}, NodeName: &node, Conditions: discoveryv1.EndpointConditions{Serving: &notServing}},
		},
	}, func() {}); err != nil {
		t.Fatalf("loading EndpointSlice: %v", err)
	}

	local, err := provider.GetLocalEndpoints(node, &kubevip.Config{})
	if err != nil {
		t.Fatalf("GetLocalEndpoints() error = %v", err)
	}
	if got, want := endpointSet(local), endpointSet([]string{"10.0.0.1"}); !reflect.DeepEqual(got, want) {
		t.Errorf("local endpoints = %v, want serving endpoints %v", got, want)
	}
}

func TestResolvePortFromFakeClientObjects(t *testing.T) {
	t.Parallel()

	//nolint:staticcheck // deprecated legacy Endpoints API is the subject under test
	legacyObject := &v1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: "default"},
		//nolint:staticcheck // deprecated legacy Endpoints API is the subject under test
		Subsets: []v1.EndpointSubset{{
			Ports: []v1.EndpointPort{{Name: "web", Port: 8080}},
		}},
	}
	legacyClient := fake.NewSimpleClientset(legacyObject)
	legacyLoaded, err := legacyClient.CoreV1().Endpoints("default").Get(t.Context(), "service", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting fake legacy Endpoints: %v", err)
	}
	legacy := NewEndpoints()
	if err := legacy.LoadObject(legacyLoaded, func() {}); err != nil {
		t.Fatalf("loading fake legacy Endpoints: %v", err)
	}

	portName := "web"
	port := int32(8081)
	sliceObject := &discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Name: "service-slice", Namespace: "default"},
		AddressType: discoveryv1.AddressTypeIPv4,
		Ports:       []discoveryv1.EndpointPort{{Name: &portName, Port: &port}},
	}
	sliceClient := fake.NewSimpleClientset(sliceObject)
	sliceLoaded, err := sliceClient.DiscoveryV1().EndpointSlices("default").Get(t.Context(), "service-slice", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting fake EndpointSlice: %v", err)
	}
	slices := NewEndpointslices()
	if err := slices.LoadObject(sliceLoaded, func() {}); err != nil {
		t.Fatalf("loading fake EndpointSlice: %v", err)
	}

	namedPort := v1.ServicePort{Port: 80, TargetPort: intstr.FromString("web")}
	if got := legacy.ResolvePort(namedPort); got != 8080 {
		t.Errorf("legacy ResolvePort() = %d, want 8080", got)
	}
	if got := slices.ResolvePort(namedPort); got != 8081 {
		t.Errorf("EndpointSlice ResolvePort() = %d, want 8081", got)
	}
}

func TestResolvePortWithLookup(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		port   v1.ServicePort
		lookup func(string) int32
		want   int32
	}{
		{
			name:   "numeric target port wins",
			port:   v1.ServicePort{Port: 80, TargetPort: intstr.FromInt(8080)},
			lookup: func(string) int32 { return 9090 },
			want:   8080,
		},
		{
			name: "named target port is looked up",
			port: v1.ServicePort{Port: 80, TargetPort: intstr.FromString("web")},
			lookup: func(name string) int32 {
				if name == "web" {
					return 8081
				}
				return 0
			},
			want: 8081,
		},
		{
			name:   "missing named target falls back to service port",
			port:   v1.ServicePort{Port: 80, TargetPort: intstr.FromString("missing")},
			lookup: func(string) int32 { return 0 },
			want:   80,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ResolvePortWithLookup(tt.port, tt.lookup); got != tt.want {
				t.Errorf("ResolvePortWithLookup() = %d, want %d", got, tt.want)
			}
		})
	}
}

func endpointSet(endpoints []string) map[string]struct{} {
	result := make(map[string]struct{}, len(endpoints))
	for _, endpoint := range endpoints {
		result[endpoint] = struct{}{}
	}
	return result
}

func assertEndpointFamilies(t *testing.T, endpoints []string, wantIPv4, wantIPv6 int) {
	t.Helper()
	ipv4, ipv6 := 0, 0
	for _, endpoint := range endpoints {
		ip := net.ParseIP(endpoint)
		if ip == nil {
			t.Errorf("endpoint %q is not an IP address", endpoint)
			continue
		}
		if ip.To4() != nil {
			ipv4++
		} else {
			ipv6++
		}
	}
	if ipv4 != wantIPv4 || ipv6 != wantIPv6 {
		t.Errorf("endpoint families = IPv4 %d, IPv6 %d; want IPv4 %d, IPv6 %d", ipv4, ipv6, wantIPv4, wantIPv6)
	}
}

func TestEndpointProvidersPreferNodeNameOverHostname(t *testing.T) {
	t.Parallel()

	nodeA := "node-a"
	nodeB := "node-b"
	serving := true

	tests := []struct {
		name string
		load func(Provider) error
	}{
		{
			name: "legacy Endpoints",
			load: func(provider Provider) error {
				//nolint:staticcheck // the legacy provider is deliberately under test
				return provider.LoadObject(&v1.Endpoints{
					Subsets: []v1.EndpointSubset{{
						Addresses: []v1.EndpointAddress{{
							IP:       "10.0.0.1",
							NodeName: &nodeB,
							Hostname: nodeA,
						}},
					}},
				}, func() {})
			},
		},
		{
			name: "EndpointSlice",
			load: func(provider Provider) error {
				hostname := nodeA
				return provider.LoadObject(&discoveryv1.EndpointSlice{
					AddressType: discoveryv1.AddressTypeIPv4,
					Endpoints: []discoveryv1.Endpoint{{
						Addresses:  []string{"10.0.0.1"},
						NodeName:   &nodeB,
						Hostname:   &hostname,
						Conditions: discoveryv1.EndpointConditions{Serving: &serving},
					}},
				}, func() {})
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var provider Provider
			if tt.name == "legacy Endpoints" {
				provider = NewEndpoints()
			} else {
				provider = NewEndpointslices()
			}
			if err := tt.load(provider); err != nil {
				t.Fatalf("LoadObject() error = %v", err)
			}

			local, err := provider.GetLocalEndpoints(nodeA, &kubevip.Config{})
			if err != nil {
				t.Fatalf("GetLocalEndpoints(%q) error = %v", nodeA, err)
			}
			if len(local) != 0 {
				t.Fatalf("GetLocalEndpoints(%q) = %v, want no endpoints", nodeA, local)
			}

			local, err = provider.GetLocalEndpoints(nodeB, &kubevip.Config{})
			if err != nil {
				t.Fatalf("GetLocalEndpoints(%q) error = %v", nodeB, err)
			}
			if got, want := endpointSet(local), endpointSet([]string{"10.0.0.1"}); !reflect.DeepEqual(got, want) {
				t.Fatalf("GetLocalEndpoints(%q) = %v, want %v", nodeB, got, want)
			}
		})
	}
}
