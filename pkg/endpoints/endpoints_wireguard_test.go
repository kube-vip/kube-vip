package endpoints

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kube-vip/kube-vip/pkg/endpoints/providers"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/nftables"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	"github.com/kube-vip/kube-vip/pkg/wireguard"
	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func TestWireguardClearDoesNotDereferenceNilServiceContext(t *testing.T) {
	worker := &wireguardWorker{}
	service := &v1.Service{}

	worker.clear(nil, nil, service, nil)
}

func TestWireguardProcessInstanceClearsAllApplyFailuresWithoutCancelingLeader(t *testing.T) {
	provider := newWireguardTestProvider(t)
	service := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: "default"},
		Spec: v1.ServiceSpec{LoadBalancerIP: "192.0.2.1", Ports: []v1.ServicePort{
			{Port: 80, Protocol: v1.ProtocolTCP, TargetPort: intstr.FromInt32(8080)},
			{Port: 443, Protocol: v1.ProtocolTCP, TargetPort: intstr.FromInt32(8443)},
		}},
	}
	tunnelMgr := wireguard.NewTunnelManager()
	loadWireguardTestConfig(t, tunnelMgr)
	var calls int
	var deleted []string
	svcCtx := servicecontext.New(context.Background())
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	defer cancelLeader()
	svcCtx.SetLeaderCancel(cancelLeader)
	worker := &wireguardWorker{
		config: &kubevip.Config{}, provider: provider, tunnelMgr: tunnelMgr,
		applyDNAT: func(string, string, uint16, []nftables.DNATTarget, string, v1.Protocol, bool, int) error {
			calls++
			return errors.New("apply failed")
		},
		deleteDNATRule: func(wgInterface string, ipv6 bool, serviceID string) error {
			if wgInterface != "tunnel0" {
				t.Fatalf("WireGuard interface = %q, want tunnel0", wgInterface)
			}
			if ipv6 {
				t.Fatal("deleted IPv6 chain for IPv4 service")
			}
			deleted = append(deleted, serviceID)
			return nil
		},
	}
	if err := worker.processInstance(svcCtx, service, nil); err != nil {
		t.Fatalf("processInstance error = %v, want nil", err)
	}
	if calls != 2 {
		t.Fatalf("ApplyDNAT calls = %d, want 2", calls)
	}
	wantDeleted := []string{
		"default_service_p80", "default_service_p80_tcp",
		"default_service_p443", "default_service_p443_tcp",
	}
	if strings.Join(deleted, ",") != strings.Join(wantDeleted, ",") {
		t.Fatalf("deleted DNAT chains = %v, want %v", deleted, wantDeleted)
	}
	select {
	case <-leaderCtx.Done():
		t.Fatal("apply failures canceled leader")
	default:
	}
}

func loadWireguardTestConfig(t *testing.T, tunnelMgr *wireguard.TunnelManager) {
	t.Helper()
	secret := &v1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Name: "wireguard", Namespace: "default"},
		Data: map[string][]byte{"tunnels": []byte(`tunnel0:
  vip: 192.0.2.1/32
  privateKey: private
  peerPublicKey: public
  peerEndpoint: 198.51.100.1:51820
  allowedIPs: [192.0.2.1/32]
  listenPort: 51820
`)},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(secret); err != nil {
			t.Errorf("encode Secret: %v", err)
		}
	}))
	t.Cleanup(server.Close)
	client, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatalf("create Kubernetes client: %v", err)
	}
	if err := tunnelMgr.LoadConfigurationsFromSecret(context.Background(), client, "default", "wireguard"); err != nil {
		t.Fatalf("load tunnel configuration: %v", err)
	}
}

