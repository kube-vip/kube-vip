//go:build linux

package vip

import (
	"context"
	"net"
	"runtime"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv6"
	"github.com/insomniacslk/dhcp/dhcpv6/nclient6"
	"github.com/insomniacslk/dhcp/dhcpv6/server6"
	"github.com/insomniacslk/dhcp/iana"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"

	"github.com/kube-vip/kube-vip/pkg/sysctl"
)

// TestDHCPv6ClientSendsVendorClassToServer runs the client against a DHCPv6
// server over a veth pair and checks that every message the server receives,
// including the ones sent to renew the lease, carries the vendor class.
func TestDHCPv6ClientSendsVendorClassToServer(t *testing.T) {
	// Sockets belong to the namespace of the thread that opens them, so the
	// server and client sockets are opened on this thread after switching it to
	// their namespace. They keep working when other threads read from them.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	originalNamespace, err := netns.Get()
	if err != nil {
		t.Fatalf("getting current network namespace: %v", err)
	}
	defer originalNamespace.Close()
	// The server gets its own namespace: with both ends of the veth pair in one
	// namespace the kernel would deliver unicast packets over loopback.
	serverNamespace, err := netns.New()
	if err != nil {
		if requireNetworkNamespaces {
			t.Fatalf("creating isolated network namespace: %v", err)
		}
		t.Skipf("creating isolated network namespace: %v", err)
	}
	defer serverNamespace.Close()
	defer func() {
		if err := netns.Set(originalNamespace); err != nil {
			t.Errorf("restoring network namespace: %v", err)
		}
	}()
	clientNamespace, err := netns.New()
	if err != nil {
		t.Fatalf("creating client network namespace: %v", err)
	}
	defer clientNamespace.Close()

	veth := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: "kvdhcp6c0"}, PeerName: "kvdhcp6s0"}
	if err := netlink.LinkAdd(veth); err != nil {
		t.Fatalf("creating veth pair: %v", err)
	}
	serverLink, err := netlink.LinkByName("kvdhcp6s0")
	if err != nil {
		t.Fatalf("finding server interface: %v", err)
	}
	if err := netlink.LinkSetNsFd(serverLink, int(serverNamespace)); err != nil {
		t.Fatalf("moving server interface: %v", err)
	}
	setUpWithoutDAD(t, veth)

	if err := netns.Set(serverNamespace); err != nil {
		t.Fatalf("entering server network namespace: %v", err)
	}
	serverLink, err = netlink.LinkByName("kvdhcp6s0")
	if err != nil {
		t.Fatalf("finding server interface: %v", err)
	}
	setUpWithoutDAD(t, serverLink)
	waitForLinkLocal(t, serverLink)

	serverID := &dhcpv6.DUIDLL{HWType: iana.HWTypeEthernet, LinkLayerAddr: serverLink.Attrs().HardwareAddr}
	leasedIP := dhcpv6.OptIAAddress{
		IPv6Addr:          net.ParseIP("2001:db8::10"),
		PreferredLifetime: time.Hour,
		ValidLifetime:     2 * time.Hour,
	}
	received := make(chan *dhcpv6.Message, 8)
	server, err := server6.NewServer("kvdhcp6s0", nclient6.AllDHCPRelayAgentsAndServers,
		func(conn net.PacketConn, peer net.Addr, m dhcpv6.DHCPv6) {
			request, ok := m.(*dhcpv6.Message)
			if !ok {
				return
			}
			select {
			case received <- request:
			default:
			}
			// A reply that cannot be built or sent makes the client time out,
			// which fails the test without reporting from the server goroutine.
			var reply *dhcpv6.Message
			var err error
			modifiers := []dhcpv6.Modifier{dhcpv6.WithServerID(serverID), dhcpv6.WithIANA(leasedIP)}
			switch request.MessageType {
			case dhcpv6.MessageTypeSolicit:
				reply, err = dhcpv6.NewAdvertiseFromSolicit(request, modifiers...)
			case dhcpv6.MessageTypeRequest, dhcpv6.MessageTypeRenew, dhcpv6.MessageTypeRebind:
				reply, err = dhcpv6.NewReplyFromMessage(request, modifiers...)
			default:
				return
			}
			if err == nil {
				_, _ = conn.WriteTo(reply.ToBytes(), peer)
			}
		})
	if err != nil {
		t.Fatalf("starting DHCPv6 server: %v", err)
	}
	go func() { _ = server.Serve() }()
	defer server.Close()

	if err := netns.Set(clientNamespace); err != nil {
		t.Fatalf("entering client network namespace: %v", err)
	}

	// The DHCPv6 client binds to the link-local address of its interface.
	waitForLinkLocal(t, veth)
	iface, err := net.InterfaceByName("kvdhcp6c0")
	if err != nil {
		t.Fatalf("finding client interface: %v", err)
	}
	client, err := NewDHCPv6Client(iface, nil, false, "", 1)
	if err != nil {
		t.Fatalf("creating DHCPv6 client: %v", err)
	}
	defer client.Stop()
	client.WithVendorClass(documentationEnterpriseNumber, "kube-vip").WithHostName("nginx-dhcp")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	addr, err := client.request(ctx, false)
	if err != nil {
		t.Fatalf("requesting lease: %v", err)
	}
	if !addr.IPv6Addr.Equal(leasedIP.IPv6Addr) {
		t.Fatalf("leased address = %s, want %s", addr.IPv6Addr, leasedIP.IPv6Addr)
	}
	client.storeAddr(addr)
	if _, err := client.renew(ctx); err != nil {
		t.Fatalf("renewing lease: %v", err)
	}

	for _, want := range []dhcpv6.MessageType{
		dhcpv6.MessageTypeSolicit,
		dhcpv6.MessageTypeRequest,
		dhcpv6.MessageTypeSolicit,
		dhcpv6.MessageTypeRenew,
	} {
		var message *dhcpv6.Message
		select {
		case message = <-received:
		case <-ctx.Done():
			t.Fatalf("server did not receive the %s", want)
		}
		if message.MessageType != want {
			t.Fatalf("server received a %s, want a %s", message.MessageType, want)
		}
		assertDHCPv6VendorClass(t, message, documentationEnterpriseNumber, "kube-vip")
	}
}

// setUpWithoutDAD brings link up with duplicate address detection disabled, so
// its link-local address is usable as soon as the link has a carrier.
func setUpWithoutDAD(t *testing.T, link netlink.Link) {
	t.Helper()
	name := link.Attrs().Name
	if err := sysctl.WriteProcSys("/proc/sys/net/ipv6/conf/"+name+"/accept_dad", "0"); err != nil {
		t.Fatalf("disabling duplicate address detection on %s: %v", name, err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		t.Fatalf("bringing up %s: %v", name, err)
	}
}

// waitForLinkLocal waits until link has a usable link-local address, which the
// kernel only adds once both ends of the veth pair are up.
func waitForLinkLocal(t *testing.T, link netlink.Link) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		addresses, err := netlink.AddrList(link, netlink.FAMILY_V6)
		if err != nil {
			t.Fatalf("listing addresses of %s: %v", link.Attrs().Name, err)
		}
		for _, address := range addresses {
			if address.IP.IsLinkLocalUnicast() && address.Flags&unix.IFA_F_TENTATIVE == 0 {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s did not get a link-local address", link.Attrs().Name)
}
