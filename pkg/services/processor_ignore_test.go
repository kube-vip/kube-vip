package services

import (
	"testing"

	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/lease"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
)

func TestReconcileIgnoredServiceStopsTrackedService(t *testing.T) {
	service := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "web", Namespace: "default", UID: "web",
			Annotations: map[string]string{kubevip.LoadbalancerIPAnnotation: "192.0.2.10"},
		},
		Spec: v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
	}
	processor := &Processor{
		config:      &kubevip.Config{EnableServicesElection: true},
		serviceLock: newTestServiceLocks(),
		ServiceInstances: []*instance.Instance{{
			ServiceUID: service.UID, ServiceSnapshot: service, AddCalled: true,
		}},
	}
	runner := &electionTestRunner{started: make(chan struct{})}
	initializeTestElectionCoordinators(processor, withTestCampaignRunner(runner))
	processor.refreshOwnedServiceVIPs()
	if got := processor.OwnedServiceVIPs(); len(got) != 1 || got[0] != "192.0.2.10" {
		t.Fatalf("owned VIPs before opt-out = %v, want [192.0.2.10]", got)
	}
	svcCtx := publishTestServiceContext(processor, service)
	svcCtx.SignalReadiness()
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := processor.StartServicesLeaderElection(svcCtx, service, nil); err != nil {
			t.Errorf("StartServicesLeaderElection() error = %v", err)
		}
	}()
	t.Cleanup(func() {
		svcCtx.Cancel()
		<-done
		processor.WaitForElectionCampaigns()
	})
	waitForElectionRunner(t, runner.started)

	ignored := service.DeepCopy()
	ignored.Annotations[kubevip.LoadbalancerIgnore] = "true"
	if err := processor.Reconcile(t.Context(), watch.Event{Type: watch.Modified, Object: ignored}, nil, false, nil, nil); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	if svcCtx.Ctx.Err() == nil {
		t.Fatal("opt-out left the tracked Service context running")
	}
	if _, ok := processor.svcMap.Load(service.UID); ok {
		t.Fatal("opt-out left the Service context tracked")
	}
	if processor.findServiceInstance(service) != nil || len(processor.OwnedServiceVIPs()) != 0 {
		t.Fatal("opt-out left the Service instance or its VIP tracked")
	}
	namespace, name := lease.ServiceName(service)
	if processor.leaseMgr.Get(lease.NewID(processor.config.LeaderElectionType, namespace, name)) != nil {
		t.Fatal("opt-out left the Service lease active")
	}
}

func TestReconcileIgnoredServiceSkipsUntrackedService(t *testing.T) {
	service := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "web", Namespace: "default", UID: "web",
			Annotations: map[string]string{kubevip.LoadbalancerIgnore: "true"},
		},
		Spec: v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
	}
	processor := &Processor{serviceLock: newTestServiceLocks(), config: &kubevip.Config{}}
	initializeTestElectionCoordinators(processor)
	if err := processor.Reconcile(t.Context(), watch.Event{Type: watch.Added, Object: service}, nil, false, nil, nil); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if _, ok := processor.svcMap.Load(service.UID); ok || processor.findServiceInstance(service) != nil {
		t.Fatal("ignored Service was tracked")
	}
}
