package manager

import (
	"context"
	"errors"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/kube-vip/kube-vip/pkg/election"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/manager/worker"
	"github.com/kube-vip/kube-vip/pkg/node/noop"
	"github.com/kube-vip/kube-vip/pkg/services"
	"github.com/kube-vip/kube-vip/pkg/upnp"
	"github.com/stretchr/testify/assert"
)

func TestWaitForShutdownReturnsWhenContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	manager := &Manager{signalChan: make(chan os.Signal, 1)}
	done := make(chan struct{})
	go func() {
		manager.waitForShutdown(ctx, cancel, nil)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("waitForShutdown did not return after context cancellation")
	}
}

func TestNormalizeNodeName(t *testing.T) {
	tests := []struct {
		name     string
		hostname string
		expected string
	}{
		{
			name:     "All lowercase hostname remains unchanged",
			hostname: "worker-node-1",
			expected: "worker-node-1",
		},
		{
			name:     "Mixed case hostname is lowercased",
			hostname: "Worker-Node-1",
			expected: "worker-node-1",
		},
		{
			name:     "All uppercase hostname is lowercased",
			hostname: "MASTER-NODE",
			expected: "master-node",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := normalizeNodeName(tt.hostname)
			assert.Equal(t, tt.expected, result, "The normalized node name did not match the expected RFC1123 compliant name")
		})
	}
}

func TestKillDoesNotBlockWithQueuedSignal(t *testing.T) {
	manager := &Manager{signalChan: make(chan os.Signal, 1)}
	manager.signalChan <- syscall.SIGUSR1
	returned := make(chan struct{})
	go func() {
		manager.Kill()
		manager.Kill()
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("Kill blocked while the OS signal channel was full")
	}
	select {
	case <-manager.killChannel():
	default:
		t.Fatal("Kill did not signal shutdown")
	}
}

func TestWaitForShutdownHandlesKillSignal(t *testing.T) {
	manager := &Manager{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		manager.waitForShutdown(ctx, cancel, nil)
		close(done)
	}()
	manager.Kill()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("waitForShutdown did not return after Kill")
	}
	if !manager.closing.Load() || ctx.Err() == nil {
		t.Fatal("Kill did not mark the manager as closing and cancel its context")
	}
}

