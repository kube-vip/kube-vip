package endpoints

import (
	"reflect"
	"testing"

	"github.com/kube-vip/kube-vip/pkg/endpoints/providers"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/nftables"
	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func TestWireguardClearDoesNotDereferenceNilServiceContext(t *testing.T) {
	worker := &wireguardWorker{}
	service := &v1.Service{}

	worker.clear(nil, nil, service)
}

func TestWireguardDNATTargetsPreservePerSliceNamedPorts(t *testing.T) {
	provider := providers.NewEndpointslices()
	name := "web"
	firstPort, secondPort := int32(8080), int32(9090)
	for _, slice := range []*discoveryv1.EndpointSlice{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "slice-a"},
			Ports:      []discoveryv1.EndpointPort{{Name: &name, Port: &firstPort}},
			Endpoints:  []discoveryv1.Endpoint{{Addresses: []string{"10.0.0.1"}}},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "slice-b"},
			Ports:      []discoveryv1.EndpointPort{{Name: &name, Port: &secondPort}},
			Endpoints:  []discoveryv1.Endpoint{{Addresses: []string{"10.0.0.1"}}},
		},
	} {
		if err := provider.LoadObject(slice, func() {}); err != nil {
			t.Fatalf("LoadObject returned error: %v", err)
		}
	}

	worker := &wireguardWorker{config: &kubevip.Config{}, provider: provider}
	port := v1.ServicePort{Port: 80, Protocol: v1.ProtocolTCP, TargetPort: intstr.FromString(name)}
	got, err := worker.dnatTargets(&v1.Service{}, port)
	if err != nil {
		t.Fatalf("dnatTargets returned error: %v", err)
	}
	want := []nftables.DNATTarget{{IP: "10.0.0.1", Port: 8080}, {IP: "10.0.0.1", Port: 9090}}
	if !sameDNATTargets(got, want) {
		t.Fatalf("dnatTargets = %v, want %v", got, want)
	}
}

func sameDNATTargets(got, want []nftables.DNATTarget) bool {
	if len(got) != len(want) {
		return false
	}
	gotSet := make(map[nftables.DNATTarget]struct{}, len(got))
	for _, target := range got {
		gotSet[target] = struct{}{}
	}
	wantSet := make(map[nftables.DNATTarget]struct{}, len(want))
	for _, target := range want {
		wantSet[target] = struct{}{}
	}
	return reflect.DeepEqual(gotSet, wantSet)
}
