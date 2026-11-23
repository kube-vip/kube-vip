package instance_test

import (
	"context"
	"sync"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kube-vip/kube-vip/pkg/arp"
	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/networkinterface"
	"github.com/kube-vip/kube-vip/pkg/route"
)

// TestNewInstanceServiceElectionPerService asserts that a Service under
// per-service election on demand gets EnableServicesElection on its VIP
// configs, so the election winner advertises the route itself when it starts
// leading (StartLoadBalancerService), while other Services keep the global
// value.
func TestNewInstanceServiceElectionPerService(t *testing.T) {
	tests := []struct {
		name        string
		config      kubevip.Config
		annotations map[string]string
		want        bool
	}{
		{
			name:        "on demand, annotated",
			config:      kubevip.Config{PerServiceElectionOnDemand: true},
			annotations: map[string]string{kubevip.ForcePerServiceElection: "true"},
			want:        true,
		},
		{
			name:   "on demand, not annotated",
			config: kubevip.Config{PerServiceElectionOnDemand: true},
		},
		{
			name:   "svc_election",
			config: kubevip.Config{EnableServicesElection: true},
			want:   true,
		},
		{
			name:        "annotated without on demand",
			annotations: map[string]string{kubevip.ForcePerServiceElection: "true"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			globalConfig := tt.config
			globalConfig.Interface = "lo"
			globalConfig.VIPSubnet = "32"
			globalConfig.EnableBGP = true

			annotations := map[string]string{kubevip.LoadbalancerIPAnnotation: "10.0.1.2"}
			for k, v := range tt.annotations {
				annotations[k] = v
			}
			svc := &v1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "test-svc", Namespace: "default", Annotations: annotations},
			}

			inst, err := instance.NewInstance(context.Background(), svc, &globalConfig,
				networkinterface.NewManager(), arp.NewManager(&globalConfig), route.NewManager(),
				nil, &sync.WaitGroup{})
			if err != nil {
				t.Fatalf("NewInstance() error = %v", err)
			}
			if len(inst.VIPConfigs) != 1 {
				t.Fatalf("VIPConfigs len = %d, want 1", len(inst.VIPConfigs))
			}
			if got := inst.VIPConfigs[0].EnableServicesElection; got != tt.want {
				t.Fatalf("EnableServicesElection = %t, want %t", got, tt.want)
			}
		})
	}
}
