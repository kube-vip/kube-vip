package vip

import (
	"net"
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
