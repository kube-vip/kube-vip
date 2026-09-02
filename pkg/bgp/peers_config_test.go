package bgp

import (
	"context"
	"sync"
	"testing"

	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	api "github.com/osrg/gobgp/v4/api"
	gobgp "github.com/osrg/gobgp/v4/pkg/server"
)

func TestAddPeerConfiguresTransportOptions(t *testing.T) {
	tests := []struct {
		name          string
		newServer     func(*testing.T) *Server
		peer          kubevip.BGPPeer
		wantPort      uint32
		wantLocalAddr string
		wantInterface string
	}{
		{
			name: "configured remote port",
			newServer: func(t *testing.T) *Server {
				return newStartedTestBGPServer(t, kubevip.BGPConfig{
					AS:       65000,
					RouterID: "192.0.2.1",
					Peers:    []kubevip.BGPPeer{{Address: "192.0.2.10", AS: 65001}},
				})
			},
			peer:     kubevip.BGPPeer{Address: "192.0.2.10", AS: 65001, Port: 180},
			wantPort: 180,
		},
		{
			name: "configured source interface after MP-BGP fallback",
			newServer: func(t *testing.T) *Server {
				return newPeerTestServer(t, kubevip.BGPConfig{
					AS:           65000,
					RouterID:     "192.0.2.1",
					SourceIF:     "lo",
					MpbgpNexthop: "fixed",
					Peers:        []kubevip.BGPPeer{{Address: "192.0.2.20", AS: 65001}},
					MpbgpIPv4:    "",
					MpbgpIPv6:    "",
				})
			},
			peer:          kubevip.BGPPeer{Address: "192.0.2.20", AS: 65001},
			wantInterface: "lo",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			server := tt.newServer(t)
			if err := server.AddPeer(context.Background(), tt.peer); err != nil {
				t.Fatalf("AddPeer() error = %v", err)
			}

			peer := listTestPeer(t, server, tt.peer.Address)
			if peer.GetTransport() == nil {
				t.Fatal("configured peer has no transport")
			}
			if tt.wantPort != 0 && peer.GetTransport().GetRemotePort() != tt.wantPort {
				t.Fatalf("remote port = %d, want %d", peer.GetTransport().GetRemotePort(), tt.wantPort)
			}
			if tt.wantLocalAddr != "" && peer.GetTransport().GetLocalAddress() != tt.wantLocalAddr {
				t.Fatalf("local address = %q, want %q", peer.GetTransport().GetLocalAddress(), tt.wantLocalAddr)
			}
			if tt.wantInterface != "" && peer.GetTransport().GetBindInterface() != tt.wantInterface {
				t.Fatalf("bind interface = %q, want %q", peer.GetTransport().GetBindInterface(), tt.wantInterface)
			}
		})
	}
}

func newStartedTestBGPServer(t *testing.T, config kubevip.BGPConfig) *Server {
	t.Helper()

	server, err := NewBGPServer(config, log.LevelError)
	if err != nil {
		t.Fatalf("NewBGPServer() error = %v", err)
	}

	go server.s.Serve()
	if err := server.s.StartBgp(context.Background(), &api.StartBgpRequest{
		Global: &api.Global{
			Asn:        config.AS,
			RouterId:   config.RouterID,
			ListenPort: -1,
		},
	}); err != nil {
		server.s.Stop()
		t.Fatalf("StartBgp() error = %v", err)
	}
	t.Cleanup(server.s.Stop)

	return server
}

func listTestPeer(t *testing.T, server *Server, address string) *api.Peer {
	t.Helper()

	var got *api.Peer
	if err := server.s.ListPeer(context.Background(), &api.ListPeerRequest{Address: address}, func(peer *api.Peer) {
		got = peer
	}); err != nil {
		t.Fatalf("ListPeer() error = %v", err)
	}
	if got == nil {
		t.Fatalf("ListPeer() returned no peer for %s", address)
	}
	return got
}

func newPeerTestServer(t *testing.T, cfg kubevip.BGPConfig) *Server {
	t.Helper()
	raw := startEmbeddedRawBGP(t)
	return &Server{s: raw, c: &cfg, tracker: make(map[string]map[string]bool)}
}

func startEmbeddedRawBGP(t *testing.T) *gobgp.BgpServer {
	t.Helper()
	raw := gobgp.NewBgpServer()
	go raw.Serve()
	if err := raw.StartBgp(context.Background(), &api.StartBgpRequest{
		Global: &api.Global{
			Asn:        65000,
			RouterId:   "192.0.2.1",
			ListenPort: -1,
		},
	}); err != nil {
		t.Fatalf("starting embedded BGP server: %v", err)
	}
	var stopOnce sync.Once
	t.Cleanup(func() {
		stopOnce.Do(func() {
			if err := raw.StopBgp(context.Background(), &api.StopBgpRequest{}); err != nil {
				t.Logf("stopping embedded BGP server: %v", err)
			}
		})
	})
	return raw
}