func TestWireguardApplyFailureDeletesOnlyFailedTunnelRule(t *testing.T) {
	provider := newWireguardTestProvider(t)
	service := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: "default", Annotations: map[string]string{
			kubevip.LoadbalancerIPAnnotation: "192.0.2.1,192.0.2.2",
		}},
		Spec: v1.ServiceSpec{Ports: []v1.ServicePort{{Port: 80, Protocol: v1.ProtocolTCP, TargetPort: intstr.FromInt32(8080)}}},
	}
	tunnelMgr := wireguard.NewTunnelManager()
	loadWireguardTunnelConfigs(t, tunnelMgr, `tunnel0:
  vip: 192.0.2.1/32
  privateKey: private
  peerPublicKey: public
  peerEndpoint: 198.51.100.1:51820
  allowedIPs: [192.0.2.1/32]
  listenPort: 51820
tunnel1:
  vip: 192.0.2.2/32
  privateKey: private
  peerPublicKey: public
  peerEndpoint: 198.51.100.2:51820
  allowedIPs: [192.0.2.2/32]
  listenPort: 51821
`)
	var applied, deleted []string
	worker := &wireguardWorker{
		config: &kubevip.Config{}, provider: provider, tunnelMgr: tunnelMgr,
		applyDNAT: func(wgInterface, _ string, _ uint16, _ []nftables.DNATTarget, _ string, _ v1.Protocol, _ bool, _ int) error {
			applied = append(applied, wgInterface)
			if wgInterface == "tunnel1" {
				return errors.New("apply failed")
			}
			return nil
		},
		deleteDNATRule: func(wgInterface string, ipv6 bool, serviceID string) error {
			if ipv6 {
				t.Fatal("deleted IPv6 rule for IPv4 tunnel")
			}
			deleted = append(deleted, wgInterface+"/"+serviceID)
			return nil
		},
		deleteDNAT: func(bool, string) error {
			t.Fatal("apply failure deleted family-wide ingress chains")
			return nil
		},
	}

	if err := worker.processInstance(servicecontext.New(context.Background()), service, nil); err != nil {
		t.Fatalf("processInstance error = %v, want nil", err)
	}
	if strings.Join(applied, ",") != "tunnel0,tunnel1" {
		t.Fatalf("applied tunnels = %v, want [tunnel0 tunnel1]", applied)
	}
	wantDeleted := []string{
		"tunnel0/default_service_p80",
		"tunnel1/default_service_p80",
		"tunnel1/default_service_p80_tcp",
	}
	if strings.Join(deleted, ",") != strings.Join(wantDeleted, ",") {
		t.Fatalf("deleted rules = %v, want %v", deleted, wantDeleted)
	}
}

func loadWireguardTunnelConfigs(t *testing.T, tunnelMgr *wireguard.TunnelManager, tunnels string) {
	t.Helper()
	secret := &v1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Name: "wireguard", Namespace: "default"},
		Data:       map[string][]byte{"tunnels": []byte(tunnels)},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(secret); err != nil {
			t.Errorf("encode Secret: %v", err)
		}
	}))
	t.Cleanup(server.Close)
	client, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatalf("create Kubernetes client: %v", err)
	}
	if err := tunnelMgr.LoadConfigurationsFromSecret(context.Background(), client, "default", "wireguard"); err != nil {
		t.Fatalf("load tunnel configuration: %v", err)
	}
}

func TestWireguardProcessInstanceWithEndpointsDoesNotCancelLeader(t *testing.T) {
	provider := newWireguardTestProvider(t)

	svcCtx := servicecontext.New(context.Background())
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	defer cancelLeader()
	svcCtx.SetLeaderCancel(cancelLeader)
	service := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: "default"},
		Spec: v1.ServiceSpec{
			LoadBalancerIP: "192.0.2.1",
			Ports:          []v1.ServicePort{{Port: 80, Protocol: v1.ProtocolTCP, TargetPort: intstr.FromInt32(8080)}},
		},
	}

	tunnelMgr := wireguard.NewTunnelManager()
	secret := &v1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Name: "wireguard", Namespace: "default"},
		Data: map[string][]byte{"tunnels": []byte(`tunnel0:
  vip: 192.0.2.1/32
  privateKey: private
  peerPublicKey: public
  peerEndpoint: 198.51.100.1:51820
  allowedIPs: [192.0.2.1/32]
  listenPort: 51820
`)},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(secret); err != nil {
			t.Errorf("encode Secret: %v", err)
		}
	}))
	defer server.Close()
	client, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatalf("create Kubernetes client: %v", err)
	}
	if err := tunnelMgr.LoadConfigurationsFromSecret(context.Background(), client, "default", "wireguard"); err != nil {
		t.Fatalf("load tunnel configuration: %v", err)
	}
	var applied []nftables.DNATTarget
	worker := &wireguardWorker{
		config:    &kubevip.Config{},
		provider:  provider,
		tunnelMgr: tunnelMgr,
		applyDNAT: func(_ string, _ string, _ uint16, targets []nftables.DNATTarget, _ string, _ v1.Protocol, _ bool, _ int) error {
			applied = append(applied, targets...)
			return nil
		},
		deleteDNATRule: func(string, bool, string) error { return nil },
	}
	if err := worker.processInstance(svcCtx, service, nil); err != nil {
		t.Fatalf("processInstance returned error: %v", err)
	}

	select {
	case <-leaderCtx.Done():
		t.Fatal("processInstance canceled leader with non-empty endpoints")
	default:
	}
	if len(applied) != 1 || applied[0].IP != "10.0.0.2" || applied[0].Port != 8080 {
		t.Fatalf("applied targets = %+v, want refreshed endpoint", applied)
	}
}

