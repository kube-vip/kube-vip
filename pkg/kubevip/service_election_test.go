package kubevip

import (
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestUsesServiceElection(t *testing.T) {
	t.Parallel()

	forced := &v1.Service{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{ForcePerServiceElection: "true"}}}
	plain := &v1.Service{}
	tests := []struct {
		name    string
		config  Config
		service *v1.Service
		want    bool
	}{
		{name: "svc_election covers every Service", config: Config{EnableServicesElection: true}, service: plain, want: true},
		{name: "on demand covers an annotated Service", config: Config{PerServiceElectionOnDemand: true}, service: forced, want: true},
		{name: "on demand skips an unannotated Service", config: Config{PerServiceElectionOnDemand: true}, service: plain},
		{name: "annotation alone is not election", config: Config{}, service: forced},
		{name: "nil Service", config: Config{PerServiceElectionOnDemand: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := UsesServiceElection(&tt.config, tt.service); got != tt.want {
				t.Fatalf("UsesServiceElection() = %t, want %t", got, tt.want)
			}
		})
	}
}
