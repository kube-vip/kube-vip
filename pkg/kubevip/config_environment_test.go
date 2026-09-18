package kubevip

import (
	"testing"
)

func TestParseEnvironmentSkipDAD(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		want    bool
		wantErr bool
	}{
		{name: "unset keeps default false", value: "", want: false},
		{name: "true enables", value: "true", want: true},
		{name: "false disables", value: "false", want: false},
		{name: "garbage errors", value: "not-a-bool", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.value != "" {
				t.Setenv(vipSkipDAD, tc.value)
			}
			c := &Config{}
			err := ParseEnvironment(c)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if c.SkipDAD != tc.want {
				t.Fatalf("SkipDAD = %v, want %v", c.SkipDAD, tc.want)
			}
		})
	}
}

func TestParseEnvironmentControlPlaneHealthCheckClientCertificate(t *testing.T) {
	t.Setenv(controlPlaneHealthCheckClientCertPath, "/tls/client.crt")
	t.Setenv(controlPlaneHealthCheckClientKeyPath, "/tls/client.key")

	config := &Config{}
	if err := ParseEnvironment(config); err != nil {
		t.Fatalf("ParseEnvironment() error = %v", err)
	}
	if config.ControlPlaneHealthCheck.ClientCertPath != "/tls/client.crt" {
		t.Fatalf("ClientCertPath = %q", config.ControlPlaneHealthCheck.ClientCertPath)
	}
	if config.ControlPlaneHealthCheck.ClientKeyPath != "/tls/client.key" {
		t.Fatalf("ClientKeyPath = %q", config.ControlPlaneHealthCheck.ClientKeyPath)
	}
}
