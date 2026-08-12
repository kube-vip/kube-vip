package cluster_test

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kube-vip/kube-vip/pkg/arp"
	"github.com/kube-vip/kube-vip/pkg/bgp"
	"github.com/kube-vip/kube-vip/pkg/cluster"
	"github.com/kube-vip/kube-vip/pkg/election"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/route"
	"github.com/kube-vip/kube-vip/pkg/vip"
	"github.com/vishvananda/netlink"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const testCIDR = "10.0.0.34/32"

func TestBGPHealthCheckLoop_AnnouncesOnHealthy(t *testing.T) {
	t.Parallel()
	healthcheck := newTestHealthServer(t, http.StatusOK)
	t.Cleanup(healthcheck.server.Close)

	bgpManager := newMockBGPRouteManager()
	startVipService(t, newBGPConfig(healthcheck.server.URL, healthcheck.caPath), bgpManager)

	expectEventually(t, func() bool { return bgpManager.isAnnounced() },
		"route should be announced")
}

func TestServicesWorkerStopAndWaitDrainsBeforeRestart(t *testing.T) {
	config := &kubevip.Config{}
	serviceCluster, err := cluster.InitCluster(config, true, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("InitCluster() error = %v", err)
	}
	var workers sync.WaitGroup
	if err := serviceCluster.StartLoadBalancerService(context.Background(), config, nil, "service", &workers); err != nil {
		t.Fatalf("first StartLoadBalancerService() error = %v", err)
	}
	if err := serviceCluster.StartLoadBalancerService(context.Background(), config, nil, "service", &workers); err == nil {
		t.Fatal("second StartLoadBalancerService() started while the first workers were active")
	}

	serviceCluster.StopAndWait()
	if err := serviceCluster.StartLoadBalancerService(context.Background(), config, nil, "service", &workers); err != nil {
		t.Fatalf("StartLoadBalancerService() after StopAndWait error = %v", err)
	}
	serviceCluster.StopAndWait()
	workers.Wait()
}

func TestServicesWorkerStopAndWaitPreservingDrainsBeforeRestart(t *testing.T) {
	config := &kubevip.Config{}
	serviceCluster, err := cluster.InitCluster(config, true, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("InitCluster() error = %v", err)
	}
	var workers sync.WaitGroup
	if err := serviceCluster.StartLoadBalancerService(context.Background(), config, nil, "service", &workers); err != nil {
		t.Fatalf("StartLoadBalancerService() error = %v", err)
	}
	serviceCluster.StopAndWait()
	if err := serviceCluster.StartLoadBalancerService(context.Background(), config, nil, "service", &workers); err != nil {
		t.Fatalf("StartLoadBalancerService() after preserving stop error = %v", err)
	}
	serviceCluster.StopAndWait()
	workers.Wait()
}

func TestServicesWorkerStopRemovesVIPUnlessPreserved(t *testing.T) {
	tests := []struct {
		name     string
		arp      bool
		preserve bool
	}{
		{name: "stop removes the VIP"},
		{name: "stop with ARP removes the VIP", arp: true},
		{name: "preserving stop keeps the VIP", preserve: true},
		{name: "preserving stop with ARP keeps the VIP", arp: true, preserve: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := &kubevip.Config{EnableARP: test.arp, ArpBroadcastRate: 3000, VIPSubnet: "32"}
			network := &mockNetwork{ip: "192.0.2.10", cidr: "192.0.2.10/32"}
			var arpMgr *arp.Manager
			if test.arp {
				arpMgr = arp.NewManager(config)
				if test.preserve {
					// A preserved VIP is shared, so the sibling Service still holds an ARP claim.
					arpMgr.Insert(arp.NewInstance(network, nil))
				}
			}
			serviceCluster, err := cluster.InitCluster(config, true, nil, arpMgr, nil, nil)
			if err != nil {
				t.Fatalf("InitCluster() error = %v", err)
			}
			serviceCluster.Network = []vip.Network{network}

			var workers sync.WaitGroup
			if err := serviceCluster.StartLoadBalancerService(context.Background(), config, nil, "service", &workers); err != nil {
				t.Fatalf("StartLoadBalancerService() error = %v", err)
			}
			if !network.isPresent() {
				t.Fatal("VIP was not added when the Service worker started")
			}

			if test.preserve {
				serviceCluster.StopAndWaitPreserving(network.IP())
			} else {
				serviceCluster.StopAndWait()
			}
			workers.Wait()

			if got := network.isPresent(); got != test.preserve {
				t.Fatalf("VIP present after stop = %t, want %t", got, test.preserve)
			}
		})
	}
}

