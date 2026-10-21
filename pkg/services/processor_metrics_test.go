package services

import (
	"fmt"
	"sync"
	"testing"

	"github.com/kube-vip/kube-vip/pkg/metrics"
	"github.com/prometheus/client_golang/prometheus/testutil"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestActiveServicesMetricTracksUIDReplacement(t *testing.T) {
	p := &Processor{}
	service := func(name string, uid types.UID) *v1.Service {
		return &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: t.Name(), UID: uid}}
	}

	stable := service("stable", "stable")
	p.trackService(stable)
	old := service("churn", "churn-0")
	p.trackService(old)

	for i := 1; i <= 600; i++ {
		replacement := service("churn", types.UID(fmt.Sprintf("churn-%d", i)))
		p.trackService(replacement)
		p.untrackService(old)
		old = replacement
	}

	if got := testutil.ToFloat64(metrics.ActiveServices.WithLabelValues(t.Name())); got != 2 {
		t.Fatalf("active services = %v, want 2", got)
	}
}

func TestActiveServicesMetricIgnoresStaleDeleteAfterRecreate(t *testing.T) {
	p := &Processor{}
	old := &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: t.Name(), UID: "old"}}
	replacement := old.DeepCopy()
	replacement.UID = "replacement"

	p.trackService(old)
	p.trackService(replacement)
	if got := testutil.ToFloat64(metrics.ActiveServices.WithLabelValues(t.Name())); got != 1 {
		t.Fatalf("active services after replacement = %v, want 1", got)
	}
	p.untrackService(old)
	p.untrackService(old)

	if got := testutil.ToFloat64(metrics.ActiveServices.WithLabelValues(t.Name())); got != 1 {
		t.Fatalf("active services = %v, want 1", got)
	}
}

func TestActiveServicesMetricConcurrentChurn(t *testing.T) {
	p := &Processor{}
	const workers = 16
	start := make(chan struct{})
	var wg sync.WaitGroup

	for worker := range workers {
		wg.Go(func() {
			<-start
			for generation := range 100 {
				uid := types.UID(fmt.Sprintf("%d-%d", worker, generation))
				service := &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: string(uid), Namespace: t.Name(), UID: uid}}
				p.trackService(service)
				p.untrackService(service)
			}
		})
	}

	close(start)
	wg.Wait()
	if got := testutil.ToFloat64(metrics.ActiveServices.WithLabelValues(t.Name())); got != 0 {
		t.Fatalf("active services = %v, want 0", got)
	}
}
