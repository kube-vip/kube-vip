package endpoints

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/kube-vip/kube-vip/pkg/endpoints/providers"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/wireguard"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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
