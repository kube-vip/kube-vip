//go:build linux

package instance

import (
	"errors"
	"os"
	"runtime"
	"testing"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// requireNetworkNamespaces makes the privileged CI job fail instead of silently
// skipping when it cannot enter a network namespace.
var requireNetworkNamespaces = os.Getenv("KUBE_VIP_REQUIRE_NETNS") != ""

func TestCleanupLinkAttachmentsOnlyDeletesOwnedVLAN(t *testing.T) {
	for _, test := range []struct {
		name      string
		preexists bool
		inUse     bool
	}{
		{name: "owned VLAN", preexists: false},
		{name: "adopted VLAN", preexists: true},
		{name: "owned VLAN used by another Service", inUse: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()

			originalNamespace, err := netns.Get()
			if err != nil {
				t.Fatalf("getting current network namespace: %v", err)
			}
			defer originalNamespace.Close()
			testNamespace, err := netns.New()
			if err != nil {
				if requireNetworkNamespaces {
					t.Fatalf("creating isolated network namespace: %v", err)
				}
				t.Skipf("creating isolated network namespace: %v", err)
			}
			defer testNamespace.Close()
			defer func() {
				if err := netns.Set(originalNamespace); err != nil {
					t.Errorf("restoring network namespace: %v", err)
				}
			}()

			parent := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "kvattach0"}}
			if err := netlink.LinkAdd(parent); err != nil {
				t.Fatalf("creating parent interface: %v", err)
			}
			if err := netlink.LinkSetUp(parent); err != nil {
				t.Fatalf("bringing up parent interface: %v", err)
			}
			if test.preexists {
				vlan := &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "kvattach0.42", ParentIndex: parent.Attrs().Index}, VlanId: 42}
				if err := netlink.LinkAdd(vlan); err != nil {
					t.Fatalf("creating existing VLAN: %v", err)
				}
			}

			instance := &Instance{}
			if err := instance.addVLAN(parent.Attrs().Name, 42); err != nil {
				t.Fatalf("adding VLAN attachment: %v", err)
			}
			if instance.vlanOwned.Load() == test.preexists {
				t.Fatalf("vlanOwned = %t, want %t", instance.vlanOwned.Load(), !test.preexists)
			}
			var remaining []*Instance
			if test.inUse {
				remaining = []*Instance{{IsVLAN: true, VLANInterface: instance.VLANInterface}}
			}
			if err := instance.CleanupLinkAttachments(remaining...); err != nil {
				t.Fatalf("cleaning attachments: %v", err)
			}
			if test.inUse {
				if !remaining[0].vlanOwned.Load() {
					t.Fatal("remaining Service did not receive VLAN cleanup ownership")
				}
				if _, err := netlink.LinkByName("kvattach0.42"); err != nil {
					t.Fatalf("VLAN was removed while a Service still used it: %v", err)
				}
				if err := remaining[0].CleanupLinkAttachments(); err != nil {
					t.Fatalf("cleaning transferred attachment: %v", err)
				}
			}

			_, err = netlink.LinkByName("kvattach0.42")
			var notFound netlink.LinkNotFoundError
			if test.preexists && err != nil {
				t.Fatalf("adopted VLAN was removed: %v", err)
			}
			if !test.preexists && !errors.As(err, &notFound) {
				t.Fatalf("owned VLAN remains after cleanup: %v", err)
			}
		})
	}
}
