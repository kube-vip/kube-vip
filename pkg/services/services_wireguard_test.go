package services

import (
	"context"
	"sync"
	"testing"

	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/node/noop"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	"github.com/kube-vip/kube-vip/pkg/wireguard"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

type recordingServiceTunnelManager struct {
	acquired []string
	released []string
}

func (m *recordingServiceTunnelManager) GetConfigForVIP(vip string) *wireguard.TunnelConfig {
	return &wireguard.TunnelConfig{VIP: vip, InterfaceName: "wg-test"}
}

func (m *recordingServiceTunnelManager) HasConfigForVIP(string) bool { return true }

func (m *recordingServiceTunnelManager) AcquireTunnelForVIP(vip, owner string) error {
	m.acquired = append(m.acquired, vip+":"+owner)
	return nil
}

func (m *recordingServiceTunnelManager) ReleaseTunnelForVIP(vip, owner string) error {
	m.released = append(m.released, vip+":"+owner)
	return nil
}

func TestConfigureAndCleanupServiceOwnWireguardTunnelForActiveLifecycle(t *testing.T) {
	tunnelManager := &recordingServiceTunnelManager{}
	service := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "service",
			UID:       types.UID("service-uid"),
		},
		Spec: v1.ServiceSpec{LoadBalancerIP: "192.0.2.10"},
	}
	svcCtx := servicecontext.New(context.Background())
	serviceInstance := &instance.Instance{
		ServiceUID:      service.UID,
		ServiceSnapshot: service.DeepCopy(),
		AddCalled:       true,
	}
	processor := &Processor{
		config: &kubevip.Config{
			EnableWireguard:        true,
			EnableServicesElection: true,
			DisableServiceUpdates:  true,
		},
		serviceLock:      newTestServiceLocks(),
		nodeLabelManager: noop.NewManager(),
		TunnelMgr:        tunnelManager,
		ServiceInstances: []*instance.Instance{serviceInstance},
	}
	processor.svcMap.Store(service.UID, svcCtx)

	if err := processor.configureService(context.Background(), svcCtx, serviceInstance, service, &sync.WaitGroup{}); err != nil {
		t.Fatalf("configureService() error = %v", err)
	}
	if len(tunnelManager.acquired) != 1 || tunnelManager.acquired[0] != "192.0.2.10:service-uid" {
		t.Fatalf("tunnel acquisitions = %v, want [192.0.2.10:service-uid]", tunnelManager.acquired)
	}

	if err := processor.deleteServiceInstance(context.Background(), serviceInstance); err != nil {
		t.Fatalf("deleteServiceInstance() error = %v", err)
	}
	if len(tunnelManager.released) != 1 || tunnelManager.released[0] != "192.0.2.10:service-uid" {
		t.Fatalf("tunnel releases = %v, want [192.0.2.10:service-uid]", tunnelManager.released)
	}
}
