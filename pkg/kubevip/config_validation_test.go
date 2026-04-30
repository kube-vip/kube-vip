package kubevip

import (
	"testing"
)

func TestValidate_HealthCheckAddress(t *testing.T) {
	tests := []struct {
		name    string
		address string
		wantErr bool
	}{
		{"empty address (disabled)", "", false},
		{"valid http URL", "http://localhost:6443/livez", false},
		{"valid https URL", "https://localhost:6443/livez", false},
		{"https with path", "https://127.0.0.1:6443/readyz?verbose", false},
		{"invalid URL", "not-a-url", true},
		{"ftp scheme", "ftp://localhost/file", true},
		{"tcp scheme", "tcp://localhost:6443", true},
		{"missing scheme", "localhost:6443/livez", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Config{ControlPlaneHealthCheck: HealthCheck{Address: tt.address}}
			err := c.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
