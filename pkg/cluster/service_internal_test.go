package cluster

import (
	"net/http"
	"strings"
	"testing"

	"github.com/kube-vip/kube-vip/pkg/backend"
)

func TestHTTPBackendCreation(t *testing.T) {
	cases := []struct {
		name      string
		addr      string
		port      uint16
		wantAddr  string
		wantPort  uint16
		wantErr   string
		nilClient bool
	}{
		{
			name:     "explicit v4 loopback with port",
			addr:     "https://127.0.0.1:6443",
			port:     9999,
			wantAddr: "https://127.0.0.1:6443",
			wantPort: 0,
		},
		{
			name:     "explicit v4 loopback with port and livez endpoint",
			addr:     "https://127.0.0.1:6443/livez",
			port:     9999,
			wantAddr: "https://127.0.0.1:6443/livez",
			wantPort: 0,
		},
		{
			name:    "empty override",
			addr:    "",
			port:    6443,
			wantErr: "empty address",
		},
		{
			name:    "garbage override",
			addr:    "://not-a-url",
			port:    6443,
			wantErr: "missing protocol scheme",
		},
		{
			name:      "nil client",
			addr:      "https://127.0.0.1:6443",
			port:      6443,
			wantErr:   "nil client",
			nilClient: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &backend.Config{
				Type:           backend.HTTP,
				Address:        tc.addr,
				Port:           tc.port,
				KubeConfigPath: "/fake/path",
				Client:         &http.Client{},
			}
			if tc.nilClient {
				cfg.Client = nil
			}
			entry, err := backend.New(cfg)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("backend.New() error = %v, want error containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unable to create backend: %v", err)
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
