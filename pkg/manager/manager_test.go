package manager

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/kube-vip/kube-vip/pkg/election"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/manager/worker"
	"github.com/kube-vip/kube-vip/pkg/node/noop"
	"github.com/kube-vip/kube-vip/pkg/services"
	"github.com/stretchr/testify/assert"
)

type controlPlaneTestWorker struct {
	started chan struct{}
}

func (w *controlPlaneTestWorker) Configure(context.Context, *sync.WaitGroup) error { return nil }
func (w *controlPlaneTestWorker) InitControlPlane() error                          { return nil }
func (w *controlPlaneTestWorker) ConfigureServices()                               {}
func (w *controlPlaneTestWorker) StartServices(context.Context) error              { return nil }
func (w *controlPlaneTestWorker) Name() string                                     { return "control-plane-test" }
func (w *controlPlaneTestWorker) Cleanup()                                         {}

func (w *controlPlaneTestWorker) StartControlPlane(ctx context.Context, _ *election.Manager) {
	close(w.started)
	<-ctx.Done()
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

func TestStartReturnsCancellationCause(t *testing.T) {
	cause := errors.New("default interface is down")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(cause)

	manager := &Manager{
		config:           &kubevip.Config{},
		nodeLabelManager: noop.NewManager(),
	}
	err := manager.Start(ctx)
	if !errors.Is(err, cause) {
		t.Fatalf("Start() error = %v, want cancellation cause %v", err, cause)
	}
}

func TestStartModeControlPlaneOnlyWaitsForShutdown(t *testing.T) {
	config := &kubevip.Config{EnableControlPlane: true}
	w := &controlPlaneTestWorker{started: make(chan struct{})}
	manager := &Manager{
		config:       config,
		signalChan:   make(chan os.Signal),
		svcProcessor: services.NewServicesProcessor(config, nil, nil, nil, nil, nil, noop.NewManager(), nil, nil, nil),
		modeWorker:   func() worker.Worker { return w },
	}

	done := make(chan error, 1)
	go func() {
		done <- manager.startMode(context.Background())
	}()
	select {
	case <-w.started:
	case <-time.After(time.Second):
		t.Fatal("control-plane worker did not start")
	}
	select {
	case err := <-done:
		t.Fatalf("control-plane-only mode returned before shutdown: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	manager.Kill()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("control-plane-only mode returned an error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("control-plane-only mode did not stop after Kill")
	}
}

func TestKillSignalsShutdownWhileSignalIsQueued(t *testing.T) {
	manager := &Manager{
		config:     &kubevip.Config{},
		signalChan: make(chan os.Signal, 1),
	}
	manager.signalChan <- syscall.SIGUSR1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	shutdownDone := make(chan struct{})
	go func() {
		manager.waitForShutdown(ctx, cancel)
		close(shutdownDone)
	}()

	returned := make(chan struct{})
	go func() {
		manager.Kill()
		close(returned)
	}()

	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("Kill blocked while the OS signal channel was full")
	}
	select {
	case <-shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("Kill did not signal shutdown")
	}
}

func TestKillDoesNotBlockAfterShutdownWithPendingSignal(t *testing.T) {
	manager := &Manager{
		config:     &kubevip.Config{},
		signalChan: make(chan os.Signal, 1),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	shutdownDone := make(chan struct{})
	go func() {
		manager.waitForShutdown(ctx, func() {})
		close(shutdownDone)
	}()
	select {
	case <-shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("waitForShutdown did not return after context cancellation")
	}

	manager.signalChan <- syscall.SIGUSR1
	returned := make(chan struct{})
	go func() {
		manager.Kill()
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("Kill blocked after shutdown with a pending signal")
	}
}

func TestWaitForShutdownTracksAndCoalescesConfigurationDump(t *testing.T) {
	dumpStarted := make(chan struct{})
	releaseDump := make(chan struct{})
	var dumpCalls atomic.Int64
	manager := &Manager{
		signalChan: make(chan os.Signal, 3),
		dump: func(context.Context) {
			dumpCalls.Add(1)
			close(dumpStarted)
			<-releaseDump
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		manager.waitForShutdown(ctx, cancel)
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
	case <-done:
		t.Fatal("waitForShutdown returned before the configuration dump completed")
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseDump)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("waitForShutdown did not return after the configuration dump completed")
	}
	if got := dumpCalls.Load(); got != 1 {
		t.Fatalf("configuration dump calls = %d, want 1", got)
	}
}

func TestWaitForShutdownHandlesKillSignal(t *testing.T) {
	manager := &Manager{
		signalChan: make(chan os.Signal, 1),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		manager.waitForShutdown(ctx, cancel)
		close(done)
	}()
	manager.Kill()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("waitForShutdown did not return after Kill")
	}
	if !manager.closing.Load() {
		t.Fatal("Kill did not mark the manager as closing")
	}
}
