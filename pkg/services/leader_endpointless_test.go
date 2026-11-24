package services

import (
	"testing"
	"time"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestEndpointlessServiceElection(t *testing.T) {
	tests := []struct {
		name        string
		policy      v1.ServiceExternalTrafficPolicy
		annotations map[string]string
		starts      bool
	}{
		{
			name:        "opted-in Cluster Service",
			policy:      v1.ServiceExternalTrafficPolicyTypeCluster,
			annotations: map[string]string{kubevip.AllowReconcileWithoutEndpoints: "true"},
			starts:      true,
		},
		{
			name:   "Cluster Service without opt-in",
			policy: v1.ServiceExternalTrafficPolicyTypeCluster,
		},
		{
			name:        "Local Service with opt-in",
			policy:      v1.ServiceExternalTrafficPolicyTypeLocal,
			annotations: map[string]string{kubevip.AllowReconcileWithoutEndpoints: "true"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := &electionTestRunner{started: make(chan struct{})}
			p := &Processor{
				serviceLock: newTestServiceLocks(),
				config:      &kubevip.Config{EnableServicesElection: true},
				leaseMgr:    lease.NewManager(),
			}
			initializeTestElectionCoordinators(p, withTestCampaignRunner(runner))
			service := &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name: "endpointless", Namespace: "default", UID: "endpointless", Annotations: test.annotations,
				},
				Spec: v1.ServiceSpec{
					Type: v1.ServiceTypeLoadBalancer, LoadBalancerIP: "192.0.2.10", ExternalTrafficPolicy: test.policy,
				},
			}
			svcCtx := servicecontext.New(t.Context())
			p.svcMap.Store(service.UID, svcCtx)
			stopped := make(chan error, 1)
			go func() { stopped <- p.StartServicesLeaderElection(svcCtx, service, nil) }()
			t.Cleanup(func() {
				svcCtx.Cancel()
				select {
				case err := <-stopped:
					if err != nil {
						t.Errorf("Service election watcher returned an error: %v", err)
					}
				case <-time.After(time.Second):
					t.Error("Service election watcher did not stop")
				}
				p.electionCoordinators.Wait()
			})

			waitForCondition(t, func() bool {
				_, started := p.electionLoops.Load(svcCtx)
				return started
			}, "Service election watcher")
			if !test.starts {
				select {
				case <-runner.started:
					t.Fatal("election started before observing endpoint readiness")
				case <-time.After(50 * time.Millisecond):
				}
				// Simulate the readiness signal that an endpoint event would supply.
				svcCtx.SignalReadiness()
			}
			waitForElectionRunner(t, runner.started)
		})
	}
}
