package arp

import (
	"sync"
	"testing"
	"time"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/vishvananda/netlink"
	v1 "k8s.io/api/core/v1"
)

func TestDrainLinkUpdatesReleasesParkedSender(t *testing.T) {
	events := make(chan netlink.LinkUpdate)
	sent := make(chan struct{})
	go func() {
		events <- netlink.LinkUpdate{}
		close(sent)
	}()

	drainLinkUpdates(events)

	select {
	case <-sent:
	case <-time.After(time.Second):
		t.Fatal("netlink sender is still parked on an unread link update")
	}
}

// stubNetwork is a minimal vip.Network implementation; only ARPName matters here.
type stubNetwork struct {
	name          string
	deleteStarted chan struct{}
	releaseDelete chan struct{}
}

func (s *stubNetwork) AddIP(bool, bool, ...int) (bool, error) { return false, nil }
func (s *stubNetwork) AddRoute(bool) (bool, error)            { return false, nil }
func (s *stubNetwork) ReplaceRoute() error                    { return nil }
func (s *stubNetwork) DeleteIP() (bool, error) {
	if s.deleteStarted != nil {
		close(s.deleteStarted)
		<-s.releaseDelete
	}
	return true, nil
}
func (s *stubNetwork) DeleteRoute() error            { return nil }
func (s *stubNetwork) UpdateRoutes() (bool, error)   { return false, nil }
func (s *stubNetwork) IsSet() (*netlink.Addr, error) { return nil, nil }
func (s *stubNetwork) IP() string                    { return "" }
func (s *stubNetwork) CIDR() string                  { return "" }
func (s *stubNetwork) IPisLinkLocal() bool           { return false }
func (s *stubNetwork) PrepareRoute() *netlink.Route  { return nil }
func (s *stubNetwork) RouteHash() string             { return "" }
func (s *stubNetwork) SetIP(string) error            { return nil }
func (s *stubNetwork) SetServicePorts(*v1.Service)   {}
func (s *stubNetwork) Interface() string             { return "eth0" }
func (s *stubNetwork) IsDADFAIL() bool               { return false }
func (s *stubNetwork) IsDNS() bool                   { return false }
func (s *stubNetwork) IsDDNS() bool                  { return false }
func (s *stubNetwork) DDNSHostName() string          { return "" }
func (s *stubNetwork) DNSName() string               { return "" }
func (s *stubNetwork) SetMask(string) error          { return nil }
func (s *stubNetwork) SetHasEndpoints(bool)          {}
func (s *stubNetwork) HasEndpoints() bool            { return false }
func (s *stubNetwork) ARPName() string               { return s.name }
func (s *stubNetwork) GetPossibleSubnets() string    { return "" }
func (s *stubNetwork) DHCPFamily() string            { return "" }
func (s *stubNetwork) IPVSMark() uint32              { return 0 }

// TestManagerInsertConcurrentFirstRegistrationsDoNotLoseClaims guards the
// get-then-store race: two never-before-seen instances for the same ARP name
// registering concurrently must both be counted, not just the last writer.
func TestManagerInsertConcurrentFirstRegistrationsDoNotLoseClaims(t *testing.T) {
	m := NewManager(&kubevip.Config{ArpBroadcastRate: 3000})
	const concurrent = 8

	var wg sync.WaitGroup
	for range concurrent {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.Insert(NewInstance(&stubNetwork{name: "shared"}, nil))
		}()
	}
	wg.Wait()

	if got := m.Count("shared"); got != concurrent {
		t.Fatalf("Count() = %d, want %d claims registered", got, concurrent)
	}
}

func TestManagerInsertDoesNotJoinEntryBeingRemoved(t *testing.T) {
	m := NewManager(&kubevip.Config{ArpBroadcastRate: 3000})
	deleteStarted := make(chan struct{})
	releaseDelete := make(chan struct{})
	first := NewInstance(&stubNetwork{name: "shared", deleteStarted: deleteStarted, releaseDelete: releaseDelete}, nil)
	m.Insert(first)

	removeDone := make(chan struct{})
	go func() {
		m.Remove(first)
		close(removeDone)
	}()
	<-deleteStarted

	insertDone := make(chan struct{})
	go func() {
		m.Insert(NewInstance(&stubNetwork{name: "shared"}, nil))
		close(insertDone)
	}()
	close(releaseDelete)
	<-removeDone
	<-insertDone

	if got := m.Count("shared"); got != 1 {
		t.Fatalf("Count() = %d, want replacement claim registered", got)
	}
}
