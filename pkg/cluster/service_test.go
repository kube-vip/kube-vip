package cluster_test

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kube-vip/kube-vip/pkg/cluster"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/vip"
	"github.com/vishvananda/netlink"
	corev1 "k8s.io/api/core/v1"
)

const testCIDR = "10.0.0.34/32"

func TestBGPHealthCheckLoop_AnnouncesOnHealthy(t *testing.T) {
	t.Parallel()
	healthcheck := newTestHealthServer(t, http.StatusOK)
	t.Cleanup(healthcheck.server.Close)

	bgpManager := newMockBGPRouteManager()
	startVipService(t, newTestConfig(healthcheck.server.URL, healthcheck.caPath), bgpManager)

	expectEventually(t, func() bool { return bgpManager.isAnnounced() },
		"route should be announced")
}

func TestBGPHealthCheckLoop_NoAnnouncementUntilHealthy(t *testing.T) {
	t.Parallel()
	healthcheck := newTestHealthServer(t, http.StatusInternalServerError)
	t.Cleanup(healthcheck.server.Close)

	bgpManager := newMockBGPRouteManager()
	startVipService(t, newTestConfig(healthcheck.server.URL, healthcheck.caPath), bgpManager)

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
	cfg := newTestConfig(healthcheck.server.URL, healthcheck.caPath)
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
	cfg := newTestConfig(healthcheck.server.URL, healthcheck.caPath)
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
	cancelContext, vipServiceDone := startVipService(t, newTestConfig(healthcheck.server.URL, healthcheck.caPath), bgpManager)

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
	startVipService(t, newTestConfig(healthcheck.server.URL, healthcheck.caPath), bgpManager)

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
	cfg := newTestConfig(healthcheck.server.URL, healthcheck.caPath)
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

var (
	errTestAddHost = &testError{msg: "mock AddHost error"}
	errTestDelHost = &testError{msg: "mock DelHost error"}
)

type testError struct{ msg string }

func (e *testError) Error() string { return e.msg }

// startVipService launches vipService in a goroutine with a mock network and
// registers a cleanup to cancel the context and wait for it to finish.
// Uses InitCluster so the real code parses certs for the BGP health check client.
func startVipService(t *testing.T, cfg *kubevip.Config, bgpManager *mockBGPRouteManager) (context.CancelFunc, <-chan struct{}) {
	t.Helper()

	c, err := cluster.InitCluster(cfg, true, nil, nil)
	if err != nil {
		t.Fatalf("InitCluster: %v", err)
	}
	c.Network = []vip.Network{&mockNetwork{ip: "10.0.0.1", cidr: testCIDR}}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		_ = c.StartVipService(ctx, cfg, nil, bgpManager, func() {})
		close(done)
	}()

	t.Cleanup(func() {
		cancel()
		<-done
	})

	return cancel, done
}

func newTestConfig(url, caPath string) *kubevip.Config {
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

func (m *mockBGPRouteManager) AddHost(_ context.Context, addr string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.addErr != nil {
		return m.addErr
	}
	m.announced[addr] = true
	return nil
}

func (m *mockBGPRouteManager) DelHost(_ context.Context, addr string) error {
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
}

func (m *mockNetwork) AddIP(bool, bool, ...int) (bool, error) { return true, nil }
func (m *mockNetwork) AddRoute(bool) error                    { return nil }
func (m *mockNetwork) DeleteIP() (bool, error)                { return false, nil }
func (m *mockNetwork) DeleteRoute() error                     { return nil }
func (m *mockNetwork) UpdateRoutes() (bool, error)            { return false, nil }
func (m *mockNetwork) IsSet() (*netlink.Addr, error)          { return nil, nil }
func (m *mockNetwork) IP() string                             { return m.ip }
func (m *mockNetwork) CIDR() string                           { return m.cidr }
func (m *mockNetwork) IPisLinkLocal() bool                    { return false }
func (m *mockNetwork) PrepareRoute() *netlink.Route           { return nil }
func (m *mockNetwork) SetIP(string) error                     { return nil }
func (m *mockNetwork) SetServicePorts(*corev1.Service)        {}
func (m *mockNetwork) Interface() string                      { return "eth0" }
func (m *mockNetwork) IsDADFAIL() bool                        { return false }
func (m *mockNetwork) IsDNS() bool                            { return false }
func (m *mockNetwork) IsDDNS() bool                           { return false }
func (m *mockNetwork) DDNSHostName() string                   { return "" }
func (m *mockNetwork) DNSName() string                        { return "" }
func (m *mockNetwork) SetMask(string) error                   { return nil }
func (m *mockNetwork) SetHasEndpoints(bool)                   {}
func (m *mockNetwork) HasEndpoints() bool                     { return false }
func (m *mockNetwork) ARPName() string                        { return "" }
func (m *mockNetwork) GetPossibleSubnets() string             { return "" }
func (m *mockNetwork) DHCPFamily() string                     { return "" }

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