func TestStartLoadBalancerServiceRollsBackEarlierNetwork(t *testing.T) {
	first := &mockNetwork{ip: "192.0.2.10", cidr: "192.0.2.10/32"}
	second := &mockNetwork{ip: "192.0.2.11", cidr: "192.0.2.11/32", setMaskErr: errors.New("set mask")}
	serviceCluster, err := cluster.InitCluster(&kubevip.Config{}, true, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("InitCluster() error = %v", err)
	}
	serviceCluster.Network = []vip.Network{first, second}

	if err := serviceCluster.StartLoadBalancerService(context.Background(), &kubevip.Config{VIPSubnet: "32"}, nil, "service", &sync.WaitGroup{}); err == nil {
		t.Fatal("StartLoadBalancerService() error = nil, want second-network failure")
	}
	first.mu.Lock()
	addCalls, deleteCalls, present := first.addIPCalls, first.deleteIPCalls, first.present
	first.mu.Unlock()
	if addCalls != 1 || deleteCalls != 1 || present {
		t.Fatalf("first network rollback = add %d, delete %d, present %t; want 1, 1, false", addCalls, deleteCalls, present)
	}
	serviceCluster.StopAndWait()
}

func TestStartLoadBalancerServiceRollbackPreservesExistingVIP(t *testing.T) {
	first := &mockNetwork{ip: "192.0.2.10", cidr: "192.0.2.10/32", present: true}
	second := &mockNetwork{ip: "192.0.2.11", cidr: "192.0.2.11/32", setMaskErr: errors.New("set mask")}
	serviceCluster, err := cluster.InitCluster(&kubevip.Config{}, true, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("InitCluster() error = %v", err)
	}
	serviceCluster.Network = []vip.Network{first, second}

	if err := serviceCluster.StartLoadBalancerService(context.Background(), &kubevip.Config{VIPSubnet: "32"}, nil, "service", &sync.WaitGroup{}); err == nil {
		t.Fatal("StartLoadBalancerService() error = nil, want second-network failure")
	}
	first.mu.Lock()
	addCalls, deleteCalls, present := first.addIPCalls, first.deleteIPCalls, first.present
	first.mu.Unlock()
	if addCalls != 1 || deleteCalls != 0 || !present {
		t.Fatalf("existing VIP rollback = add %d, delete %d, present %t; want 1, 0, true", addCalls, deleteCalls, present)
	}
	serviceCluster.StopAndWait()
}

