package vip

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv6"
)

func TestDHCPv6StopReleasesManagerReferenceForParentInterface(t *testing.T) {
	// DEFECT: Stop deletes the manager entry using the VLAN child name even though NewDHCPv6Client keyed the shared client by its parent name (pkg/vip/dhcpv6.go:143).
	previousManager := dhcpv6ClientManager
	t.Cleanup(func() { dhcpv6ClientManager = previousManager })

	references := &atomic.Int32{}
	references.Store(2)
	shared := &DHCPv6InternalClient{references: references}
	dhcpv6ClientManager = &DHCPv6ClientManager{
		clients: map[string]*DHCPv6InternalClient{"parent0": shared},
	}

	client := &DHCPv6Client{
		iface:      &net.Interface{Name: "vlan-child"},
		managerKey: "parent0",
		ipChan:     make(chan string),
		stopChan:   make(chan struct{}),
		ic:         shared,
		addr:       &dhcpv6.OptIAAddress{},
	}

	client.Stop()

	if got := references.Load(); got != 1 {
		t.Fatalf("manager reference count = %d, want 1 after stopping one VLAN client", got)
	}
}

func TestDHCPv6ClientManagerSharesOneClientPerParentInterface(t *testing.T) {
	references := &atomic.Int32{}
	references.Store(1)
	shared := &DHCPv6InternalClient{references: references}
	manager := &DHCPv6ClientManager{
		clients: map[string]*DHCPv6InternalClient{"parent0": shared},
	}

	var wg sync.WaitGroup
	for range 64 {
		wg.Go(func() {
			client, err := manager.Add("parent0")
			if err != nil {
				t.Errorf("Add() error = %v", err)
				return
			}
			if client != shared {
				t.Errorf("Add() client = %p, want the shared client %p", client, shared)
			}
			manager.Delete("parent0")
		})
	}
	wg.Wait()

	if got := manager.Get("parent0"); got != shared {
		t.Fatalf("shared client = %v, want it retained while still referenced", got)
	}
	if got := references.Load(); got != 1 {
		t.Fatalf("manager reference count = %d, want 1", got)
	}
}

func TestDHCPRetryTimersAreRescheduledAfterFailure(t *testing.T) {
	t1, t2 := time.NewTimer(time.Hour), time.NewTimer(time.Hour)
	t1.Stop()
	t2.Stop()
	resetLeaseTimers(t1, t2, 10*time.Millisecond, 20*time.Millisecond)
	select {
	case <-t1.C:
	case <-time.After(time.Second):
		t.Fatal("renew timer was not rescheduled")
	}
	select {
	case <-t2.C:
	case <-time.After(time.Second):
		t.Fatal("rebind timer was not rescheduled")
	}
}

func TestDHCPStopWaitsForReleaseCompletion(t *testing.T) {
	v4 := NewDHCPv4Client(nil, false, "", 0, false)
	close(v4.started)
	v4Done := make(chan struct{})
	go func() { v4.Stop(); close(v4Done) }()
	select {
	case <-v4Done:
		t.Fatal("DHCPv4 Stop returned before release completion")
	case <-time.After(20 * time.Millisecond):
	}
	close(v4.done)
	select {
	case <-v4Done:
	case <-time.After(time.Second):
		t.Fatal("DHCPv4 Stop did not finish after release completion")
	}

	v6 := &DHCPv6Client{stopChan: make(chan struct{}), started: make(chan struct{}), done: make(chan struct{}), managerKey: "missing"}
	close(v6.started)
	v6Done := make(chan struct{})
	go func() { v6.Stop(); close(v6Done) }()
	select {
	case <-v6Done:
		t.Fatal("DHCPv6 Stop returned before release completion")
	case <-time.After(20 * time.Millisecond):
	}
	close(v6.done)
	select {
	case <-v6Done:
	case <-time.After(time.Second):
		t.Fatal("DHCPv6 Stop did not finish after release completion")
	}
}

func TestDHCPStopBeforeStartPreventsNetworkSetup(t *testing.T) {
	v4 := NewDHCPv4Client(nil, false, "", 0, false)
	v4.Stop()
	v4Done := make(chan error, 1)
	go func() { v4Done <- v4.Start(context.Background()) }()
	select {
	case err := <-v4Done:
		if err != nil {
			t.Fatalf("DHCPv4 Start after Stop returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("DHCPv4 Start after Stop did not skip setup")
	}

	v6 := &DHCPv6Client{
		iface:      &net.Interface{Name: "never-started"},
		stopChan:   make(chan struct{}),
		done:       make(chan struct{}),
		started:    make(chan struct{}),
		managerKey: "missing",
	}
	v6.Stop()
	v6Done := make(chan error, 1)
	go func() { v6Done <- v6.Start(context.Background()) }()
	select {
	case err := <-v6Done:
		if err != nil {
			t.Fatalf("DHCPv6 Start after Stop returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("DHCPv6 Start after Stop did not skip setup")
	}
}

func TestGetAddressRejectsIANAWithoutAddresses(t *testing.T) {
	// DEFECT: getAddress indexes the first IAADDR without checking whether the IANA contains one, so a malformed/expired reply panics (pkg/vip/dhcpv6.go:392).
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("getAddress panicked on an IANA without IAADDR: %v", recovered)
		}
	}()

	if _, err := getAddress([]*dhcpv6.OptIANA{{}}); err == nil {
		t.Fatal("getAddress accepted an IANA without an IAADDR")
	}
}