func TestWaitForShutdownTracksAndCoalescesConfigurationDump(t *testing.T) {
	dumpStarted := make(chan struct{})
	releaseDump := make(chan struct{})
	var dumpCalls atomic.Int64
	manager := &Manager{
		signalChan: make(chan os.Signal),
		dump: func(context.Context) {
			dumpCalls.Add(1)
			close(dumpStarted)
			<-releaseDump
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer close(releaseDump)
	done := make(chan struct{})
	go func() {
		manager.waitForShutdown(ctx, cancel, nil)
		close(done)
	}()
	manager.signalChan <- syscall.SIGUSR1
	select {
	case <-dumpStarted:
	case <-time.After(time.Second):
		t.Fatal("configuration dump did not start")
	}
	manager.signalChan <- syscall.SIGUSR1
	manager.Kill()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("configuration dump blocked shutdown cancellation")
	}
	select {
	case <-done:
		t.Fatal("shutdown returned before the configuration dump completed")
	default:
	}
	if got := dumpCalls.Load(); got != 1 {
		t.Fatalf("configuration dump calls = %d, want 1", got)
	}
}

type shutdownTestWorker struct {
	worker.Worker
	configure func(context.Context, *sync.WaitGroup) error
	start     func(context.Context)
}

func (w *shutdownTestWorker) Name() string            { return "test" }
func (w *shutdownTestWorker) Cleanup()                {}
func (w *shutdownTestWorker) InitControlPlane() error { return nil }
func (w *shutdownTestWorker) Configure(ctx context.Context, wg *sync.WaitGroup) error {
	if w.configure != nil {
		return w.configure(ctx, wg)
	}
	return nil
}
func (w *shutdownTestWorker) StartControlPlane(ctx context.Context, _ *election.Manager) {
	w.start(ctx)
}

func TestStartModeControlPlaneExitStopsManager(t *testing.T) {
	config := &kubevip.Config{EnableControlPlane: true}
	processor, err := services.NewServicesProcessor(config, nil, nil, nil, nil, nil, noop.NewManager(), nil, lease.NewManager(), nil)
	if err != nil {
		t.Fatal(err)
	}
	manager := &Manager{
		config:       config,
		svcProcessor: processor,
		modeWorker: func() worker.Worker {
			return &shutdownTestWorker{start: func(context.Context) {}}
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.startMode(ctx); err != nil {
		t.Fatal(err)
	}
	if !manager.closing.Load() {
		t.Fatal("control-plane exit did not shut down the manager")
	}
}

func TestStartModeCancelsWorkersBeforeWaiting(t *testing.T) {
	config := &kubevip.Config{}
	processor, err := services.NewServicesProcessor(config, nil, nil, nil, nil, nil, noop.NewManager(), nil, lease.NewManager(), nil)
	if err != nil {
		t.Fatal(err)
	}
	stopped := make(chan struct{})
	manager := &Manager{
		config:       config,
		svcProcessor: processor,
		modeWorker: func() worker.Worker {
			return &shutdownTestWorker{configure: func(ctx context.Context, wg *sync.WaitGroup) error {
				wg.Go(func() {
					<-ctx.Done()
					close(stopped)
				})
				return nil
			}}
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.startMode(ctx); err != nil {
		t.Fatal(err)
	}
	if ctx.Err() != nil {
		t.Fatal("startMode relied on parent cancellation to drain workers")
	}
	select {
	case <-stopped:
	default:
		t.Fatal("startMode returned before its worker stopped")
	}
}

func TestStartStopsHealthServerWhenModeExits(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}

	config := &kubevip.Config{EnableControlPlane: true, HealthCheckPort: port}
	processor, err := services.NewServicesProcessor(config, nil, nil, nil, nil, nil, noop.NewManager(), nil, lease.NewManager(), nil)
	if err != nil {
		t.Fatal(err)
	}
	manager := &Manager{
		config:           config,
		svcProcessor:     processor,
		nodeLabelManager: noop.NewManager(),
		modeWorker: func() worker.Worker {
			return &shutdownTestWorker{start: func(context.Context) {}}
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- manager.Start(ctx)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("Start did not return after the control plane exited; the health server kept it alive")
	}
	if ctx.Err() != nil {
		t.Fatal("Start relied on parent cancellation to stop the health server")
	}
}

type shutdownTestUPNPGateway struct {
	upnp.ConnectionClient
	externalAddress func(context.Context) (string, error)
}

func (g shutdownTestUPNPGateway) GetExternalIPAddressCtx(ctx context.Context) (string, error) {
	return g.externalAddress(ctx)
}

func TestStartStopsUPNPWhenModeExits(t *testing.T) {
	for _, tc := range []struct {
		name              string
		configureErr      error
		terminateOnLookup bool
	}{
		{name: "termination during refresh"},
		{name: "termination during gateway lookup", terminateOnLookup: true},
		{name: "mode configuration failure", configureErr: errors.New("mode setup failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := &kubevip.Config{EnableARP: true, EnableControlPlane: true, EnableUPNP: true}
			processor, err := services.NewServicesProcessor(config, nil, nil, nil, nil, nil, noop.NewManager(), nil, lease.NewManager(), nil)
			if err != nil {
				t.Fatal(err)
			}
			signals := make(chan os.Signal, 1)
			refreshHoldingLock := make(chan struct{})
			serviceLock := services.NewServiceLock()
			const serviceUID = "service-using-upnp"
			lookupStarted := false
			manager := &Manager{
				config:           config,
				signalChan:       signals,
				svcProcessor:     processor,
				nodeLabelManager: noop.NewManager(),
				discoverUPNP: func(context.Context) []upnp.ConnectionClient {
					return []upnp.ConnectionClient{shutdownTestUPNPGateway{
						externalAddress: func(ctx context.Context) (string, error) {
							lookupStarted = true
							if tc.terminateOnLookup {
								signals <- syscall.SIGTERM
								<-ctx.Done()
								return "", ctx.Err()
							}
							return "192.0.2.1", nil
						},
					}}
				},
				refreshUPNP: func(ctx context.Context) {
					serviceLock.Lock(serviceUID)
					defer func() { _ = serviceLock.Unlock(serviceUID) }()
					close(refreshHoldingLock)
					<-ctx.Done()
				},
				modeWorker: func() worker.Worker {
					return &shutdownTestWorker{
						configure: func(context.Context, *sync.WaitGroup) error { return tc.configureErr },
						start: func(ctx context.Context) {
							select {
							case <-refreshHoldingLock:
							case <-ctx.Done():
								return
							}
							signals <- syscall.SIGTERM
							<-ctx.Done()
							// Service teardown needs the lock held by the in-flight refresh.
							serviceLock.Lock(serviceUID)
							_ = serviceLock.Unlock(serviceUID)
						},
					}
				},
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- manager.Start(ctx) }()

			select {
			case err := <-done:
				if !errors.Is(err, tc.configureErr) {
					t.Fatalf("Start returned %v, want %v", err, tc.configureErr)
				}
				if tc.configureErr == nil && !lookupStarted {
					t.Fatal("Start skipped the UPnP gateway lookup")
				}
			case <-time.After(time.Second):
				t.Fatal("UPnP worker did not stop when kube-vip shut down")
			}
		})
	}
}