func TestStartLoadBalancerServiceCancelledContextDoesNotConfigureVIP(t *testing.T) {
	network := &mockNetwork{ip: "192.0.2.10", cidr: "192.0.2.10/32"}
	serviceCluster, err := cluster.InitCluster(&kubevip.Config{}, true, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("InitCluster() error = %v", err)
	}
	serviceCluster.Network = []vip.Network{network}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err = serviceCluster.StartLoadBalancerService(ctx, &kubevip.Config{VIPSubnet: "32"}, nil, "service", &sync.WaitGroup{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("StartLoadBalancerService() error = %v, want context.Canceled", err)
	}
	network.mu.Lock()
	addCalls := network.addIPCalls
	network.mu.Unlock()
	if addCalls != 0 {
		t.Fatalf("AddIP calls = %d, want 0 after context cancellation", addCalls)
	}
}

func TestBGPHealthCheckLoop_NoAnnouncementUntilHealthy(t *testing.T) {
	t.Parallel()
	healthcheck := newTestHealthServer(t, http.StatusInternalServerError)
	t.Cleanup(healthcheck.server.Close)

	bgpManager := newMockBGPRouteManager()
	startVipService(t, newBGPConfig(healthcheck.server.URL, healthcheck.caPath), bgpManager)

	expectConsistently(t, func() bool { return !bgpManager.isAnnounced() },
		2*time.Second, "route should not be announced while unhealthy")

	healthcheck.setStatus(http.StatusOK)
	expectEventually(t, func() bool { return bgpManager.isAnnounced() },
		"route should be announced after recovery")
}

func TestBGPHealthCheckLoop_WithdrawsAfterThreshold(t *testing.T) {
	t.Parallel()
	healthcheck := newTestHealthServer(t, http.StatusOK)
	t.Cleanup(healthcheck.server.Close)

	bgpManager := newMockBGPRouteManager()
	cfg := newBGPConfig(healthcheck.server.URL, healthcheck.caPath)
	cfg.ControlPlaneHealthCheck.FailureThreshold = 3
	startVipService(t, cfg, bgpManager)

	expectEventually(t, func() bool { return bgpManager.isAnnounced() },
		"route should be announced")

	healthcheck.setStatus(http.StatusServiceUnavailable)

	expectConsistently(t, func() bool { return bgpManager.isAnnounced() },
		1500*time.Millisecond, "route should stay announced before threshold is reached")

	expectEventually(t, func() bool { return !bgpManager.isAnnounced() },
		"route should be withdrawn after threshold")
}

func TestBGPHealthCheckLoop_ReAnnouncesOnRecovery(t *testing.T) {
	t.Parallel()
	healthcheck := newTestHealthServer(t, http.StatusOK)
	t.Cleanup(healthcheck.server.Close)

	bgpManager := newMockBGPRouteManager()
	cfg := newBGPConfig(healthcheck.server.URL, healthcheck.caPath)
	cfg.ControlPlaneHealthCheck.FailureThreshold = 1
	startVipService(t, cfg, bgpManager)

	expectEventually(t, func() bool { return bgpManager.isAnnounced() },
		"route should be announced")

	healthcheck.setStatus(http.StatusServiceUnavailable)
	expectEventually(t, func() bool { return !bgpManager.isAnnounced() },
		"route should be withdrawn")

	healthcheck.setStatus(http.StatusOK)
	expectEventually(t, func() bool { return bgpManager.isAnnounced() },
		"route should be re-announced")
}

func TestBGPHealthCheckLoop_StopsOnContextCancel(t *testing.T) {
	t.Parallel()
	healthcheck := newTestHealthServer(t, http.StatusOK)
	t.Cleanup(healthcheck.server.Close)

	bgpManager := newMockBGPRouteManager()
	cancelContext, vipServiceDone := startVipService(t, newBGPConfig(healthcheck.server.URL, healthcheck.caPath), bgpManager)

	expectEventually(t, func() bool { return bgpManager.isAnnounced() },
		"route should be announced")

	cancelContext()

	select {
	case <-vipServiceDone:
	case <-time.After(5 * time.Second):
		t.Fatal("vipService did not stop after context cancellation")
	}
}

func TestBGPHealthCheckLoop_RetriesAddHostOnFailure(t *testing.T) {
	t.Parallel()
	healthcheck := newTestHealthServer(t, http.StatusOK)
	t.Cleanup(healthcheck.server.Close)

	bgpManager := newMockBGPRouteManager()
	bgpManager.setAddErr(errTestAddHost)
	startVipService(t, newBGPConfig(healthcheck.server.URL, healthcheck.caPath), bgpManager)

	expectConsistently(t, func() bool { return !bgpManager.isAnnounced() },
		2*time.Second, "route should not be announced while AddHost errors")

	bgpManager.setAddErr(nil)
	expectEventually(t, func() bool { return bgpManager.isAnnounced() },
		"route should be announced after clearing AddHost error")
}

func TestBGPHealthCheckLoop_RetriesDelHostOnFailure(t *testing.T) {
	t.Parallel()
	healthcheck := newTestHealthServer(t, http.StatusOK)
	t.Cleanup(healthcheck.server.Close)

	bgpManager := newMockBGPRouteManager()
	cfg := newBGPConfig(healthcheck.server.URL, healthcheck.caPath)
	cfg.ControlPlaneHealthCheck.FailureThreshold = 1
	startVipService(t, cfg, bgpManager)

	expectEventually(t, func() bool { return bgpManager.isAnnounced() },
		"route should be announced")

	bgpManager.setDelErr(errTestDelHost)
	healthcheck.setStatus(http.StatusServiceUnavailable)

	expectConsistently(t, func() bool { return bgpManager.isAnnounced() },
		1500*time.Millisecond, "route should stay announced while DelHost errors")

	bgpManager.setDelErr(nil)
	expectEventually(t, func() bool { return !bgpManager.isAnnounced() },
		"route should be withdrawn after clearing DelHost error")
}

func TestRoutingTableHealthCheck_AddsVIPWhenHealthy(t *testing.T) {
	t.Parallel()
	healthcheck := newTestHealthServer(t, http.StatusOK)
	t.Cleanup(healthcheck.server.Close)

	network := &mockNetwork{ip: "10.0.0.1", cidr: testCIDR}
	startRoutingTableVipService(t, newRoutingTableConfig(healthcheck.server.URL, healthcheck.caPath), network)

	expectEventually(t, network.isPresent,
		"VIP should be added while health check is healthy")
}

func TestRoutingTableHealthCheck_RemovesVIPWhenUnhealthy(t *testing.T) {
	t.Parallel()
	healthcheck := newTestHealthServer(t, http.StatusOK)
	t.Cleanup(healthcheck.server.Close)

	network := &mockNetwork{ip: "10.0.0.1", cidr: testCIDR}
	startRoutingTableVipService(t, newRoutingTableConfig(healthcheck.server.URL, healthcheck.caPath), network)

	expectEventually(t, network.isPresent,
		"VIP should be added while health check is healthy")

	healthcheck.setStatus(http.StatusServiceUnavailable)
	expectEventually(t, func() bool { return !network.isPresent() },
		"VIP should be removed once health check becomes unhealthy")
}

func TestRoutingTableHealthCheck_AddsIPv6VIPWithIPv4HealthCheckURL(t *testing.T) {
	t.Parallel()
	healthcheck := newTestHealthServer(t, http.StatusOK)
	t.Cleanup(healthcheck.server.Close)

	// The health-check URL is IPv4 (127.0.0.1) while the VIP is IPv6; guards against the IPv6 VIP being skipped.
	network := &mockNetwork{ip: "fd00::1", cidr: "fd00::1/128"}
	startRoutingTableVipService(t, newRoutingTableConfig(healthcheck.server.URL, healthcheck.caPath), network)

	expectEventually(t, network.isPresent,
		"IPv6 VIP should be added when the IPv4 health-check URL is healthy")
}

func TestRoutingTableHealthCheck_HTTPTakesPrecedenceOverKubernetesAddr(t *testing.T) {
	t.Parallel()
	healthcheck := newTestHealthServer(t, http.StatusOK)
	t.Cleanup(healthcheck.server.Close)

	// When both are set, the HTTP check must drive the VIP; KubernetesAddr points at a closed port and would never report healthy.
	cfg := newRoutingTableConfig(healthcheck.server.URL, healthcheck.caPath)
	cfg.KubernetesAddr = "127.0.0.1:1"

	network := &mockNetwork{ip: "10.0.0.1", cidr: testCIDR}
	startRoutingTableVipService(t, cfg, network)

	expectEventually(t, network.isPresent,
		"VIP should be added because the HTTP health check is healthy, even though KubernetesAddr is set")

	healthcheck.setStatus(http.StatusServiceUnavailable)
	expectEventually(t, func() bool { return !network.isPresent() },
		"VIP should be removed once the HTTP health check becomes unhealthy")
}

func TestRoutingTableInvalidHTTPHealthCheckAddressKillsWithoutFallback(t *testing.T) {
	t.Parallel()
	// An invalid ControlPlaneHealthCheck.Address must call killFunc and return an error instead of falling back to KubernetesAddr.
	cfg := &kubevip.Config{
		EnableRoutingTable:         true,
		BackendHealthCheckInterval: 1,
		KubernetesAddr:             "127.0.0.1:6443",
		ControlPlaneHealthCheck:    kubevip.HealthCheck{Address: "://bad"},
	}
	c, err := cluster.InitCluster(cfg, true, nil, nil, route.NewManager(), nil)
	if err != nil {
		t.Fatalf("InitCluster: %v", err)
	}
	c.Network = []vip.Network{&mockNetwork{ip: "10.0.0.1", cidr: testCIDR}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var killCalls atomic.Int32
	killFunc := func() {
		killCalls.Add(1)
		cancel()
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- c.StartVipService(ctx, cfg, nil, nil, killFunc)
	}()

	select {
	case err := <-errCh:
		if err == nil || !strings.Contains(err.Error(), "failed to create HTTP backend") {
			t.Fatalf("StartVipService() error = %v, want \"failed to create HTTP backend\"", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("StartVipService did not return after an invalid HTTP health-check address")
	}

	if killCalls.Load() == 0 {
		t.Fatal("killFunc was not called for an invalid HTTP health-check address")
	}
}

func TestRoutingTableInvalidKubernetesAddrFallsBack(t *testing.T) {
	t.Parallel()
	// An unparsable KubernetesAddr must be logged and fall back to node/localhost addresses instead of returning an error.
	cfg := &kubevip.Config{EnableRoutingTable: true, BackendHealthCheckInterval: 1, KubernetesAddr: "://not-a-url"}
	c, err := cluster.InitCluster(cfg, true, nil, nil, route.NewManager(), nil)
	if err != nil {
		t.Fatalf("InitCluster: %v", err)
	}
	c.Network = []vip.Network{&mockNetwork{ip: "10.0.0.1", cidr: testCIDR}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- c.StartVipService(ctx, cfg, nil, nil, func() {})
	}()

	select {
	case err := <-errCh:
		t.Fatalf("StartVipService returned early: %v", err)
	case <-time.After(2 * time.Second):
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("StartVipService() error = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("StartVipService did not stop after context cancellation")
	}
}

func TestRoutingTableInvalidNodeAddressFallsBackToLocalhost(t *testing.T) {
	t.Parallel()
	// A node InternalIP that backend.New rejects (zoned IPv6 "fe80::1%eth0") must be logged and
	// skipped, not treated as fatal. When no node address is usable, StartVipService falls back to
	// a localhost backend and keeps running: it must not return or call killFunc until ctx is cancelled.
	const nodeName = "test-node"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/nodes/"+nodeName {
			http.NotFound(w, r)
			return
		}
		node := corev1.Node{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Node"},
			ObjectMeta: metav1.ObjectMeta{Name: nodeName},
			Status: corev1.NodeStatus{
				Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "fe80::1%eth0"}},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(node)
	}))
	t.Cleanup(server.Close)

	client, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatalf("kubernetes.NewForConfig: %v", err)
	}
	em := &election.Manager{KubernetesClient: client}

	cfg := &kubevip.Config{EnableRoutingTable: true, BackendHealthCheckInterval: 1, NodeName: nodeName}
	c, err := cluster.InitCluster(cfg, true, nil, nil, route.NewManager(), nil)
	if err != nil {
		t.Fatalf("InitCluster: %v", err)
	}
	c.Network = []vip.Network{&mockNetwork{ip: "10.0.0.1", cidr: testCIDR}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var killCalls atomic.Int32
	killFunc := func() {
		killCalls.Add(1)
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- c.StartVipService(ctx, cfg, em, nil, killFunc)
	}()

	select {
	case err := <-errCh:
		t.Fatalf("StartVipService returned early: %v", err)
	case <-time.After(2 * time.Second):
	}

	if n := killCalls.Load(); n != 0 {
		t.Fatalf("killFunc was called %d time(s), want 0 for invalid node address with localhost fallback", n)
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("StartVipService() error = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("StartVipService did not stop after context cancellation")
	}
}

var (
	errTestAddHost = &testError{msg: "mock AddHost error"}
	errTestDelHost = &testError{msg: "mock DelHost error"}
)

type testError struct{ msg string }

func (e *testError) Error() string { return e.msg }

// startVipService launches vipService in a goroutine with a mock network and
// registers a cleanup to cancel the context and wait for it to finish.
// Uses InitCluster so the real code parses certs for the BGP health check client.
func startVipService(t *testing.T, cfg *kubevip.Config, bgpServer bgp.BGPManager) (context.CancelFunc, <-chan struct{}) {
	t.Helper()
	c, err := cluster.InitCluster(cfg, true, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("InitCluster: %v", err)
	}
	c.Network = []vip.Network{&mockNetwork{ip: "10.0.0.1", cidr: testCIDR}}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		_ = c.StartVipService(ctx, cfg, nil, bgpServer, func() {})
		close(done)
	}()

	t.Cleanup(func() {
		cancel()
		<-done
	})

	return cancel, done
}

// startRoutingTableVipService launches vipService in routing-table mode with a
// mock network and a real route.Manager (which only drives the mock network's
// route methods, so no netlink calls happen). Registers cleanup to stop it.
func startRoutingTableVipService(t *testing.T, cfg *kubevip.Config, network *mockNetwork) {
	t.Helper()

	c, err := cluster.InitCluster(cfg, true, nil, nil, route.NewManager(), nil)
	if err != nil {
		t.Fatalf("InitCluster: %v", err)
	}
	c.Network = []vip.Network{network}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		_ = c.StartVipService(ctx, cfg, nil, nil, func() {})
		close(done)
	}()

	t.Cleanup(func() {
		cancel()
		<-done
	})
}

