package networkinterface

import (
	"fmt"
	"sync"
	"testing"

	"github.com/vishvananda/netlink"
)

func TestManagerGetGenerations(t *testing.T) {
	manager := NewManager()
	firstIntf := dummyLink("eth0", 1)
	first := manager.Get(firstIntf)

	if got := manager.Get(dummyLink("eth0", 1)); got != first {
		t.Fatal("Get returned a new link for the same name and index")
	}

	secondIntf := dummyLink("eth0", 2)
	second := manager.Get(secondIntf)
	if second == first {
		t.Fatal("Get reused a link after the interface index changed")
	}
	if first.Intf != firstIntf {
		t.Fatal("Get mutated the previous link generation")
	}
	if second.Intf != secondIntf {
		t.Fatal("Get did not retain the new link generation")
	}
}

func TestManagerGetConcurrent(t *testing.T) {
	manager := NewManager()
	const goroutines = 32

	results := make(chan *Link, goroutines)
	var wg sync.WaitGroup
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- manager.Get(dummyLink("eth0", 1))
		}()
	}
	wg.Wait()
	close(results)

	var first *Link
	for result := range results {
		if first == nil {
			first = result
		} else if result != first {
			t.Fatal("concurrent Get returned multiple links for the same interface")
		}
	}

	for i := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			manager.Get(dummyLink(fmt.Sprintf("eth%d", i+1), i+2))
		}()
	}
	wg.Wait()

	if got, want := len(manager.interfaces), goroutines+1; got != want {
		t.Fatalf("manager has %d interfaces, want %d", got, want)
	}
	for i := range goroutines {
		name := fmt.Sprintf("eth%d", i+1)
		if got := manager.Get(dummyLink(name, i+2)); got.Intf.Attrs().Name != name {
			t.Fatalf("Get(%q) returned interface %q", name, got.Intf.Attrs().Name)
		}
	}
}

func dummyLink(name string, index int) netlink.Link {
	return &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: name, Index: index}}
}
