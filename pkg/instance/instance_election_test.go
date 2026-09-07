package instance_test

import (
	"context"
	"sync"
	"testing"

	"github.com/kube-vip/kube-vip/pkg/arp"
	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/networkinterface"
	"github.com/kube-vip/kube-vip/pkg/route"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestNewInstanceUsesEffectiveServiceElection(t *testing.T) {
	for _, test := range []struct {
		name        string
		config      *kubevip.Config
		annotations map[string]string
		address     string
		bgp         bool
		routing     bool
		want        bool
	}{
		{name: "global IP", config: &kubevip.Config{EnableServicesElection: true}, address: "192.0.2.10", want: true},
		{name: "forced on-demand BGP IP", config: &kubevip.Config{PerServiceElectionOnDemand: true}, annotations: map[string]string{kubevip.ForcePerServiceElection: "true"}, address: "192.0.2.10", bgp: true, want: true},
		{name: "forced on-demand routing-table hostname", config: &kubevip.Config{PerServiceElectionOnDemand: true}, annotations: map[string]string{kubevip.ForcePerServiceElection: "true"}, address: "vip.example.test", routing: true, want: true},
		{name: "ordinary mixed-mode service", config: &kubevip.Config{PerServiceElectionOnDemand: true}, address: "192.0.2.10"},
		{name: "annotation value is exact", config: &kubevip.Config{PerServiceElectionOnDemand: true}, annotations: map[string]string{kubevip.ForcePerServiceElection: "True"}, address: "192.0.2.10"},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.config.Interface = "lo"
			test.config.VIPSubnet = "32"
			test.config.EnableBGP = test.bgp
			test.config.EnableRoutingTable = test.routing
			annotations := test.annotations
			if annotations == nil {
				annotations = make(map[string]string)
			}
			annotations[kubevip.LoadbalancerIPAnnotation] = test.address
			service := &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: "default", UID: "service", Annotations: annotations}}

			inst, err := instance.NewInstance(context.Background(), service, test.config,
				networkinterface.NewManager(), arp.NewManager(test.config), route.NewManager(), nil, &sync.WaitGroup{})
			if err != nil {
				t.Fatalf("NewInstance() error = %v", err)
			}
			if len(inst.VIPConfigs) != 1 {
				t.Fatalf("VIPConfigs = %d, want 1", len(inst.VIPConfigs))
			}
			if got := inst.VIPConfigs[0].EnableServicesElection; got != test.want {
				t.Fatalf("EnableServicesElection = %t, want %t", got, test.want)
			}
			if inst.VIPConfigs[0].EnableBGP != test.bgp || inst.VIPConfigs[0].EnableRoutingTable != test.routing {
				t.Fatalf("winner config BGP/RT = %t/%t, want %t/%t", inst.VIPConfigs[0].EnableBGP,
					inst.VIPConfigs[0].EnableRoutingTable, test.bgp, test.routing)
			}
		})
	}
}
