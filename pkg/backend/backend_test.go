package backend

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProcessConfig(t *testing.T) {
	tests := []struct {
		backendType BackendType
		name        string
		address     string
		port        uint16
		wantAddress string
		wantPort    uint16
		wantErr     bool
	}{
		{
			backendType: HTTP,
			name:        "preserves health check URL and path in HTTP backend",
			address:     "http://localhost:6443/livez",
			port:        8443,
			wantAddress: "http://localhost:6443/livez",
			wantPort:    0,
		},
		{
			backendType: Discovery,
			name:        "extracts IPv4 host and port in Discovery backend",
			address:     "127.0.0.1:6666",
			port:        8443,
			wantAddress: "127.0.0.1",
			wantPort:    6666,
		},
		{
			backendType: Discovery,
			name:        "extracts IPv6 host and port in Discovery backend",
			address:     "[::1]:6666",
			port:        8443,
			wantAddress: "::1",
			wantPort:    6666,
		},
		{
			backendType: Discovery,
			name:        "extracts IPv4 host and default port in Discovery backend if port not specified in the address",
			address:     "127.0.0.1",
			port:        8443,
			wantAddress: "127.0.0.1",
			wantPort:    8443,
		},
		{
			backendType: Discovery,
			name:        "extracts IPv6 host and default port in Discovery backend if port not specified in the address",
			address:     "[::1]",
			port:        8443,
			wantAddress: "::1",
			wantPort:    8443,
		},
		{
			backendType: HTTP,
			name:        "rejects empty address in HTTP backend",
			wantErr:     true,
		},
		{
			backendType: Discovery,
			name:        "rejects empty address in Discovery backend",
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := &Config{
				Type:    tt.backendType,
				Address: tt.address,
				Port:    tt.port,
			}

			cfg, err := processConfig(config)
			if (err != nil) != tt.wantErr {
				t.Fatalf("processConfig() error = %v, wantErr %t", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if cfg.Address != tt.wantAddress {
				t.Errorf("Address = %q, want %q", cfg.Address, tt.wantAddress)
			}
			if cfg.Port != tt.wantPort {
				t.Errorf("Port = %d, want %d", cfg.Port, tt.wantPort)
			}

			if config.Address != tt.address {
				t.Errorf("Original address = %q, want %q", config.Address, tt.address)
			}
			if config.Port != tt.port {
				t.Errorf("Original port = %d, want %d", config.Port, tt.port)
			}

		})
	}
}

func TestProcessAddress(t *testing.T) {
	tests := []struct {
		name     string
		address  string
		wantHost string
		wantPort uint16
		wantErr  bool
	}{
		{
			name:     "IPv4 literal",
			address:  "127.0.0.1",
			wantHost: "127.0.0.1",
		},
		{
			name:     "IPv6 literal",
			address:  "::1",
			wantHost: "::1",
		},
		{
			name:     "host and port",
			address:  "localhost:6443",
			wantHost: "localhost",
			wantPort: 6443,
		},
		{
			name:     "URL retains scheme and excludes path from host",
			address:  "http://localhost:6443/livez",
			wantHost: "localhost",
			wantPort: 6443,
		},
		{
			name:     "IPv6 URL host and port",
			address:  "https://[::1]:6443",
			wantHost: "::1",
			wantPort: 6443,
		},
		{
			name:    "invalid URL",
			address: "://not-a-url",
			wantErr: true,
		},
		{
			name:    "port out of range",
			address: "localhost:70000",
			wantErr: true,
		},
		{
			name:     "bracketed IPv6 literal",
			address:  "[::1]",
			wantHost: "::1",
		},
		{
			name:     "IPv4 host and port",
			address:  "127.0.0.1:6443",
			wantHost: "127.0.0.1",
			wantPort: 6443,
		},
		{
			name:     "bracketed IPv6 host and port",
			address:  "[::1]:6443",
			wantHost: "::1",
			wantPort: 6443,
		},
		{
			name:     "bare hostname",
			address:  "localhost",
			wantHost: "localhost",
		},
		{
			name:     "FQDN host and port",
			address:  "kube-api.example.com:6443",
			wantHost: "kube-api.example.com",
			wantPort: 6443,
		},
		{
			name:     "hostname starting with digits and host and port",
			address:  "10-host:6443",
			wantHost: "10-host",
			wantPort: 6443,
		},
		{
			name:     "URL with IPv4 host and port",
			address:  "https://127.0.0.1:6443",
			wantHost: "127.0.0.1",
			wantPort: 6443,
		},
		{
			name:     "URL with hostname and no port",
			address:  "https://localhost",
			wantHost: "localhost",
		},
		{
			name:     "URL with trailing slash and path dropped",
			address:  "https://k8s.local:6443/",
			wantHost: "k8s.local",
			wantPort: 6443,
		},
		{
			name:    "URL with empty host",
			address: "https://",
			wantErr: true,
		},
		{
			name:    "URL with path but no host",
			address: "https:///foo",
			wantErr: true,
		},
		{
			name:    "zoned IPv6 literal",
			address: "fe80::1%eth0",
			wantErr: true,
		},
		{
			name:    "URL port out of range",
			address: "https://127.0.0.1:99999",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := processAddress(tt.address)
			if (err != nil) != tt.wantErr {
				t.Fatalf("processAddress() error = %v, wantErr %t", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if got.host != tt.wantHost || got.port != tt.wantPort {
				t.Errorf("processAddress() = {host:%q port:%d}, want {host:%q port:%d}",
					got.host, got.port, tt.wantHost, tt.wantPort)
			}
		})
	}
}

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
			cfg := &Config{
				Type:           HTTP,
				Address:        tc.addr,
				Port:           tc.port,
				KubeConfigPath: "/fake/path",
				Client:         &http.Client{},
			}
			if tc.nilClient {
				cfg.Client = nil
			}
			entry, err := New(cfg)
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

func TestHTTPBackendCheck(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		want       bool
		wantErr    bool
	}{
		{name: "healthy status", statusCode: http.StatusOK, want: true},
		{name: "unhealthy status", statusCode: http.StatusServiceUnavailable, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requestPath := ""
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requestPath = r.URL.Path
				w.WriteHeader(tt.statusCode)
			}))
			t.Cleanup(server.Close)

			entry, err := New(&Config{
				Type:    HTTP,
				Address: server.URL + "/livez",
				Client:  server.Client(),
			})
			if err != nil {
				t.Fatalf("creating backend: %v", err)
			}

			got, err := entry.Check(context.Background())
			if got != tt.want {
				t.Errorf("Check() = %t, want %t", got, tt.want)
			}
			if (err != nil) != tt.wantErr {
				t.Errorf("Check() error = %v, want error = %t", err, tt.wantErr)
			}
			if requestPath != "/livez" {
				t.Errorf("request path = %q, want %q", requestPath, "/livez")
			}
		})
	}
}

