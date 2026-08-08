package cluster

import (
	"testing"
)

func TestKubernetesAddrBackendEntry(t *testing.T) {
	cases := []struct {
		name     string
		addr     string
		port     uint16
		wantAddr string
		wantPort uint16
		wantNil  bool
	}{
		{
			name:     "explicit v4 loopback with port",
			addr:     "https://127.0.0.1:6443",
			port:     9999,
			wantAddr: "127.0.0.1",
			wantPort: 6443,
		},
		{
			name:     "hostname without port falls back to config port",
			addr:     "https://localhost",
			port:     6443,
			wantAddr: "localhost",
			wantPort: 6443,
		},
		{
			name:    "empty override",
			addr:    "",
			port:    6443,
			wantNil: true,
		},
		{
			name:    "garbage override",
			addr:    "://not-a-url",
			port:    6443,
			wantNil: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entry := kubernetesAddrBackendEntry(tc.addr, tc.port)
			if tc.wantNil {
				if entry != nil {
					t.Fatalf("expected nil entry, got %+v", entry)
				}
				return
			}
			if entry == nil {
				t.Fatal("expected an entry, got nil")
			}
			if entry.Addr != tc.wantAddr || entry.Port != tc.wantPort {
				t.Fatalf("got %+v, want addr %q port %d", entry, tc.wantAddr, tc.wantPort)
			}
		})
	}
}
