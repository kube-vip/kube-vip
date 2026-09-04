package networkinterface

import (
	"sync"
	"testing"

	"github.com/vishvananda/netlink"
)

func TestManagerGetReplacesChangedInterfaceIndex(t *testing.T) {
	manager := NewManager()
	firstInterface := dummyLink("eth0", 1)
	first := manager.Get(firstInterface)

	if got := manager.Get(dummyLink("eth0", 1)); got != first {
		t.Fatal("Get returned a new link for the same interface generation")
	}

	secondInterface := dummyLink("eth0", 2)
	second := manager.Get(secondInterface)
	if second != first {
		t.Fatal("Get replaced the shared link after the interface index changed")
	}
	var current netlink.Link
	if err := first.WithInterface(func(intf netlink.Link) error {
		current = intf
		return nil
	}); err != nil {
		t.Fatalf("WithInterface() error = %v", err)
	}
	if current != secondInterface {
		t.Fatal("Get did not retain the new link generation")
	}
}

func TestManagerGetConcurrent(t *testing.T) {
	manager := NewManager()
	interfaces := []netlink.Link{dummyLink("eth0", 1), dummyLink("eth1", 2)}
	var wg sync.WaitGroup
	results := make(chan struct {
		index int
		link  *Link
	}, 64)
	for index := range cap(results) {
		interfaceIndex := index % len(interfaces)
		wg.Go(func() {
			results <- struct {
				index int
				link  *Link
			}{index: interfaceIndex, link: manager.Get(interfaces[interfaceIndex])}
		})
	}
	wg.Wait()
	close(results)

	var cached [2]*Link
	for result := range results {
		if result.link == nil {
			t.Fatal("concurrent interface lookup returned nil")
		}
		if cached[result.index] == nil {
			cached[result.index] = result.link
		} else if result.link != cached[result.index] {
			t.Fatalf("interface %d produced multiple cached Link objects", result.index)
		}
	}
	if cached[0] == cached[1] {
		t.Fatal("different interfaces shared one cached Link object")
	}
}

func dummyLink(name string, index int) netlink.Link {
	return &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: name, Index: index}}
}
