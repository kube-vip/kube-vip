package endpoints

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/kube-vip/kube-vip/pkg/endpoints/providers"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/nftables"
	"github.com/kube-vip/kube-vip/pkg/wireguard"
	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
)

type recordingTunnelReleaser struct {
	releases []string
}

func (r *recordingTunnelReleaser) ReleaseTunnelForVIP(vip, owner string) error {
	r.releases = append(r.releases, fmt.Sprintf("%s:%s", vip, owner))
	return nil
}

type tunnelClaim struct {
	vip   string
	owner string
}

type recordingServiceTunnelManager struct {
	configured map[string]bool
	acquired   []tunnelClaim
	released   []tunnelClaim
	acquireErr map[string]error
}

func (m *recordingServiceTunnelManager) GetConfigForVIP(vip string) *wireguard.TunnelConfig {
	if !m.configured[vip] {
		return nil
	}
	return &wireguard.TunnelConfig{VIP: vip, InterfaceName: "wg-test"}
}

func (m *recordingServiceTunnelManager) HasConfigForVIP(vip string) bool {
	return m.configured[vip]
}

func (m *recordingServiceTunnelManager) AcquireTunnelForVIP(vip, owner string) error {
	m.acquired = append(m.acquired, tunnelClaim{vip: vip, owner: owner})
	return m.acquireErr[vip]
}

func (m *recordingServiceTunnelManager) ReleaseTunnelForVIP(vip, owner string) error {
	m.released = append(m.released, tunnelClaim{vip: vip, owner: owner})
	return nil
}

type staticWireguardProvider struct {
	providers.Provider
	endpoints []string
}

func (p *staticWireguardProvider) GetAllEndpoints() ([]string, error) {
	return p.endpoints, nil
}

func (p *staticWireguardProvider) GetLabel() string { return "test" }

func TestWireguardClearDoesNotDereferenceNilServiceContext(t *testing.T) {
	worker := &wireguardWorker{}
	service := &v1.Service{}

	worker.clear(context.TODO(), nil, nil, service, nil)
}

func TestReleaseWireguardServiceTunnelsUsesServiceUIDOwner(t *testing.T) {
	releaser := &recordingTunnelReleaser{}
	service := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID("service-uid")},
		Spec:       v1.ServiceSpec{LoadBalancerIP: "192.0.2.10"},
	}

	releaseWireguardServiceTunnels(releaser, service)

	if len(releaser.releases) != 1 || releaser.releases[0] != "192.0.2.10:service-uid" {
		t.Fatalf("tunnel releases = %v, want [192.0.2.10:service-uid]", releaser.releases)
	}
}

func TestAcquireWireguardServiceTunnelsUsesServiceUIDOwner(t *testing.T) {
	manager := &recordingServiceTunnelManager{configured: map[string]bool{
		"192.0.2.10":   true,
		"2001:db8::10": true,
	}}
	service := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "service",
			UID:       types.UID("service-uid"),
			Annotations: map[string]string{
				kubevip.LoadbalancerIPAnnotation: "192.0.2.10,2001:db8::10",
			},
		},
	}

	if err := AcquireWireguardServiceTunnels(manager, service); err != nil {
		t.Fatalf("AcquireWireguardServiceTunnels() error = %v", err)
	}
	want := []tunnelClaim{
		{vip: "192.0.2.10", owner: "service-uid"},
		{vip: "2001:db8::10", owner: "service-uid"},
	}
	if fmt.Sprint(manager.acquired) != fmt.Sprint(want) {
		t.Fatalf("tunnel acquisitions = %v, want %v", manager.acquired, want)
	}
	if len(manager.released) != 0 {
		t.Fatalf("successful tunnel acquisition released claims: %v", manager.released)
	}
}

func TestAcquireWireguardServiceTunnelsRollsBackPartialActivation(t *testing.T) {
	failure := errors.New("injected tunnel failure")
	manager := &recordingServiceTunnelManager{
		configured: map[string]bool{"192.0.2.10": true, "192.0.2.20": true},
		acquireErr: map[string]error{"192.0.2.20": failure},
	}
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Namespace: "default", Name: "service", UID: types.UID("service-uid"),
		Annotations: map[string]string{kubevip.LoadbalancerIPAnnotation: "192.0.2.10,192.0.2.20"},
	}}

	err := AcquireWireguardServiceTunnels(manager, service)
	if !errors.Is(err, failure) {
		t.Fatalf("AcquireWireguardServiceTunnels() error = %v, want %v", err, failure)
	}
	wantRelease := []tunnelClaim{{vip: "192.0.2.10", owner: "service-uid"}}
	if fmt.Sprint(manager.released) != fmt.Sprint(wantRelease) {
		t.Fatalf("rollback releases = %v, want %v", manager.released, wantRelease)
	}
}

func TestWireguardEndpointProcessingDoesNotAcquireServiceTunnel(t *testing.T) {
	manager := &recordingServiceTunnelManager{configured: map[string]bool{"192.0.2.10": true}}
	provider := &staticWireguardProvider{endpoints: []string{"10.0.0.10"}}
	worker := newWireguardWorker(&kubevip.Config{}, provider, manager)
	service := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "service", UID: types.UID("service-uid")},
		Spec: v1.ServiceSpec{
			LoadBalancerIP:        "192.0.2.10",
			ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeCluster,
			// An unsupported protocol avoids touching nftables while still driving
			// the endpoint-processing path that previously acquired the tunnel.
			Ports: []v1.ServicePort{{Protocol: v1.ProtocolSCTP, Port: 80}},
		},
	}

	if err := worker.processInstance(context.Background(), nil, service, nil); err != nil {
		t.Fatalf("processInstance() error = %v", err)
	}
	if len(manager.acquired) != 0 {
		t.Fatalf("endpoint processing acquired Service tunnels: %v", manager.acquired)
	}
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
	port := v1.ServicePort{Name: "web", Port: 80, Protocol: v1.ProtocolTCP, TargetPort: intstr.FromString("target")}
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