func TestWireguardMissingTunnelDeletesStaleDNATWithoutCancelingLeader(t *testing.T) {
	provider := newWireguardTestProvider(t)

	svcCtx := servicecontext.New(context.Background())
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	defer cancelLeader()
	svcCtx.SetLeaderCancel(cancelLeader)
	service := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: "default"},
		Spec: v1.ServiceSpec{
			LoadBalancerIP: "192.0.2.1",
			Ports:          []v1.ServicePort{{Port: 80, Protocol: v1.ProtocolTCP, TargetPort: intstr.FromInt32(8080)}},
		},
	}
	var deleted []string
	worker := &wireguardWorker{
		config:    &kubevip.Config{},
		provider:  provider,
		tunnelMgr: wireguard.NewTunnelManager(),
		applyDNAT: func(string, string, uint16, []nftables.DNATTarget, string, v1.Protocol, bool, int) error {
			t.Fatal("ApplyDNAT called before complete tunnel validation")
			return nil
		},
		deleteDNAT: func(ipv6 bool, serviceID string) error {
			if ipv6 {
				t.Fatal("deleted IPv6 chain for IPv4 service")
			}
			deleted = append(deleted, serviceID)
			return nil
		},
	}

	if err := worker.processInstance(svcCtx, service, nil); err != nil {
		t.Fatalf("processInstance error = %v, want nil", err)
	}
	wantDeleted := []string{"default_service_p80_tcp", "default_service_p80"}
	if strings.Join(deleted, ",") != strings.Join(wantDeleted, ",") {
		t.Fatalf("deleted DNAT chains = %v, want %v", deleted, wantDeleted)
	}
	select {
	case <-leaderCtx.Done():
		t.Fatal("missing tunnel validation canceled leader")
	default:
	}
}

func TestWireguardMissingServiceIPClearsBothFamilies(t *testing.T) {
	provider := newWireguardTestProvider(t)
	service := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: "default"},
		Spec:       v1.ServiceSpec{Ports: []v1.ServicePort{{Port: 80, Protocol: v1.ProtocolTCP}}},
	}
	var deleted []string
	worker := &wireguardWorker{
		config: &kubevip.Config{}, provider: provider,
		deleteDNAT: func(ipv6 bool, serviceID string) error {
			deleted = append(deleted, fmt.Sprintf("%t/%s", ipv6, serviceID))
			return nil
		},
	}

	if err := worker.processInstance(servicecontext.New(context.Background()), service, nil); err != nil {
		t.Fatalf("processInstance error = %v, want nil", err)
	}
	wantDeleted := []string{
		"false/default_service_p80_tcp", "true/default_service_p80_tcp",
		"false/default_service_p80", "true/default_service_p80",
	}
	if strings.Join(deleted, ",") != strings.Join(wantDeleted, ",") {
		t.Fatalf("deleted rules = %v, want %v", deleted, wantDeleted)
	}
}

