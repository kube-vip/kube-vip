package endpoints

import (
	"context"
	"errors"
	"testing"

	"github.com/kube-vip/kube-vip/pkg/cluster"
	"github.com/kube-vip/kube-vip/pkg/endpoints/providers"
	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	"github.com/kube-vip/kube-vip/pkg/vip"
	"github.com/vishvananda/netlink"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestBGPWorkerRetriesAddAfterFailedWithdrawal(t *testing.T) {
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: "default", UID: "uid"}}
	network := &bgpTestNetwork{ip: "10.0.0.40", cidr: "10.0.0.40/32"}
	instances := []*instance.Instance{{
		ServiceSnapshot: service.DeepCopy(),
		Clusters:        []*cluster.Cluster{{Network: []vip.Network{network}}},
	}}
	manager := &recordingBGPRouteManager{}
	worker := &BGP{
		generic: generic{
			config:    &kubevip.Config{},
			provider:  providers.NewEndpointslices(),
			instances: &instances,
		},
		bgpServer: manager,
	}
	svcCtx := servicecontext.New(context.Background())

	if err := worker.processInstance(svcCtx, service); err != nil {
		t.Fatalf("initial processInstance: %v", err)
	}
	manager.delErr = errors.New("withdrawal failed")
	lastEndpoint := "10.0.0.2"
	worker.clear(svcCtx, &lastEndpoint, service)
	if svcCtx.IsNetworkConfigured(network.ip) {
		t.Fatal("network remained configured after failed withdrawal")
	}

	if err := worker.processInstance(svcCtx, service); err != nil {
		t.Fatalf("recovery processInstance: %v", err)
	}
	if manager.addCalls != 2 {
		t.Fatalf("AddHost calls = %d, want 2", manager.addCalls)
	}
}

type recordingBGPRouteManager struct {
	addCalls int
	delErr   error
}

func (m *recordingBGPRouteManager) AddHost(context.Context, string, string) error {
	m.addCalls++
	return nil
}

func (m *recordingBGPRouteManager) DelHost(context.Context, string, string) error {
	return m.delErr
}

type bgpTestNetwork struct {
	ip   string
	cidr string
}

func (n *bgpTestNetwork) AddIP(bool, bool, ...int) (bool, error) { return false, nil }
func (n *bgpTestNetwork) AddRoute(bool) (bool, error)            { return false, nil }
func (n *bgpTestNetwork) ReplaceRoute() error                    { return nil }
func (n *bgpTestNetwork) DeleteIP() (bool, error)                { return false, nil }
func (n *bgpTestNetwork) DeleteRoute() error                     { return nil }
func (n *bgpTestNetwork) UpdateRoutes() (bool, error)            { return false, nil }
func (n *bgpTestNetwork) IsSet() (*netlink.Addr, error)          { return nil, nil }
func (n *bgpTestNetwork) IP() string                             { return n.ip }
func (n *bgpTestNetwork) CIDR() string                           { return n.cidr }
func (n *bgpTestNetwork) IPisLinkLocal() bool                    { return false }
func (n *bgpTestNetwork) PrepareRoute() *netlink.Route           { return nil }
func (n *bgpTestNetwork) RouteHash() string                      { return "" }
func (n *bgpTestNetwork) SetIP(string) error                     { return nil }
func (n *bgpTestNetwork) SetServicePorts(*v1.Service)            {}
func (n *bgpTestNetwork) Interface() string                      { return "" }
func (n *bgpTestNetwork) IsDADFAIL() bool                        { return false }
func (n *bgpTestNetwork) IsDNS() bool                            { return false }
func (n *bgpTestNetwork) IsDDNS() bool                           { return false }
func (n *bgpTestNetwork) DDNSHostName() string                   { return "" }
func (n *bgpTestNetwork) DNSName() string                        { return "" }
func (n *bgpTestNetwork) SetMask(string) error                   { return nil }
func (n *bgpTestNetwork) SetHasEndpoints(bool)                   {}
func (n *bgpTestNetwork) HasEndpoints() bool                     { return false }
func (n *bgpTestNetwork) ARPName() string                        { return "" }
func (n *bgpTestNetwork) GetPossibleSubnets() string             { return "" }
func (n *bgpTestNetwork) DHCPFamily() string                     { return "" }
func (n *bgpTestNetwork) IPVSMark() uint32                       { return 0 }
