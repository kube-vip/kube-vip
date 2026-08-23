//go:build linux

package vip

import (
	"testing"

	"github.com/kube-vip/kube-vip/pkg/networkinterface"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func TestAddIPPerCallDADSkipDoesNotPersist(t *testing.T) {
	address, err := netlink.ParseAddr("2001:db8::10/128")
	if err != nil {
		t.Fatal(err)
	}

	configurator := &network{
		address: address,
		link: &networkinterface.Link{
			Intf: &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "kube-vip-dad-test"}},
		},
	}

	// The netlink operation may fail without CAP_NET_ADMIN, but the address
	// flags are set before that operation and are what this test exercises.
	_, _ = configurator.AddIP(false, true)
	if configurator.address.Flags&unix.IFA_F_NODAD == 0 {
		t.Fatal("skipDAD=true did not set IFA_F_NODAD")
	}

	_, _ = configurator.AddIP(false, false)
	if configurator.address.Flags&unix.IFA_F_NODAD != 0 {
		t.Fatal("IFA_F_NODAD persisted into a normal AddIP call")
	}
}
