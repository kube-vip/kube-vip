package vip

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestDHCPv4BackoffExhaustionDoesNotDeadlock(t *testing.T) {
	// DEFECT: requestWithBackoff calls Stop from the Start/request goroutine before Start can close releasedChan, so exhausted DHCPv4 retries deadlock (pkg/vip/dhcpv4.go:229).
	client := NewDHCPv4Client(
		&net.Interface{Name: "definitely-not-a-kube-vip-interface"},
		false,
		"",
		1,
		false,
	)

	done := make(chan struct{})
	go func() {
		_, _ = client.requestWithBackoff(context.Background())
		close(done)
	}()

	select {
	case err := <-client.ErrorChannel():
		if err == nil {
			t.Fatal("expected DHCPv4 request error")
		}
	case <-time.After(time.Second):
		t.Fatal("DHCPv4 did not report exhausted backoff")
	}

	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("DHCPv4 backoff exhaustion deadlocked")
	}
}
