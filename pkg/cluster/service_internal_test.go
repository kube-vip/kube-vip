package cluster

import (
	"testing"

	"github.com/kube-vip/kube-vip/pkg/backend"
)

func TestHTTPBackendCreation(t *testing.T) {
	cases := []struct {
		name        string
		addr        string
		port        uint16
		wantAddr    string
		wantPort    uint16
		wantNil     bool
		keepAddress bool
	}{
		{
			name:        "explicit v4 loopback with port",
			addr:        "https://127.0.0.1:6443",
			port:        9999,
			wantAddr:    "127.0.0.1",
			wantPort:    6443,
			keepAddress: false,
		},
		{
			name:        "explicit v4 loopback with port and livez endpoint",
			addr:        "https://127.0.0.1:6443/livez",
			port:        9999,
			wantAddr:    "https://127.0.0.1:6443/livez",
			wantPort:    0,
			keepAddress: true,
		},
		{
			name:        "hostname without port falls back to config port",
			addr:        "https://localhost",
			port:        6443,
			wantAddr:    "localhost",
			wantPort:    6443,
			keepAddress: false,
		},
		{
			name:        "empty override",
			addr:        "",
			port:        6443,
			wantNil:     true,
			keepAddress: false,
		},
		{
			name:        "garbage override",
			addr:        "://not-a-url",
			port:        6443,
			wantNil:     true,
			keepAddress: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &backend.Config{
				Type:           backend.HTTP,
				Address:        tc.addr,
				Port:           tc.port,
				KubeConfigPath: "/fake/path",
				Client:         nil,
				KeepAddress:    tc.keepAddress,
			}
			entry, err := backend.New(cfg)
			if err != nil {
				t.Fatalf("unable to create backend: %s", err.Error())
			}
			if tc.wantNil {
				if entry != nil {
					t.Fatalf("expected nil entry, got %+v", entry)
				}
				return
			}
			if entry == nil {
				t.Fatal("expected an entry, got nil")
			}
			if entry.Address() != tc.wantAddr || entry.Port() != tc.wantPort {
				t.Fatalf("got %+v, want addr %q port %d", entry, tc.wantAddr, tc.wantPort)
			}
		})
	}
}
