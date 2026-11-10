package kubevip

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kube-vip/kube-vip/pkg/metrics"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
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

func TestValidate_Pprof(t *testing.T) {
	tests := []struct {
		name        string
		enablePprof bool
		addr        string
		wantErr     bool
	}{
		{name: "disabled without address"},
		{name: "disabled with address", addr: "127.0.0.1:6060"},
		{name: "enabled with address", enablePprof: true, addr: "127.0.0.1:6060"},
		{name: "enabled with default address", enablePprof: true, addr: metrics.DefaultPprofHTTPServer},
		{name: "enabled without address", enablePprof: true, wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := &Config{EnablePprof: test.enablePprof, PprofHTTPServer: test.addr}
			if err := config.Validate(); (err != nil) != test.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %t", err, test.wantErr)
			}
		})
	}
}

func TestValidate_InstanceName(t *testing.T) {
	tests := []struct {
		name         string
		instanceName string
		wantErr      bool
	}{
		{name: "empty uses legacy default", instanceName: "", wantErr: false},
		{name: "letters digits and separators", instanceName: "release_01.prod-a", wantErr: false},
		{name: "exact maximum length", instanceName: strings.Repeat("a", instanceNameMaxLength), wantErr: false},
		{name: "exceeds maximum length", instanceName: strings.Repeat("a", instanceNameMaxLength+1), wantErr: true},
		{name: "space", instanceName: "release a", wantErr: true},
		{name: "slash", instanceName: "namespace/release", wantErr: true},
		{name: "dollar sign", instanceName: "release$a", wantErr: true},
		{name: "at sign", instanceName: "release@a", wantErr: true},
		{name: "newline", instanceName: "release\na", wantErr: true},
		{name: "null byte", instanceName: "release\x00a", wantErr: true},
		{name: "unicode", instanceName: "rilascio-à", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := &Config{InstanceName: tt.instanceName}
			err := config.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %t", err, tt.wantErr)
			}
		})
	}
}

func TestValidate_RoutingProtocol(t *testing.T) {
	tests := []struct {
		name     string
		protocol int
		wantErr  bool
	}{
		{name: "unset", protocol: 0, wantErr: false},
		{name: "kube-vip default", protocol: 248, wantErr: false},
		{name: "maximum byte value", protocol: 255, wantErr: false},
		{name: "truncated on the wire", protocol: 256, wantErr: true},
		{name: "negative", protocol: -1, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := &Config{RoutingProtocol: tt.protocol}
			err := config.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %t", err, tt.wantErr)
			}
		})
	}
}

