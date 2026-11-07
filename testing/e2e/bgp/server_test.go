//go:build e2e

package bgp

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"text/template"
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

func TestConfigUsesUnprivilegedBGPListener(t *testing.T) {
	t.Parallel()

	tmpl, err := template.ParseFiles("config.toml.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	var config bytes.Buffer
	if err := tmpl.Execute(&config, configValues{AS: GoBGPAS, Port: GoBGPPort, IPv4: "172.18.0.1", IPv6: "fd00::1"}); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"port = 1179", `local-address-list = ["172.18.0.1", "fd00::1"]`} {
		if !strings.Contains(config.String(), expected) {
			t.Fatalf("GoBGP test config does not contain %q", expected)
		}
	}
}

func TestValidateBindAddresses(t *testing.T) {
	t.Parallel()
	if err := validateBindAddresses("172.18.0.1", "fd00::1"); err != nil {
		t.Fatalf("validateBindAddresses() error = %v", err)
	}
	for _, pair := range [][2]string{{"", "fd00::1"}, {"::1", "fd00::1"}, {"172.18.0.1", "fe80::1"}} {
		if err := validateBindAddresses(pair[0], pair[1]); err == nil {
			t.Fatalf("validateBindAddresses(%q, %q) succeeded", pair[0], pair[1])
		}
	}
}

func TestRouteFamily(t *testing.T) {
	t.Parallel()
	tests := []struct {
		address string
		want    api.Family_Afi
	}{
		{address: "192.0.2.1", want: api.Family_AFI_IP},
		{address: "2001:db8::1", want: api.Family_AFI_IP6},
	}
	for _, test := range tests {
		t.Run(test.address, func(t *testing.T) {
			family := routeFamily(test.address)
			if family.Afi != test.want || family.Safi != api.Family_SAFI_UNICAST {
				t.Fatalf("routeFamily(%q) = %s/%s, want %s/%s", test.address, family.Afi, family.Safi, test.want, api.Family_SAFI_UNICAST)
			}
		})
	}
}

func TestPeerRequestEnablesMultiprotocolFamilies(t *testing.T) {
	t.Parallel()
	for _, address := range []string{"192.0.2.2", "2001:db8::2"} {
		t.Run(address, func(t *testing.T) {
			peer := peerRequest(address, KubevipAS).GetPeer()
			if peer.GetConf().GetNeighborAddress() != address || peer.GetConf().GetPeerAsn() != KubevipAS {
				t.Fatalf("peer config = %+v", peer.GetConf())
			}
			families := peer.GetAfiSafis()
			if len(families) != 2 || families[0].GetConfig().GetFamily().GetAfi() != api.Family_AFI_IP ||
				families[1].GetConfig().GetFamily().GetAfi() != api.Family_AFI_IP6 {
				t.Fatalf("peer families = %+v, want IPv4 and IPv6 unicast", families)
			}
		})
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
