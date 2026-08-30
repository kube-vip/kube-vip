package networkinterface_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/kube-vip/kube-vip/pkg/arp"
	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/networkinterface"
	"github.com/kube-vip/kube-vip/pkg/node/noop"
	"github.com/kube-vip/kube-vip/pkg/route"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestManagerReconstructsProductionInstance(t *testing.T) {
	config := &kubevip.Config{Interface: "lo", ServicesInterface: "lo", VIPSubnet: "32", DisableServiceUpdates: true}
	manager := networkinterface.NewManager()
	service := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: "default", UID: "service"},
		Spec:       v1.ServiceSpec{LoadBalancerIP: "192.0.2.10"},
	}

	start := make(chan struct{})
	ready := sync.WaitGroup{}
	ready.Add(2)
	results := make(chan struct {
		instance *instance.Instance
		err      error
	}, 2)
	for range 2 {
		go func() {
			ready.Done()
			<-start
			instanceConfig := *config
			inst, err := instance.NewInstance(context.Background(), service.DeepCopy(), &instanceConfig, manager, arp.NewManager(&instanceConfig), route.NewManager(), noop.NewManager(), &sync.WaitGroup{})
			results <- struct {
				instance *instance.Instance
				err      error
			}{inst, err}
		}()
	}
	ready.Wait()
	close(start)
	instances := make([]*instance.Instance, 0, 2)
	for range 2 {
		select {
		case result := <-results:
			if result.err != nil {
				t.Fatalf("NewInstance() error = %v", result.err)
			}
			instances = append(instances, result.instance)
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for concurrent NewInstance calls")
		}
	}
	first, second := instances[0], instances[1]
	if len(first.Clusters) != 1 || len(second.Clusters) != 1 {
		t.Fatalf("cluster counts = %d, %d, want 1 each", len(first.Clusters), len(second.Clusters))
	}
}
