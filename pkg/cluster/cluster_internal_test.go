package cluster

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/kube-vip/kube-vip/pkg/arp"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/vishvananda/netlink"
	v1 "k8s.io/api/core/v1"
)

func TestControlPlaneElectionVIPsPreservesConfigOrder(t *testing.T) {
	config := &kubevip.Config{Address: "2001:db8::10,192.0.2.10"}
	want := []string{"2001:db8::10", "192.0.2.10"}
	if got := controlPlaneElectionVIPs(config); !slices.Equal(got, want) {
		t.Fatalf("controlPlaneElectionVIPs() = %v, want %v", got, want)
	}
}

type recordingLabeler struct {
	added   chan struct{}
	removed chan struct{}
}

func (l *recordingLabeler) AddLabel(map[string]string) error {
	l.added <- struct{}{}
	return nil
}

func (l *recordingLabeler) RemoveLabel(map[string]string) error {
	l.removed <- struct{}{}
	return nil
}

// stubNetwork is a minimal vip.Network implementation for exercising
// cleanupVIP without a real interface.
type stubNetwork struct {
	ip            string
	deleteIPCalls int
}

func (s *stubNetwork) AddIP(bool, bool, ...int) (bool, error) { return false, nil }
func (s *stubNetwork) AddRoute(bool) (bool, error)            { return false, nil }
func (s *stubNetwork) ReplaceRoute() error                    { return nil }
func (s *stubNetwork) DeleteIP() (bool, error)                { s.deleteIPCalls++; return true, nil }
func (s *stubNetwork) DeleteRoute() error                     { return nil }
func (s *stubNetwork) UpdateRoutes() (bool, error)            { return false, nil }
func (s *stubNetwork) IsSet() (*netlink.Addr, error)          { return nil, nil }
func (s *stubNetwork) IP() string                             { return s.ip }
func (s *stubNetwork) CIDR() string                           { return s.ip + "/32" }
func (s *stubNetwork) IPisLinkLocal() bool                    { return false }
func (s *stubNetwork) PrepareRoute() *netlink.Route           { return nil }
func (s *stubNetwork) RouteHash() string                      { return "" }
func (s *stubNetwork) SetIP(string) error                     { return nil }
func (s *stubNetwork) SetServicePorts(*v1.Service)            {}
func (s *stubNetwork) Interface() string                      { return "eth0" }
func (s *stubNetwork) IsDADFAIL() bool                        { return false }
func (s *stubNetwork) IsDNS() bool                            { return false }
func (s *stubNetwork) IsDDNS() bool                           { return false }
func (s *stubNetwork) DDNSHostName() string                   { return "" }
func (s *stubNetwork) DNSName() string                        { return "" }
func (s *stubNetwork) SetMask(string) error                   { return nil }
func (s *stubNetwork) SetHasEndpoints(bool)                   {}
func (s *stubNetwork) HasEndpoints() bool                     { return false }
func (s *stubNetwork) ARPName() string                        { return "shared-vip" }
func (s *stubNetwork) GetPossibleSubnets() string             { return "" }
func (s *stubNetwork) DHCPFamily() string                     { return "" }
func (s *stubNetwork) IPVSMark() uint32                       { return 0 }

// TestCleanupVIPRetainsSharedVIPWithOneSiblingLeft reproduces the off-by-one:
// layer2Update already removes its own ARP claim before cleanupVIP runs, so a
// single remaining sibling must still block deletion.
func TestCleanupVIPRetainsSharedVIPWithOneSiblingLeft(t *testing.T) {
	arpMgr := arp.NewManager(&kubevip.Config{ArpBroadcastRate: 3000})
	netA := &stubNetwork{ip: "192.0.2.10"}
	netB := &stubNetwork{ip: "192.0.2.10"}

	instA := arp.NewInstance(netA, nil)
	instB := arp.NewInstance(netB, nil)
	arpMgr.Insert(instA)
	arpMgr.Insert(instB)

	// Cluster A's layer2Update goroutine ends first and drops its own claim,
	// leaving only sibling B registered.
	arpMgr.Remove(instA)

	c := &Cluster{arpMgr: arpMgr}
	c.cleanupVIP(&kubevip.Config{EnableARP: true}, netA)

	if netA.deleteIPCalls != 0 {
		t.Fatalf("cleanupVIP deleted the shared VIP while a sibling was still registered")
	}
}

func TestControlPlaneFollowsSharedServiceElection(t *testing.T) {
	config := &kubevip.Config{KubernetesLeaderElection: kubevip.KubernetesLeaderElection{LeaseName: "default/shared"}}
	leaseID := lease.NewID(config.LeaderElectionType, "default", "shared")
	leaseMgr := lease.NewManager()
	sharedLease, _ := leaseMgr.Acquire(context.Background(), leaseID, "service")
	if !sharedLease.BeginElection() {
		t.Fatal("Service election did not start")
	}
	sharedLease.ElectionStarted()

	labels := &recordingLabeler{added: make(chan struct{}, 1), removed: make(chan struct{}, 1)}
	cluster := &Cluster{stop: make(chan struct{}), nodeLabelMgr: labels}
	done := make(chan error, 1)
	go func() {
		done <- cluster.StartCluster(context.Background(), config, nil, nil, leaseMgr, func() {})
	}()
	select {
	case <-labels.added:
	case <-time.After(time.Second):
		t.Fatal("control plane did not activate under the shared Service election")
	}

	sharedLease.ElectionStopped()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shared control-plane follower returned an error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("control plane did not stop after shared Service leadership ended")
	}
	select {
	case <-labels.removed:
	default:
		t.Fatal("control-plane label was not removed after shared leadership ended")
	}
	if sharedLease.Ctx.Err() != nil || leaseMgr.Get(leaseID) != sharedLease {
		t.Fatal("control-plane cleanup cancelled the surviving Service lease")
	}
	leaseMgr.Delete(leaseID, "service", sharedLease)
}

func TestStopAndWaitPreservingUpgradesInProgressStop(t *testing.T) {
	done := make(chan struct{})
	service := &Cluster{
		stop: make(chan struct{}),
		service: &servicesWorker{
			stop:     make(chan struct{}),
			done:     done,
			stopping: true,
		},
	}

	returned := make(chan struct{})
	go func() {
		service.StopAndWaitPreserving("192.0.2.10")
		close(returned)
	}()

	deadline := time.Now().Add(time.Second)
	for {
		service.stopMu.Lock()
		_, preserving := service.service.preserveVIPs["192.0.2.10"]
		service.stopMu.Unlock()
		if preserving {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("preserving stop did not update the in-progress worker shutdown")
		}
		time.Sleep(time.Millisecond)
	}

	service.finishServicesWorker(done)
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("preserving stop did not return after worker cleanup completed")
	}
}