func newRoutingTableConfig(url, caPath string) *kubevip.Config {
	cfg := newBGPConfig(url, caPath)
	cfg.EnableBGP = false
	cfg.EnableRoutingTable = true
	cfg.BackendHealthCheckInterval = 1
	return cfg
}

func newBGPConfig(url, caPath string) *kubevip.Config {
	return &kubevip.Config{
		EnableBGP: true,
		ControlPlaneHealthCheck: kubevip.HealthCheck{
			Address:          url,
			CAPath:           caPath,
			PeriodSeconds:    1,
			TimeoutSeconds:   2,
			FailureThreshold: 1,
		},
	}
}

// mockBGPRouteManager tracks announced addresses as a set.
// AddHost adds, DelHost removes. Errors prevent state changes.
type mockBGPRouteManager struct {
	mu        sync.Mutex
	announced map[string]bool
	addErr    error
	delErr    error
}

func newMockBGPRouteManager() *mockBGPRouteManager {
	return &mockBGPRouteManager{announced: make(map[string]bool)}
}

func (m *mockBGPRouteManager) AddHost(_ context.Context, addr string, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.addErr != nil {
		return m.addErr
	}
	m.announced[addr] = true
	return nil
}

func (m *mockBGPRouteManager) DelHost(_ context.Context, addr string, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.delErr != nil {
		return m.delErr
	}
	delete(m.announced, addr)
	return nil
}