func TestValidate_LeaderElectionTimings(t *testing.T) {
	tests := []struct {
		name          string
		leaseDuration int
		renewDeadline int
		retryPeriod   int
		wantErr       string
	}{
		{name: "client-go defaults", leaseDuration: 15, renewDeadline: 10, retryPeriod: 2},
		{name: "e2e timings", leaseDuration: 5, renewDeadline: 3, retryPeriod: 1},
		{name: "smallest valid timings", leaseDuration: 3, renewDeadline: 2, retryPeriod: 1},
		{name: "all one second", leaseDuration: 1, renewDeadline: 1, retryPeriod: 1,
			wantErr: "vip_leaseduration (1) must be greater than vip_renewdeadline (1)"},
		{name: "lease duration below renew deadline", leaseDuration: 5, renewDeadline: 10, retryPeriod: 2,
			wantErr: "vip_leaseduration (5) must be greater than vip_renewdeadline (10)"},
		{name: "renew deadline equal to retry period", leaseDuration: 15, renewDeadline: 2, retryPeriod: 2,
			wantErr: "vip_renewdeadline (2) must be greater than vip_retryperiod (2) * 1.2"},
		{name: "renew deadline within jitter of retry period", leaseDuration: 15, renewDeadline: 6, retryPeriod: 5,
			wantErr: "vip_renewdeadline (6) must be greater than vip_retryperiod (5) * 1.2"},
		{name: "zero lease duration", leaseDuration: 0, renewDeadline: 10, retryPeriod: 2,
			wantErr: "vip_leaseduration is 0, must be at least 1 second"},
		{name: "zero renew deadline", leaseDuration: 15, renewDeadline: 0, retryPeriod: 2,
			wantErr: "vip_renewdeadline is 0, must be at least 1 second"},
		{name: "zero retry period", leaseDuration: 15, renewDeadline: 10, retryPeriod: 0,
			wantErr: "vip_retryperiod is 0, must be at least 1 second"},
		{name: "negative retry period", leaseDuration: 15, renewDeadline: 10, retryPeriod: -1,
			wantErr: "vip_retryperiod is -1, must be at least 1 second"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := &Config{KubernetesLeaderElection: KubernetesLeaderElection{
				EnableLeaderElection: true,
				LeaseDuration:        tt.leaseDuration,
				RenewDeadline:        tt.renewDeadline,
				RetryPeriod:          tt.retryPeriod,
			}}
			err := config.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v, want nil", err)
				}
				return
			}
			if err == nil || err.Error() != tt.wantErr {
				t.Fatalf("Validate() error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidate_LeaderElectionTimingsOnlyWhenKubernetesElectionMayRun(t *testing.T) {
	tests := []struct {
		name    string
		config  Config
		wantErr bool
	}{
		{name: "no election", config: Config{EnableControlPlane: true}},
		{name: "control plane leader election", config: Config{KubernetesLeaderElection: KubernetesLeaderElection{EnableLeaderElection: true}}, wantErr: true},
		{name: "service election", config: Config{EnableServicesElection: true}, wantErr: true},
		{name: "services", config: Config{EnableServices: true}, wantErr: true},
		{name: "explicit kubernetes backend", config: Config{EnableServices: true, LeaderElectionType: "kubernetes"}, wantErr: true},
		{name: "etcd backend", config: Config{EnableControlPlane: true, LeaderElectionType: "etcd",
			KubernetesLeaderElection: KubernetesLeaderElection{EnableLeaderElection: true}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The etcd e2e manifest uses these timings: etcd only uses the lease
			// duration, but the Kubernetes leader elector refuses them.
			tt.config.LeaseDuration = 2
			tt.config.RenewDeadline = 3
			tt.config.RetryPeriod = 1
			if err := tt.config.Validate(); (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %t", err, tt.wantErr)
			}
		})
	}
}

// TestValidate_LeaderElectionTimingsMatchClientGo makes sure every timing that
// passes validation is accepted by the client-go leader elector, which panics
// on the ones it refuses, and that every refused timing fails validation.
func TestValidate_LeaderElectionTimingsMatchClientGo(t *testing.T) {
	for leaseDuration := -1; leaseDuration <= 12; leaseDuration++ {
		for renewDeadline := -1; renewDeadline <= 12; renewDeadline++ {
			for retryPeriod := -1; retryPeriod <= 12; retryPeriod++ {
				validateErr := validateLeaderElectionTimings(leaseDuration, renewDeadline, retryPeriod)
				_, clientGoErr := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
					Lock:          &resourcelock.LeaseLock{LockConfig: resourcelock.ResourceLockConfig{Identity: "node"}},
					LeaseDuration: time.Duration(leaseDuration) * time.Second,
					RenewDeadline: time.Duration(renewDeadline) * time.Second,
					RetryPeriod:   time.Duration(retryPeriod) * time.Second,
					Callbacks: leaderelection.LeaderCallbacks{
						OnStartedLeading: func(context.Context) {},
						OnStoppedLeading: func() {},
					},
				})
				if (validateErr == nil) != (clientGoErr == nil) {
					t.Errorf("lease %d, renew %d, retry %d: validation error = %v, client-go error = %v",
						leaseDuration, renewDeadline, retryPeriod, validateErr, clientGoErr)
				}
			}
		}
	}
}

func TestInstanceNameLimitReservesNftablesPrefixAndFamilySuffix(t *testing.T) {
	name := strings.Repeat("a", instanceNameMaxLength)
	if got := len(egressNftablesTablePrefix + name + egressNftablesTableSuffix); got != nftablesNameMaxLength {
		t.Fatalf("family-specific table name length = %d, want %d", got, nftablesNameMaxLength)
	}
}
