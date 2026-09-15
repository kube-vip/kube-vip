package kubevip

import (
	"testing"

	"github.com/kube-vip/kube-vip/pkg/metrics"
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

func TestParseEnvironmentEnablePprof(t *testing.T) {
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
				t.Setenv(enablePprof, tc.value)
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
			if c.EnablePprof != tc.want {
				t.Fatalf("EnablePprof = %v, want %v", c.EnablePprof, tc.want)
			}
		})
	}
}

func TestParseEnvironmentPprofServer(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  string
	}{
		{name: "unset keeps default value", value: "", want: metrics.DefaultPprofHTTPServer},
		{name: "true configures the value as defined", value: "127.0.0.1:7070", want: "127.0.0.1:7070"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.value != "" {
				t.Setenv(pprofServer, tc.value)
			}
			// The flag default is already in the config by the time the
			// environment is parsed, so start from it.
			c := &Config{PprofHTTPServer: metrics.DefaultPprofHTTPServer}
			if err := ParseEnvironment(c); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if c.PprofHTTPServer != tc.want {
				t.Fatalf("PprofHTTPServer = %q, want %q", c.PprofHTTPServer, tc.want)
			}
		})
	}
}