func (m *mockBGPRouteManager) isAnnounced() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.announced[testCIDR]
}

func (m *mockBGPRouteManager) setAddErr(err error) {
	m.mu.Lock()
	m.addErr = err
	m.mu.Unlock()
}

func (m *mockBGPRouteManager) setDelErr(err error) {
	m.mu.Lock()
	m.delErr = err
	m.mu.Unlock()
}

// mockNetwork implements vip.Network with no-op operations.
type mockNetwork struct {
	ip   string
	cidr string

	mu            sync.Mutex
	present       bool
	setMaskErr    error
	addIPCalls    int
	deleteIPCalls int
}

func (m *mockNetwork) AddIP(bool, bool, ...int) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.addIPCalls++
	m.present = true
	return true, nil
}
func (m *mockNetwork) DeleteIP() (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleteIPCalls++
	deleted := m.present
	m.present = false
	return deleted, nil
}
func (m *mockNetwork) isPresent() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.present
}
func (m *mockNetwork) AddRoute(bool) (bool, error) { return false, nil }
func (m *mockNetwork) ReplaceRoute() error         { return nil }
func (m *mockNetwork) DeleteRoute() error          { return nil }
func (m *mockNetwork) UpdateRoutes() (bool, error) { return false, nil }
func (m *mockNetwork) IsSet() (*netlink.Addr, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.present {
		return &netlink.Addr{}, nil
	}
	return nil, nil
}
func (m *mockNetwork) IP() string                      { return m.ip }
func (m *mockNetwork) CIDR() string                    { return m.cidr }
func (m *mockNetwork) IPisLinkLocal() bool             { return false }
func (m *mockNetwork) PrepareRoute() *netlink.Route    { return nil }
func (m *mockNetwork) RouteHash() string               { return "" }
func (m *mockNetwork) SetIP(string) error              { return nil }
func (m *mockNetwork) SetServicePorts(*corev1.Service) {}
func (m *mockNetwork) Interface() string               { return "eth0" }
func (m *mockNetwork) IsDADFAIL() bool                 { return false }
func (m *mockNetwork) IsDNS() bool                     { return false }
func (m *mockNetwork) IsDDNS() bool                    { return false }
func (m *mockNetwork) DDNSHostName() string            { return "" }
func (m *mockNetwork) DNSName() string                 { return "" }
func (m *mockNetwork) SetMask(string) error            { return m.setMaskErr }
func (m *mockNetwork) SetHasEndpoints(bool)            {}
func (m *mockNetwork) HasEndpoints() bool              { return false }
func (m *mockNetwork) ARPName() string                 { return "" }
func (m *mockNetwork) GetPossibleSubnets() string      { return "" }
func (m *mockNetwork) DHCPFamily() string              { return "" }
func (m *mockNetwork) IPVSMark() uint32                { return 0 }

