package vip

import (
	"net"
	"sync"
	"sync/atomic"
	"testing"

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
		iface:        &net.Interface{Name: "vlan-child"},
		managerKey:   "parent0",
		ipChan:       make(chan string),
		stopChan:     make(chan struct{}),
		releasedChan: make(chan struct{}),
		ic:           shared,
	}
	close(client.releasedChan)

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
