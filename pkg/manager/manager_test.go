package manager

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
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

func TestKillDoesNotBlockWhenSignalChannelIsFull(t *testing.T) {
	manager := &Manager{
		config:     &kubevip.Config{},
		signalChan: make(chan os.Signal, 1),
	}
	manager.signalChan <- syscall.SIGUSR1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	shutdownDone := make(chan struct{})
	go func() {
		manager.waitForShutdown(ctx, cancel, nil)
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

func TestWaitForShutdownHandlesKillSignal(t *testing.T) {
	manager := &Manager{
		signalChan: make(chan os.Signal, 1),
	}
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
	if !manager.closing.Load() {
		t.Fatal("Kill did not mark the manager as closing")
	}
}