// testHealthServer wraps an HTTPS httptest.Server with an atomic status code.
// caPath is the path to the server's CA cert for client verification.
type testHealthServer struct {
	server     *httptest.Server
	statusCode atomic.Int64
	caPath     string
}

func newTestHealthServer(t *testing.T, status int) *testHealthServer {
	t.Helper()
	healthcheck := &testHealthServer{}
	healthcheck.statusCode.Store(int64(status))
	healthcheck.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(int(healthcheck.statusCode.Load()))
	}))

	cert := healthcheck.server.Certificate()
	if cert == nil {
		t.Fatal("TLS server has no certificate")
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	caFile := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(caFile, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	healthcheck.caPath = caFile
	return healthcheck
}

func (ths *testHealthServer) setStatus(code int) {
	ths.statusCode.Store(int64(code))
}

// expectConsistently continuously checks that condition remains true for the given duration.
// Fails immediately if the condition becomes false at any point.
func expectConsistently(t *testing.T, condition func() bool, duration time.Duration, msg string) {
	t.Helper()
	deadline := time.Now().Add(duration)
	for time.Now().Before(deadline) {
		if !condition() {
			t.Fatalf("condition violated: %s", msg)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// expectEventually polls condition until it returns true or 5s timeout is reached.
func expectEventually(t *testing.T, condition func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timeout: %s", msg)
}