func TestBackendMapFindUsesCanonicalIdentity(t *testing.T) {
	first, err := New(&Config{
		Type:    Discovery,
		Address: "::1",
		Port:    6443,
	})
	if err != nil {
		t.Fatalf("creating first backend: %v", err)
	}
	backends := Map{}
	backends.Add(&Entry{Backend: first})
	if got := backends.Find(Discovery, "[::1]", 6443); got == nil || got.Backend != first {
		t.Fatalf("Find() = %v, want the existing backend", got)
	}
	if got := backends.Find(Discovery, "::1", 6444); got != nil {
		t.Errorf("Find() with a different port = %v, want nil", got)
	}
	if got := backends.Find(HTTP, "::1", 6443); got != nil {
		t.Errorf("Find() with a different backend type = %v, want nil", got)
	}
}

func TestBackendKeyPreservesHostnameAndURL(t *testing.T) {
	tests := []struct {
		name        string
		address     string
		wantAddress string
	}{
		{
			name:        "hostname",
			address:     "localhost",
			wantAddress: "localhost",
		},
		{
			name:        "URL with path",
			address:     "https://localhost:6443/livez",
			wantAddress: "https://localhost:6443/livez",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key := NewKey(HTTP, tt.address, 0)
			if key.Address != tt.wantAddress {
				t.Errorf("key address = %q, want %q", key.Address, tt.wantAddress)
			}
		})
	}
}
