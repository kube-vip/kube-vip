//go:build e2e

package bgp

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	api "github.com/osrg/gobgp/v4/api"
	"google.golang.org/grpc"
)

type testGoBGPServer struct {
	api.UnimplementedGoBgpServiceServer
}

func (testGoBGPServer) GetBgp(context.Context, *api.GetBgpRequest) (*api.GetBgpResponse, error) {
	return &api.GetBgpResponse{Global: &api.Global{Asn: GoBGPAS}}, nil
}

func TestConfigDisablesBGPListener(t *testing.T) {
	t.Parallel()

	config, err := os.ReadFile("config.toml.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(config), "port = -1") {
		t.Fatal("GoBGP test config must disable its privileged BGP listener")
	}
}

func TestWaitForGoBGPReady(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	server := grpc.NewServer()
	api.RegisterGoBgpServiceServer(server, testGoBGPServer{})
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()

	tests := []struct {
		name      string
		address   string
		exited    chan error
		stdout    string
		stderr    string
		wantError string
	}{
		{name: "ready", address: listener.Addr().String(), exited: make(chan error)},
		{name: "exited", address: "127.0.0.1:0", exited: bufferedError(errors.New("boom")), stdout: "started", stderr: "bind: permission denied", wantError: "stdout:\nstarted\nstderr:\nbind: permission denied"},
		{name: "timeout", address: "127.0.0.1:0", exited: make(chan error), stderr: "still starting", wantError: "timed out waiting for GoBGP API readiness\nstderr:\nstill starting"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := waitForGoBGPReady(test.address, test.exited, bytes.NewBufferString(test.stdout), bytes.NewBufferString(test.stderr), 100*time.Millisecond)
			if test.wantError == "" && err != nil {
				t.Fatalf("waitForGoBGPReady() error = %v", err)
			}
			if test.wantError != "" && (err == nil || !strings.Contains(err.Error(), test.wantError)) {
				t.Fatalf("waitForGoBGPReady() error = %v, want substring %q", err, test.wantError)
			}
		})
	}
}

func bufferedError(err error) chan error {
	result := make(chan error, 1)
	result <- err
	return result
}