func TestWireguardProcessInstanceKeepsEndpointFamiliesSeparate(t *testing.T) {
	provider := newWireguardTestProvider(t)
	if err := provider.LoadObject(&discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name: "service-2", Namespace: "default",
			Labels: map[string]string{discoveryv1.LabelServiceName: "service"},
		},
		AddressType: discoveryv1.AddressTypeIPv6,
		Endpoints:   []discoveryv1.Endpoint{{Addresses: []string{"2001:db8::2"}}},
	}, func() {}); err != nil {
		t.Fatalf("LoadObject returned error: %v", err)
	}
	service := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: "default", Annotations: map[string]string{
			kubevip.LoadbalancerIPAnnotation: "192.0.2.1,2001:db8::1",
		}},
		Spec: v1.ServiceSpec{Ports: []v1.ServicePort{{Port: 80, Protocol: v1.ProtocolTCP}}},
	}
	tunnelMgr := wireguard.NewTunnelManager()
	loadWireguardTunnelConfigs(t, tunnelMgr, `tunnel0:
  vip: 192.0.2.1/32
  privateKey: private
  peerPublicKey: public
  peerEndpoint: 198.51.100.1:51820
  allowedIPs: [192.0.2.1/32]
  listenPort: 51820
tunnel1:
  vip: 2001:db8::1/128
  privateKey: private
  peerPublicKey: public
  peerEndpoint: '[2001:db8::10]:51820'
  allowedIPs: ['2001:db8::1/128']
  listenPort: 51821
`)
	applied := make(map[string][]nftables.DNATTarget)
	worker := &wireguardWorker{
		config: &kubevip.Config{}, provider: provider, tunnelMgr: tunnelMgr,
		applyDNAT: func(_ string, vip string, _ uint16, targets []nftables.DNATTarget, _ string, _ v1.Protocol, _ bool, _ int) error {
			applied[vip] = targets
			return nil
		},
		deleteDNATRule: func(string, bool, string) error { return nil },
	}

	if err := worker.processInstance(servicecontext.New(context.Background()), service, nil); err != nil {
		t.Fatalf("processInstance error = %v", err)
	}
	if targets := applied["192.0.2.1"]; len(targets) != 1 || targets[0].IP != "10.0.0.2" {
		t.Fatalf("IPv4 targets = %v, want 10.0.0.2", targets)
	}
	if targets := applied["2001:db8::1"]; len(targets) != 1 || targets[0].IP != "2001:db8::2" {
		t.Fatalf("IPv6 targets = %v, want 2001:db8::2", targets)
	}
}

func TestWireguardClearIncludesSCTP(t *testing.T) {
	service := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: "default"},
		Spec: v1.ServiceSpec{
			LoadBalancerIP: "192.0.2.1",
			Ports:          []v1.ServicePort{{Port: 80, Protocol: v1.ProtocolSCTP}},
		},
	}
	var deleted []string
	worker := &wireguardWorker{deleteDNAT: func(ipv6 bool, serviceID string) error {
		if ipv6 {
			t.Fatal("cleared IPv6 rule for IPv4 service")
		}
		deleted = append(deleted, serviceID)
		return nil
	}}

	worker.clear(nil, nil, service, nil)
	wantDeleted := []string{"default_service_p80_sctp", "default_service_p80"}
	if strings.Join(deleted, ",") != strings.Join(wantDeleted, ",") {
		t.Fatalf("deleted service IDs = %v, want %v", deleted, wantDeleted)
	}
}

func TestWireguardProcessInstanceSeparatesSamePortProtocols(t *testing.T) {
	provider := newWireguardTestProvider(t)
	service := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: "default"},
		Spec: v1.ServiceSpec{
			LoadBalancerIP: "192.0.2.1",
			Ports: []v1.ServicePort{
				{Port: 53, Protocol: v1.ProtocolTCP, TargetPort: intstr.FromInt32(8053)},
				{Port: 53, Protocol: v1.ProtocolUDP, TargetPort: intstr.FromInt32(8053)},
			},
		},
	}
	tunnelMgr := wireguard.NewTunnelManager()
	loadWireguardTestConfig(t, tunnelMgr)
	var applied, migrated []string
	worker := &wireguardWorker{
		config: &kubevip.Config{}, provider: provider, tunnelMgr: tunnelMgr,
		applyDNAT: func(_ string, _ string, _ uint16, _ []nftables.DNATTarget, serviceID string, _ v1.Protocol, _ bool, _ int) error {
			applied = append(applied, serviceID)
			return nil
		},
		deleteDNATRule: func(_ string, _ bool, serviceID string) error {
			migrated = append(migrated, serviceID)
			return nil
		},
	}

	if err := worker.processInstance(servicecontext.New(context.Background()), service, nil); err != nil {
		t.Fatalf("processInstance error = %v", err)
	}
	wantApplied := []string{"default_service_p53_tcp", "default_service_p53_udp"}
	if strings.Join(applied, ",") != strings.Join(wantApplied, ",") {
		t.Fatalf("applied service IDs = %v, want %v", applied, wantApplied)
	}
	if strings.Join(migrated, ",") != "default_service_p53,default_service_p53" {
		t.Fatalf("migrated service IDs = %v, want legacy ID for each protocol", migrated)
	}
}

func newWireguardTestProvider(t *testing.T) providers.Provider {
	t.Helper()
	provider := providers.NewEndpointslices()
	if err := provider.LoadObject(&discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name: "service-1", Namespace: "default",
			Labels: map[string]string{discoveryv1.LabelServiceName: "service"},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints:   []discoveryv1.Endpoint{{Addresses: []string{"10.0.0.2"}}},
	}, func() {}); err != nil {
		t.Fatalf("LoadObject returned error: %v", err)
	}
	return provider
}
