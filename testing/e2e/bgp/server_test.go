//go:build e2e

package bgp

import (
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

func TestWaitForProcessReady(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	tests := []struct {
		name      string
		address   string
		exited    chan error
		wantError string
	}{
		{name: "ready", address: listener.Addr().String(), exited: make(chan error)},
		{name: "exited", address: "127.0.0.1:0", exited: bufferedError(errors.New("boom")), wantError: "boom"},
		{name: "timeout", address: "127.0.0.1:0", exited: make(chan error), wantError: "timed out"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := waitForProcessReady(test.address, test.exited, 100*time.Millisecond)
			if test.wantError == "" && err != nil {
				t.Fatalf("waitForProcessReady() error = %v", err)
			}
			if test.wantError != "" && (err == nil || !strings.Contains(err.Error(), test.wantError)) {
				t.Fatalf("waitForProcessReady() error = %v, want substring %q", err, test.wantError)
			}
		})
	}
}

func bufferedError(err error) chan error {
	result := make(chan error, 1)
	result <- err
	return result
}
