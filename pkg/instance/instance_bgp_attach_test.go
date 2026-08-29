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

func TestNewInstance_PropagatesBGPAttachIPToInterface(t *testing.T) {
	tests := []struct {
		name   string
		attach bool
	}{
		{name: "attach enabled is propagated", attach: true},
		{name: "attach disabled is propagated", attach: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			globalConfig := &kubevip.Config{
				Interface:              "lo",
				VIPSubnet:              "32",
				EnableBGP:              true,
				BGPAttachIPToInterface: tt.attach,
			}

			svc := &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-svc",
					Namespace: "default",
					Annotations: map[string]string{
						kubevip.LoadbalancerIPAnnotation: "10.0.1.2",
					},
				},
			}

			inst, err := instance.NewInstance(context.Background(), svc, globalConfig,
				networkinterface.NewManager(), arp.NewManager(globalConfig), route.NewManager(),
				nil, &sync.WaitGroup{})
			if err != nil {
				t.Fatalf("NewInstance() error = %v", err)
			}

			if len(inst.VIPConfigs) != 1 {
				t.Fatalf("VIPConfigs len = %d, want 1", len(inst.VIPConfigs))
			}

			if got := inst.VIPConfigs[0].BGPAttachIPToInterface; got != tt.attach {
				t.Fatalf("BGPAttachIPToInterface = %t, want %t", got, tt.attach)
			}
		})
	}
}
