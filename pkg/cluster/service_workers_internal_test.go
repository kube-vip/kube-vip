package cluster

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/kube-vip/kube-vip/pkg/arp"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/route"
	"github.com/kube-vip/kube-vip/pkg/vip"
	"github.com/vishvananda/netlink"
	v1 "k8s.io/api/core/v1"
)

func TestStartLoadBalancerServiceSetMaskFailureCompletesGeneration(t *testing.T) {
	network := &workerTestNetwork{ip: "192.0.2.10", setMaskErr: errors.New("set mask")}
	c := &Cluster{stop: make(chan struct{}), Network: []vip.Network{network}}
	err := c.StartLoadBalancerService(context.Background(), &kubevip.Config{VIPSubnet: "32"}, nil, "service", &sync.WaitGroup{})
	if err == nil {
		t.Fatal("StartLoadBalancerService error = nil, want SetMask failure")
	}

	done := make(chan struct{})
	go func() {
		c.StopWorkersAndWaitPreserving(nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("StopWorkersAndWait blocked after partial startup")
	}
}

type workerTestNetwork struct {
	mu            sync.Mutex
	ip            string
	setMaskErr    error
	isSetErr      error
	addIPErr      error
	existing      bool
	addIPCalls    int
	deleteIPCalls int
	addRouteCalls int
	deleteRoutes  int
}

func (n *workerTestNetwork) AddIP(bool, bool, ...int) (bool, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.addIPCalls++
	added := !n.existing
	n.existing = true
	return added, n.addIPErr
}
func (n *workerTestNetwork) AddRoute(bool) (bool, error) {
	n.mu.Lock()
	n.addRouteCalls++
	n.mu.Unlock()
	return true, nil
}
func (n *workerTestNetwork) ReplaceRoute() error { return nil }
func (n *workerTestNetwork) DeleteIP() (bool, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.deleteIPCalls++
	deleted := n.existing
	n.existing = false
	return deleted, nil
}
func (n *workerTestNetwork) DeleteRoute() error {
	n.mu.Lock()
	n.deleteRoutes++
	n.mu.Unlock()
	return nil
}
func (n *workerTestNetwork) UpdateRoutes() (bool, error) { return false, nil }
func (n *workerTestNetwork) IsSet() (*netlink.Addr, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.isSetErr != nil {
		return nil, n.isSetErr
	}
	if n.existing {
		return &netlink.Addr{}, nil
	}
	return nil, nil
}
func (n *workerTestNetwork) IP() string                   { return n.ip }
func (n *workerTestNetwork) CIDR() string                 { return n.ip + "/32" }
func (n *workerTestNetwork) IPisLinkLocal() bool          { return false }
func (n *workerTestNetwork) PrepareRoute() *netlink.Route { return nil }
func (n *workerTestNetwork) RouteHash() string            { return "" }
func (n *workerTestNetwork) SetIP(ip string) error        { n.ip = ip; return nil }
func (n *workerTestNetwork) SetServicePorts(*v1.Service)  {}
func (n *workerTestNetwork) Interface() string            { return "lo" }
func (n *workerTestNetwork) IsDADFAIL() bool              { return false }
func (n *workerTestNetwork) IsDNS() bool                  { return false }
func (n *workerTestNetwork) IsDDNS() bool                 { return false }
func (n *workerTestNetwork) DDNSHostName() string         { return "" }
func (n *workerTestNetwork) DNSName() string              { return "" }
func (n *workerTestNetwork) SetMask(string) error         { return n.setMaskErr }
func (n *workerTestNetwork) SetHasEndpoints(bool)         {}
func (n *workerTestNetwork) HasEndpoints() bool           { return false }
func (n *workerTestNetwork) ARPName() string              { return "" }
func (n *workerTestNetwork) GetPossibleSubnets() string   { return "" }
func (n *workerTestNetwork) DHCPFamily() string           { return "" }
func (n *workerTestNetwork) IPVSMark() uint32             { return 0 }

func TestStartLoadBalancerServiceRollsBackEarlierNetwork(t *testing.T) {
	first := &workerTestNetwork{ip: "192.0.2.10"}
	second := &workerTestNetwork{ip: "192.0.2.11", setMaskErr: errors.New("set mask")}
	c := &Cluster{stop: make(chan struct{}), Network: []vip.Network{first, second}}

	if err := c.StartLoadBalancerService(context.Background(), &kubevip.Config{VIPSubnet: "32"}, nil, "service", &sync.WaitGroup{}); err == nil {
		t.Fatal("StartLoadBalancerService error = nil, want second network failure")
	}
	first.mu.Lock()
	addCalls, deleteCalls := first.addIPCalls, first.deleteIPCalls
	first.mu.Unlock()
	if addCalls != 1 || deleteCalls != 1 {
		t.Fatalf("first network AddIP/DeleteIP calls = %d/%d, want 1/1", addCalls, deleteCalls)
	}
	if c.WorkersRunning() {
		t.Fatal("partial startup left a worker generation running")
	}
}

func TestStartLoadBalancerServiceRollsBackPartialAddIP(t *testing.T) {
	first := &workerTestNetwork{ip: "192.0.2.10", addIPErr: errors.New("configure firewall")}
	second := &workerTestNetwork{ip: "192.0.2.11", setMaskErr: errors.New("set mask")}
	c := &Cluster{stop: make(chan struct{}), Network: []vip.Network{first, second}}

	if err := c.StartLoadBalancerService(context.Background(), &kubevip.Config{VIPSubnet: "32"}, nil, "service", &sync.WaitGroup{}); err == nil {
		t.Fatal("StartLoadBalancerService error = nil, want second network failure")
	}
	first.mu.Lock()
	addCalls, deleteCalls, exists := first.addIPCalls, first.deleteIPCalls, first.existing
	first.mu.Unlock()
	if addCalls != 1 || deleteCalls != 1 || exists {
		t.Fatalf("partial AddIP rollback = add %d, delete %d, exists %t; want 1, 1, false", addCalls, deleteCalls, exists)
	}
}

func TestStartLoadBalancerServiceRollsBackEarlierRoute(t *testing.T) {
	first := &workerTestNetwork{ip: "192.0.2.10"}
	second := &workerTestNetwork{ip: "192.0.2.11", setMaskErr: errors.New("set mask")}
	c := &Cluster{
		stop:     make(chan struct{}),
		Network:  []vip.Network{first, second},
		routeMgr: route.NewManager(),
	}
	config := &kubevip.Config{
		VIPSubnet:              "32",
		EnableRoutingTable:     true,
		EnableServicesElection: true,
	}

	if err := c.StartLoadBalancerService(context.Background(), config, nil, "service", &sync.WaitGroup{}); err == nil {
		t.Fatal("StartLoadBalancerService error = nil, want second network failure")
	}
	first.mu.Lock()
	addCalls, deleteCalls := first.addRouteCalls, first.deleteRoutes
	first.mu.Unlock()
	if addCalls != 1 || deleteCalls != 1 {
		t.Fatalf("first network AddRoute/DeleteRoute calls = %d/%d, want 1/1", addCalls, deleteCalls)
	}
}

func TestStartLoadBalancerServiceRollbackPreservesExistingSharedVIP(t *testing.T) {
	first := &workerTestNetwork{ip: "192.0.2.10", existing: true}
	second := &workerTestNetwork{ip: "192.0.2.11", setMaskErr: errors.New("set mask")}
	c := &Cluster{stop: make(chan struct{}), Network: []vip.Network{first, second}}

	if err := c.StartLoadBalancerService(context.Background(), &kubevip.Config{VIPSubnet: "32"}, nil, "service", &sync.WaitGroup{}); err == nil {
		t.Fatal("StartLoadBalancerService error = nil, want second network failure")
	}
	first.mu.Lock()
	addCalls, deleteCalls, exists := first.addIPCalls, first.deleteIPCalls, first.existing
	first.mu.Unlock()
	if addCalls != 1 || deleteCalls != 0 || !exists {
		t.Fatalf("existing shared VIP state after rollback = add %d, delete %d, exists %t; want 1, 0, true", addCalls, deleteCalls, exists)
	}
}

func TestStartLoadBalancerServiceRollbackPreservesARPSiblingVIP(t *testing.T) {
	first := &workerTestNetwork{ip: "192.0.2.10"}
	second := &workerTestNetwork{ip: "192.0.2.11", setMaskErr: errors.New("set mask")}
	config := &kubevip.Config{VIPSubnet: "32", EnableARP: true, ArpBroadcastRate: 3000}
	arpMgr := arp.NewManager(config)
	arpMgr.Insert(arp.NewInstance(first, nil))
	c := &Cluster{stop: make(chan struct{}), Network: []vip.Network{first, second}, arpMgr: arpMgr}

	if err := c.StartLoadBalancerService(context.Background(), config, nil, "service", &sync.WaitGroup{}); err == nil {
		t.Fatal("StartLoadBalancerService error = nil, want second network failure")
	}
	first.mu.Lock()
	deleteCalls := first.deleteIPCalls
	first.mu.Unlock()
	if deleteCalls != 0 {
		t.Fatalf("shared ARP VIP DeleteIP calls = %d, want 0", deleteCalls)
	}
}

func TestStartLoadBalancerServiceCancelledBeforeStartupCompletesGeneration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := &Cluster{stop: make(chan struct{}), Network: []vip.Network{&workerTestNetwork{ip: "192.0.2.10"}}}

	if err := c.StartLoadBalancerService(ctx, &kubevip.Config{VIPSubnet: "32"}, nil, "service", &sync.WaitGroup{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("StartLoadBalancerService error = %v, want context.Canceled", err)
	}
	if c.WorkersRunning() {
		t.Fatal("cancelled startup left a worker generation running")
	}
}
