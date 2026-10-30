package backend

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBackendFamily(t *testing.T) {
	tests := []struct {
		name        string
		address     string
		backendType BackendType
		wantFamily  AddressFamily
	}{
		{
			name:        "hostname URL",
			address:     "https://localhost:6443/livez",
			backendType: HTTP,
			wantFamily:  AddressFamilyHostname,
		},
		{
			name:        "IPv4 URL",
			address:     "https://127.0.0.1:6443/livez",
			backendType: HTTP,
			wantFamily:  AddressFamilyIPv4,
		},
		{
			name:        "IPv6 URL",
			address:     "https://[::1]:6443/livez",
			backendType: HTTP,
			wantFamily:  AddressFamilyIPv6,
		},
		{
			name:        "discovery IPv4",
			address:     "127.0.0.1",
			backendType: Discovery,
			wantFamily:  AddressFamilyIPv4,
		},
		{
			name:        "discovery IPv6",
			address:     "::1",
			backendType: Discovery,
			wantFamily:  AddressFamilyIPv6,
		},
		{
			name:        "discovery hostname",
			address:     "localhost",
			backendType: Discovery,
			wantFamily:  AddressFamilyHostname,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry, err := New(&Config{
				Type:    tt.backendType,
				Address: tt.address,
				Client:  &http.Client{},
			})
			if err != nil {
				t.Fatalf("creating backend: %v", err)
			}
			if got := entry.Family(); got != tt.wantFamily {
				t.Errorf("Family() = %v, want %v", got, tt.wantFamily)
			}
		})
	}
}

func TestAddressFamilyUnknown(t *testing.T) {
	if got := addressFamily(""); got != AddressFamilyUnknown {
		t.Errorf("addressFamily(empty) = %v, want %v", got, AddressFamilyUnknown)
	}
}

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
			wantAddress: "[::1]",
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
			wantAddress: "[::1]",
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

			err := processConfig(config)
			if (err != nil) != tt.wantErr {
				t.Fatalf("processConfig() error = %v, wantErr %t", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if config.Address != tt.wantAddress {
				t.Errorf("Address = %q, want %q", config.Address, tt.wantAddress)
			}
			if config.Port != tt.wantPort {
				t.Errorf("Port = %d, want %d", config.Port, tt.wantPort)
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

func TestHTTPBackendCheck(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		want       bool
	}{
		{name: "healthy status", statusCode: http.StatusOK, want: true},
		{name: "unhealthy status", statusCode: http.StatusServiceUnavailable},
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

			if got := entry.Check(context.Background()); got != tt.want {
				t.Errorf("Check() = %t, want %t", got, tt.want)
			}
			if requestPath != "/livez" {
				t.Errorf("request path = %q, want %q", requestPath, "/livez")
			}
		})
	}
}
