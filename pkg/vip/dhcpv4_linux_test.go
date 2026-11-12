//go:build linux

package vip

import (
	"context"
	"net"
	"runtime"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/server4"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// TestDHCPv4ClientSendsIdentityOptionsToServer runs the client against a DHCP
// server over a veth pair and checks the options of every message the server
// receives, including the renewal that is sent once the lease is bound.
func TestDHCPv4ClientSendsIdentityOptionsToServer(t *testing.T) {
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

	serverIP := net.IPv4(192, 0, 2, 1).To4()
	leasedIP := net.IPv4(192, 0, 2, 10).To4()
	veth := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: "kvdhcpc0"}, PeerName: "kvdhcps0"}
	if err := netlink.LinkAdd(veth); err != nil {
		t.Fatalf("creating veth pair: %v", err)
	}
	if err := netlink.LinkSetUp(veth); err != nil {
		t.Fatalf("bringing up client interface: %v", err)
	}
	serverLink, err := netlink.LinkByName("kvdhcps0")
	if err != nil {
		t.Fatalf("finding server interface: %v", err)
	}
	if err := netlink.LinkSetNsFd(serverLink, int(serverNamespace)); err != nil {
		t.Fatalf("moving server interface: %v", err)
	}

	if err := netns.Set(serverNamespace); err != nil {
		t.Fatalf("entering server network namespace: %v", err)
	}
	serverLink, err = netlink.LinkByName("kvdhcps0")
	if err != nil {
		t.Fatalf("finding server interface: %v", err)
	}
	if err := netlink.AddrAdd(serverLink, &netlink.Addr{IPNet: &net.IPNet{IP: serverIP, Mask: net.CIDRMask(24, 32)}}); err != nil {
		t.Fatalf("adding server address: %v", err)
	}
	if err := netlink.LinkSetUp(serverLink); err != nil {
		t.Fatalf("bringing up server interface: %v", err)
	}
	received := make(chan *dhcpv4.DHCPv4, 8)
	server, err := server4.NewServer("kvdhcps0", &net.UDPAddr{IP: net.IPv4zero, Port: dhcpv4.ServerPort},
		func(conn net.PacketConn, peer net.Addr, request *dhcpv4.DHCPv4) {
			select {
			case received <- request:
			default:
			}
			var replyType dhcpv4.MessageType
			switch request.MessageType() {
			case dhcpv4.MessageTypeDiscover:
				replyType = dhcpv4.MessageTypeOffer
			case dhcpv4.MessageTypeRequest:
				replyType = dhcpv4.MessageTypeAck
			default:
				return
			}
			// A reply that cannot be built or sent makes the client time out,
			// which fails the test without reporting from the server goroutine.
			reply, err := dhcpv4.NewReplyFromRequest(request,
				dhcpv4.WithMessageType(replyType),
				dhcpv4.WithYourIP(leasedIP),
				dhcpv4.WithServerIP(serverIP),
				dhcpv4.WithOption(dhcpv4.OptServerIdentifier(serverIP)),
				dhcpv4.WithOption(dhcpv4.OptSubnetMask(net.CIDRMask(24, 32))),
				dhcpv4.WithOption(dhcpv4.OptIPAddressLeaseTime(time.Hour)),
			)
			if err == nil {
				_, _ = conn.WriteTo(reply.ToBytes(), peer)
			}
		})
	if err != nil {
		t.Fatalf("starting DHCP server: %v", err)
	}
	go func() { _ = server.Serve() }()
	defer server.Close()

	if err := netns.Set(clientNamespace); err != nil {
		t.Fatalf("entering client network namespace: %v", err)
	}

	iface, err := net.InterfaceByName("kvdhcpc0")
	if err != nil {
		t.Fatalf("finding client interface: %v", err)
	}
	client := NewDHCPv4Client(iface, false, "", 1, false).WithVendorClass("kube-vip")
	client.WithHostName("nginx-dhcp")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	lease, err := client.request(ctx, false)
	if err != nil {
		t.Fatalf("requesting lease: %v", err)
	}
	client.storeLease(lease)

	// kube-vip adds the leased address to the interface before it renews.
	if err := netlink.AddrAdd(veth, &netlink.Addr{IPNet: &net.IPNet{IP: leasedIP, Mask: net.CIDRMask(24, 32)}}); err != nil {
		t.Fatalf("adding leased address: %v", err)
	}
	if _, err := client.renew(ctx); err != nil {
		t.Fatalf("renewing lease: %v", err)
	}

	for _, want := range []struct {
		name        string
		messageType dhcpv4.MessageType
		clientIP    net.IP
	}{
		{name: "DHCPDISCOVER", messageType: dhcpv4.MessageTypeDiscover, clientIP: net.IPv4zero},
		{name: "DHCPREQUEST", messageType: dhcpv4.MessageTypeRequest, clientIP: net.IPv4zero},
		{name: "renewal DHCPREQUEST", messageType: dhcpv4.MessageTypeRequest, clientIP: leasedIP},
	} {
		var message *dhcpv4.DHCPv4
		select {
		case message = <-received:
		case <-ctx.Done():
			t.Fatalf("server did not receive the %s", want.name)
		}
		if message.MessageType() != want.messageType || !message.ClientIPAddr.Equal(want.clientIP) {
			t.Fatalf("server received %s from %s, want the %s", message.MessageType(), message.ClientIPAddr, want.name)
		}
		assertDHCPv4Option(t, message, dhcpv4.OptionHostName, "nginx-dhcp")
		assertDHCPv4Option(t, message, dhcpv4.OptionClientIdentifier, "nginx-dhcp")
		assertDHCPv4Option(t, message, dhcpv4.OptionClassIdentifier, "kube-vip")
	}
}
