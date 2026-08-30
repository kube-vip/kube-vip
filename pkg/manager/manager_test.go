package manager

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/node/noop"
	"github.com/stretchr/testify/assert"
)

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

func TestKillDoesNotBlockWhenSignalChannelIsFull(t *testing.T) {
	manager := &Manager{
		config:     &kubevip.Config{},
		signalChan: make(chan os.Signal, 1),
		dump:       func(context.Context) {},
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
