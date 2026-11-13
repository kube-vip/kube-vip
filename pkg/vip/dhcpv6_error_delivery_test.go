package vip

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDHCPv6ReportErrorStopsWhenContextIsCancelled(t *testing.T) {
	client := &DHCPv6Client{errorChan: make(chan error), stopChan: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		client.reportError(ctx, errors.New("request failed"))
		close(done)
	}()
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("error delivery blocked after context cancellation")
	}
}

func TestDHCPv6ReportErrorDeliversToActiveReceiver(t *testing.T) {
	client := &DHCPv6Client{errorChan: make(chan error), stopChan: make(chan struct{})}
	received := make(chan error, 1)
	go func() { received <- <-client.errorChan }()
	client.reportError(context.Background(), errors.New("request failed"))

	select {
	case err := <-received:
		if err == nil || err.Error() != "request failed" {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("active receiver did not receive error")
	}
}
