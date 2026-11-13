package vip

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv6"
	"github.com/insomniacslk/dhcp/dhcpv6/server6"
	"github.com/insomniacslk/dhcp/iana"
	"github.com/mdlayher/packet"
	"github.com/vishvananda/netlink"
)

// The synthetic peer receives real multicast packets without requiring group-join
// support in the test runtime. Replies still use the native IPv6 UDP socket.
// This socket is separate from the independent raw wire observer below.
type dhcpv6SyntheticPeerConn struct {
	net.PacketConn
	raw  *packet.Conn
	zone string
}

func (c *dhcpv6SyntheticPeerConn) ReadFrom(buf []byte) (int, net.Addr, error) {
	frame := make([]byte, 65536)
	for {
		n, _, err := c.raw.ReadFrom(frame)
		if err != nil {
			return 0, nil, err
		}
		msg, err := decodeDHCPv6ClientWire(frame[:n])
		if err != nil {
			return 0, nil, err
		}
		if msg == nil {
			continue
		}
		payloadLen := int(binary.BigEndian.Uint16(frame[44:46])) - 8
		if payloadLen > len(buf) {
			return 0, nil, fmt.Errorf("synthetic DHCPv6 peer receive buffer too small")
		}
		copy(buf, frame[48:48+payloadLen])
		return payloadLen, &net.UDPAddr{IP: append(net.IP(nil), frame[8:24]...), Port: int(binary.BigEndian.Uint16(frame[40:42])), Zone: c.zone}, nil
	}
}

func (c *dhcpv6SyntheticPeerConn) Close() error {
	return errors.Join(c.raw.Close(), c.PacketConn.Close())
}

func (c *dhcpv6SyntheticPeerConn) SetDeadline(deadline time.Time) error {
	return errors.Join(c.raw.SetDeadline(deadline), c.PacketConn.SetDeadline(deadline))
}

func (c *dhcpv6SyntheticPeerConn) SetReadDeadline(deadline time.Time) error {
	return c.raw.SetReadDeadline(deadline)
}

// Only the plain IPv6 UDP client traffic used by this isolated fixture is decoded.
func decodeDHCPv6ClientWire(frame []byte) (*dhcpv6.Message, error) {
	if len(frame) < 48 || frame[0]>>4 != 6 || frame[6] != syscall.IPPROTO_UDP {
		return nil, nil
	}
	udp := frame[40:]
	if binary.BigEndian.Uint16(udp[:2]) != 546 || binary.BigEndian.Uint16(udp[2:4]) != 547 {
		return nil, nil
	}
	payloadLen, udpLen := int(binary.BigEndian.Uint16(frame[4:6])), int(binary.BigEndian.Uint16(udp[4:6]))
	if payloadLen+40 > len(frame) || udpLen < 12 || udpLen > payloadLen {
		return nil, fmt.Errorf("invalid isolated DHCPv6 wire lengths")
	}
	decoded, err := dhcpv6.FromBytes(append([]byte(nil), udp[8:udpLen]...))
	if err != nil {
		return nil, err
	}
	msg, ok := decoded.(*dhcpv6.Message)
	if !ok {
		return nil, fmt.Errorf("unexpected relay message in isolated client wire")
	}
	return msg, nil
}

func TestDHCPv6WireObserverRecognizesRelease(t *testing.T) {
	msg, err := dhcpv6.NewMessage()
	if err != nil {
		t.Fatal(err)
	}
	msg.MessageType = dhcpv6.MessageTypeRelease
	data := msg.ToBytes()
	frame := make([]byte, 48+len(data))
	frame[0], frame[6] = 0x60, syscall.IPPROTO_UDP
	binary.BigEndian.PutUint16(frame[4:6], uint16(8+len(data)))
	binary.BigEndian.PutUint16(frame[40:42], 546)
	binary.BigEndian.PutUint16(frame[42:44], 547)
	binary.BigEndian.PutUint16(frame[44:46], uint16(8+len(data)))
	copy(frame[48:], data)
	got, err := decodeDHCPv6ClientWire(frame)
	if err != nil || got == nil || got.MessageType != dhcpv6.MessageTypeRelease || got.TransactionID != msg.TransactionID {
		t.Fatalf("synthetic RELEASE decoder control failed: %v", err)
	}
	for _, invalid := range [][]byte{nil, frame[:44], append([]byte(nil), frame...)} {
		if len(invalid) >= 48 {
			binary.BigEndian.PutUint16(invalid[44:46], 65535)
		}
		if got, _ := decodeDHCPv6ClientWire(invalid); got != nil {
			t.Fatal("invalid wire decoder control accepted")
		}
	}
	wrongPort := append([]byte(nil), frame...)
	binary.BigEndian.PutUint16(wrongPort[42:44], 546)
	if got, err := decodeDHCPv6ClientWire(wrongPort); got != nil || err != nil {
		t.Fatal("non-server-directed packet accepted by wire observer")
	}
}

func TestDHCPv6PublicStartAcquiresLease(t *testing.T) {
	checkDHCPv6PublicStop(t, dhcpv6AcquireStop)
}

func TestDHCPv6StopReleasesActiveLease(t *testing.T) {
	checkDHCPv6PublicStop(t, dhcpv6ActiveRelease)
}

func TestDHCPv6StopPreservesOtherActiveClientOnSharedParent(t *testing.T) {
	checkDHCPv6PublicStop(t, dhcpv6SharedActiveParent)
}

type dhcpv6StopCase uint8

const (
	dhcpv6AcquireStop dhcpv6StopCase = iota
	dhcpv6ActiveRelease
	dhcpv6SharedActiveParent
)

type dhcpv6ObservedReply struct {
	request *dhcpv6.Message
	at      time.Time
}

func checkDHCPv6PublicStop(t *testing.T, scenario dhcpv6StopCase) {
	t.Helper()
	requireRelease := scenario != dhcpv6AcquireStop
	sharedParent := scenario == dhcpv6SharedActiveParent
	if os.Getenv("KUBE_VIP_ISOLATED_DHCP_TEST") != "1" {
		t.Skip("requires explicitly isolated DHCP test namespace")
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	links, err := netlink.LinkList()
	must(err)
	if len(links) != 1 || links[0].Attrs().Name != "lo" {
		t.Fatal("DHCPv6 fixture requires initially only loopback, never host networking")
	}
	must(netlink.LinkSetUp(links[0]))
	pair := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: "kv6-dhcp-c"}, PeerName: "kv6-dhcp-s"}
	must(netlink.LinkAdd(pair))
	t.Cleanup(func() { _ = netlink.LinkDel(pair) })
	clientLink, err := netlink.LinkByName("kv6-dhcp-c")
	must(err)
	peerLink, err := netlink.LinkByName("kv6-dhcp-s")
	must(err)
	for _, link := range []netlink.Link{clientLink, peerLink} {
		must(netlink.LinkSetUp(link))
	}
	for index, link := range []netlink.Link{clientLink, peerLink} {
		addr, err := netlink.ParseAddr([]string{"fe80::2/64", "fe80::1/64"}[index])
		must(err)
		addr.Flags = syscall.IFA_F_NODAD
		must(netlink.AddrAdd(link, addr))
	}
	clientIface, err := net.InterfaceByName(clientLink.Attrs().Name)
	must(err)
	peerIface, err := net.InterfaceByName(peerLink.Attrs().Name)
	must(err)
	local, err := dhcpv6.GetLinkLocalAddr(clientIface.Name)
	must(err)
	if !local.Equal(net.ParseIP("fe80::2")) {
		t.Fatalf("DHCPv6 fixture lacks expected native ready link-local address: %s", local)
	}
	fingerprint := func() string {
		t.Helper()
		var state []string
		for _, original := range []netlink.Link{clientLink, peerLink} {
			link, err := netlink.LinkByIndex(original.Attrs().Index)
			must(err)
			attrs := link.Attrs()
			addrs, err := netlink.AddrList(link, netlink.FAMILY_V6)
			must(err)
			var ips []string
			for _, addr := range addrs {
				ips = append(ips, addr.IPNet.String())
			}
			sort.Strings(ips)
			state = append(state, fmt.Sprintf("%s/%d/%s/%s/%d/%d/%v", attrs.Name, attrs.Index, link.Type(), attrs.HardwareAddr, attrs.MTU, attrs.Flags, ips))
		}
		return fmt.Sprint(state)
	}
	before := fingerprint()
	peerErrors := make(chan error, 8)
	report := func(err error) {
		select {
		case peerErrors <- err:
		default:
		}
	}
	raw, err := packet.Listen(peerIface, packet.Datagram, syscall.ETH_P_IPV6, nil)
	must(err)
	rawStop, rawDone := make(chan struct{}), make(chan struct{})
	rawRequests := make(chan *dhcpv6.Message, 16)
	var rawReleaseCount atomic.Uint32
	go func() {
		defer close(rawDone)
		buf := make([]byte, 65536)
		for {
			n, _, err := raw.ReadFrom(buf)
			if err != nil {
				select {
				case <-rawStop:
					return
				default:
					report(fmt.Errorf("raw DHCPv6 peer read: %w", err))
					return
				}
			}
			msg, err := decodeDHCPv6ClientWire(buf[:n])
			if err != nil {
				report(fmt.Errorf("raw DHCPv6 peer decode: %w", err))
				return
			}
			if msg == nil {
				continue
			}
			if msg.MessageType == dhcpv6.MessageTypeRelease {
				rawReleaseCount.Add(1)
			}
			select {
			case rawRequests <- msg:
			default:
				report(fmt.Errorf("raw DHCPv6 peer queue overflow"))
				return
			}
		}
	}()
	t.Cleanup(func() {
		close(rawStop)
		_ = raw.Close()
		select {
		case <-rawDone:
		case <-time.After(2 * time.Second):
			t.Error("raw DHCPv6 peer did not stop")
		}
	})
	leaseIP := net.ParseIP("2001:db8:1::20")
	lease := dhcpv6.OptIAAddress{IPv6Addr: leaseIP, PreferredLifetime: 8 * time.Second, ValidLifetime: 8 * time.Second}
	clientID := &dhcpv6.DUIDEN{EnterpriseNumber: 1, EnterpriseIdentifier: []byte("public-v6-proof")}
	otherClientID := &dhcpv6.DUIDEN{EnterpriseNumber: 1, EnterpriseIdentifier: []byte("public-v6-other")}
	otherLeaseIP := net.ParseIP("2001:db8:1::21")
	serverID := &dhcpv6.DUIDEN{EnterpriseNumber: 32473, EnterpriseIdentifier: []byte("synthetic-v6-peer")}
	peerRequests := make(chan *dhcpv6.Message, 16)
	ackSent := make(chan time.Time, 1)
	otherReplies := make(chan dhcpv6ObservedReply, 4)
	peerStop, peerDone := make(chan struct{}), make(chan error, 1)
	peerUDP, err := server6.NewIPv6UDPConn(peerIface.Name, &net.UDPAddr{IP: net.IPv6unspecified, Port: 547})
	must(err)
	peerRaw, err := packet.Listen(peerIface, packet.Datagram, syscall.ETH_P_IPV6, nil)
	if err != nil {
		_ = peerUDP.Close()
		must(err)
	}
	peerConn := &dhcpv6SyntheticPeerConn{PacketConn: peerUDP, raw: peerRaw, zone: peerIface.Name}
	server, err := server6.NewServer(peerIface.Name, nil, func(conn net.PacketConn, peer net.Addr, message dhcpv6.DHCPv6) {
		msg, ok := message.(*dhcpv6.Message)
		if !ok || msg.Options.ClientID() == nil || msg.Options.OneIANA() == nil {
			report(fmt.Errorf("invalid synthetic DHCPv6 client DUID/IANA control"))
			return
		}
		offeredLease, expectedLeaseIP := lease, leaseIP
		if !bytes.Equal(msg.Options.ClientID().ToBytes(), clientID.ToBytes()) {
			if !sharedParent || !bytes.Equal(msg.Options.ClientID().ToBytes(), otherClientID.ToBytes()) {
				report(fmt.Errorf("invalid synthetic DHCPv6 client DUID/IANA control"))
				return
			}
			offeredLease.IPv6Addr, expectedLeaseIP = otherLeaseIP, otherLeaseIP
		}
		select {
		case peerRequests <- msg:
		default:
			report(fmt.Errorf("synthetic DHCPv6 request queue overflow"))
			return
		}
		mods := []dhcpv6.Modifier{dhcpv6.WithServerID(serverID), dhcpv6.WithIAID(msg.Options.OneIANA().IaId)}
		var reply *dhcpv6.Message
		var err error
		switch msg.MessageType {
		case dhcpv6.MessageTypeSolicit:
			reply, err = dhcpv6.NewAdvertiseFromSolicit(msg, append(mods, dhcpv6.WithIANA(offeredLease))...)
		case dhcpv6.MessageTypeRequest, dhcpv6.MessageTypeRenew, dhcpv6.MessageTypeRelease:
			if msg.Options.ServerID() == nil || !bytes.Equal(msg.Options.ServerID().ToBytes(), serverID.ToBytes()) {
				report(fmt.Errorf("synthetic DHCPv6 request lacks selected server identity"))
				return
			}
			if msg.MessageType == dhcpv6.MessageTypeRenew || msg.MessageType == dhcpv6.MessageTypeRelease {
				addr := msg.Options.OneIANA().Options.OneAddress()
				if addr == nil || !addr.IPv6Addr.Equal(expectedLeaseIP) {
					report(fmt.Errorf("synthetic DHCPv6 RENEW/RELEASE lacks original lease"))
					return
				}
			}
			if msg.MessageType == dhcpv6.MessageTypeRelease {
				mods = append(mods, dhcpv6.WithOption(&dhcpv6.OptStatusCode{StatusCode: iana.StatusSuccess}))
			} else {
				mods = append(mods, dhcpv6.WithIANA(offeredLease))
			}
			reply, err = dhcpv6.NewReplyFromMessage(msg, mods...)
		default:
			report(fmt.Errorf("unexpected DHCPv6 peer message before original T1: %s", msg.MessageType))
			return
		}
		if err == nil {
			_, err = conn.WriteTo(reply.ToBytes(), peer)
		}
		if err != nil {
			select {
			case <-peerStop:
				return
			default:
				report(fmt.Errorf("synthetic DHCPv6 reply: %w", err))
			}
			return
		}
		if msg.MessageType == dhcpv6.MessageTypeRequest || msg.MessageType == dhcpv6.MessageTypeRenew {
			if bytes.Equal(msg.Options.ClientID().ToBytes(), otherClientID.ToBytes()) {
				select {
				case otherReplies <- dhcpv6ObservedReply{request: msg, at: time.Now()}:
				default:
					report(fmt.Errorf("synthetic shared DHCPv6 REPLY queue overflow"))
				}
			} else {
				select {
				case ackSent <- time.Now():
				default:
				}
			}
		}
	}, server6.WithConn(peerConn))
	if err != nil {
		_ = peerConn.Close()
	}
	must(err)
	go func() { peerDone <- server.Serve() }()
	t.Cleanup(func() {
		close(peerStop)
		_ = server.Close()
		select {
		case <-peerDone:
		case <-time.After(2 * time.Second):
			t.Error("synthetic DHCPv6 server did not stop")
		}
	})
	client, err := NewDHCPv6Client(clientIface, clientLink, false, "", 2)
	must(err)
	client.WithHostName("public-v6-proof")
	ctx, cancel := context.WithCancel(context.Background())
	startResult, startDone := make(chan error, 1), make(chan struct{})
	errorStop, errorDone := make(chan struct{}), make(chan struct{})
	clientErrors := make(chan error, 8)
	go func() {
		defer close(errorDone)
		for {
			select {
			case err := <-client.ErrorChannel():
				select {
				case clientErrors <- err:
				default:
					report(fmt.Errorf("public DHCPv6 error receiver overflow"))
				}
			case <-errorStop:
				return
			}
		}
	}()
	go func() { defer close(startDone); startResult <- client.Start(ctx) }()
	stopped := false
	t.Cleanup(func() {
		cancel()
		if !stopped {
			client.Stop()
		}
		select {
		case <-startDone:
		case <-time.After(2 * time.Second):
			t.Error("real DHCPv6 Start ignored fixture teardown cancellation")
		}
		close(errorStop)
		select {
		case <-errorDone:
		case <-time.After(2 * time.Second):
			t.Error("public DHCPv6 error receiver did not stop")
		}
	})
	select {
	case ip := <-client.IPChannel():
		if !net.ParseIP(ip).Equal(leaseIP) {
			t.Fatalf("real public DHCPv6 client delivered wrong lease: %s", ip)
		}
	case err := <-peerErrors:
		t.Fatalf("synthetic DHCPv6 acquisition peer failed: %v", err)
	case err := <-clientErrors:
		t.Fatalf("real DHCPv6 acquisition errored before lease control: %v", err)
	case err := <-startResult:
		t.Fatalf("real DHCPv6 Start ended before acquisition control: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("real DHCPv6 client acquired no lease; protocol fixture not validated")
	}
	var ackAt time.Time
	select {
	case ackAt = <-ackSent:
	case <-time.After(time.Second):
		t.Fatal("synthetic DHCPv6 peer lacks actual original REPLY timestamp")
	}
	var originalIAID [4]byte
	for _, kind := range []dhcpv6.MessageType{dhcpv6.MessageTypeSolicit, dhcpv6.MessageTypeRequest} {
		var wire, peer *dhcpv6.Message
		select {
		case wire = <-rawRequests:
		case err := <-peerErrors:
			t.Fatalf("raw DHCPv6 initial control failed: %v", err)
		case <-time.After(time.Second):
			t.Fatal("raw DHCPv6 observer missed initial receive control")
		}
		select {
		case peer = <-peerRequests:
		case <-time.After(time.Second):
			t.Fatal("synthetic raw DHCPv6 peer missed initial receive control")
		}
		if wire.MessageType != kind || peer.MessageType != kind || !bytes.Equal(wire.ToBytes(), peer.ToBytes()) || peer.Options.OneIANA() == nil {
			t.Fatal("independent wire/peer DHCPv6 transaction/DUID/IANA controls disagree")
		}
		if kind == dhcpv6.MessageTypeRequest {
			originalIAID = peer.Options.OneIANA().IaId
		}
	}
	if ctx.Err() != nil || time.Since(ackAt) >= 2*time.Second || fingerprint() != before || rawReleaseCount.Load() != 0 {
		t.Fatal("DHCPv6 original native/lease/caller/pre-T1 controls invalid")
	}
	var otherClient *DHCPv6Client
	var otherCtx context.Context
	var otherDone chan struct{}
	var otherResult, otherErrors chan error
	var otherAckAt time.Time
	var otherIAID [4]byte
	otherStopped := false
	var readOtherPair func(dhcpv6.MessageType, bool) *dhcpv6.Message
	if sharedParent {
		otherClient, err = NewDHCPv6Client(clientIface, clientLink, false, "", 2)
		must(err)
		otherClient.WithHostName("public-v6-other")
		var otherCancel context.CancelFunc
		otherCtx, otherCancel = context.WithCancel(context.Background())
		otherDone, otherResult, otherErrors = make(chan struct{}), make(chan error, 1), make(chan error, 8)
		otherErrorStop, otherErrorDone := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(otherErrorDone)
			for {
				select {
				case err := <-otherClient.ErrorChannel():
					select {
					case otherErrors <- err:
					default:
						report(fmt.Errorf("shared DHCPv6 error receiver overflow"))
					}
				case <-otherErrorStop:
					return
				}
			}
		}()
		go func() { defer close(otherDone); otherResult <- otherClient.Start(otherCtx) }()
		t.Cleanup(func() {
			otherCancel()
			if !otherStopped {
				otherClient.Stop()
			}
			select {
			case <-otherDone:
			case <-time.After(2 * time.Second):
				t.Error("other real DHCPv6 Start ignored fixture teardown cancellation")
			}
			close(otherErrorStop)
			select {
			case <-otherErrorDone:
			case <-time.After(2 * time.Second):
				t.Error("shared DHCPv6 error receiver did not stop")
			}
		})
		readOtherPair = func(kind dhcpv6.MessageType, rejectStoppedOwner bool) *dhcpv6.Message {
			t.Helper()
			var messages [2]*dhcpv6.Message
			for index, incoming := range []<-chan *dhcpv6.Message{rawRequests, peerRequests} {
				timer := time.NewTimer(time.Second)
			receive:
				for {
					select {
					case msg := <-incoming:
						if msg.Options.ClientID() != nil && bytes.Equal(msg.Options.ClientID().ToBytes(), otherClientID.ToBytes()) {
							if msg.MessageType != kind || msg.Options.OneIANA() == nil {
								t.Fatal("other DHCPv6 client wire sequence/IANA control invalid")
							}
							messages[index] = msg
							break receive
						}
						if index == 0 && rejectStoppedOwner {
							t.Fatal("stopped DHCPv6 owner sent later traffic during shared-parent control")
						}
					case err := <-peerErrors:
						t.Fatalf("shared DHCPv6 wire/peer control failed: %v", err)
					case err := <-otherErrors:
						t.Fatalf("other DHCPv6 client errored during shared wire control: %v", err)
					case <-otherDone:
						t.Fatalf("other DHCPv6 Start ended during shared wire control: %v", <-otherResult)
					case <-timer.C:
						t.Fatal("other DHCPv6 raw/peer receive control timed out")
					}
				}
				timer.Stop()
			}
			if !bytes.Equal(messages[0].ToBytes(), messages[1].ToBytes()) {
				t.Fatal("other DHCPv6 independent wire/peer transaction/DUID/IANA controls disagree")
			}
			return messages[1]
		}
		select {
		case ip := <-otherClient.IPChannel():
			if !net.ParseIP(ip).Equal(otherLeaseIP) {
				t.Fatalf("other real DHCPv6 client delivered wrong distinct lease: %s", ip)
			}
		case err := <-peerErrors:
			t.Fatalf("shared DHCPv6 acquisition peer failed: %v", err)
		case err := <-otherErrors:
			t.Fatalf("other real DHCPv6 acquisition failed: %v", err)
		case <-otherDone:
			t.Fatalf("other real DHCPv6 Start ended before lease control: %v", <-otherResult)
		case <-time.After(3 * time.Second):
			t.Fatal("other real DHCPv6 client acquired no distinct lease")
		}
		var reply dhcpv6ObservedReply
		select {
		case reply = <-otherReplies:
		case <-time.After(time.Second):
			t.Fatal("shared peer lacks actual second-owner REPLY control")
		}
		readOtherPair(dhcpv6.MessageTypeSolicit, false)
		request := readOtherPair(dhcpv6.MessageTypeRequest, false)
		if !bytes.Equal(request.ToBytes(), reply.request.ToBytes()) {
			t.Fatal("shared peer REPLY did not correspond to second-owner raw transaction")
		}
		otherAckAt, otherIAID = reply.at, request.Options.OneIANA().IaId
		if ctx.Err() != nil || otherCtx.Err() != nil || fingerprint() != before || time.Since(ackAt) >= 2*time.Second || time.Since(otherAckAt) >= 2*time.Second || rawReleaseCount.Load() != 0 {
			t.Fatal("two active DHCPv6 owner native/caller/distinct-lease/pre-T1 controls invalid")
		}
		t.Log("two real DHCPv6 clients on one native parent acquired distinct DUID/8s leases; independent raw/peer transactions/server/IAID agree; both callers and original interfaces live before T1")
	}
	t.Log("real public DHCPv6 Start delivered original8s lease; independent wire observer and raw peer initial SOLICIT/REQUEST transaction/DUID/server/IAID controls agree; original interfaces and caller live before T1")
	stopped = true
	stopDone := make(chan struct{})
	go func() {
		client.Stop()
		if sharedParent {
			client.Stop()
		}
		close(stopDone)
	}()
	select {
	case <-stopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("real DHCPv6 Stop did not return with caller live")
	}
	select {
	case <-startDone:
	case <-time.After(2 * time.Second):
		t.Fatal("real DHCPv6 Start did not complete after Stop with caller live")
	}
	startErr := <-startResult
	if !requireRelease {
		if ctx.Err() != nil || fingerprint() != before {
			t.Fatal("DHCPv6 acquisition control lost native interface/caller")
		}
		t.Logf("public DHCPv6 acquisition/Stop completion control passed; RELEASE not certified by this control; Start=%v", startErr)
		return
	}
	var released *dhcpv6.Message
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
releaseWait:
	for {
		select {
		case msg := <-rawRequests:
			if msg.MessageType == dhcpv6.MessageTypeRelease {
				released = msg
				break releaseWait
			}
		case err := <-peerErrors:
			t.Fatalf("synthetic DHCPv6 Stop wire peer failed: %v", err)
		case <-timer.C:
			break releaseWait
		}
	}
	select {
	case err := <-peerErrors:
		t.Fatalf("synthetic DHCPv6 Stop observer failed: %v", err)
	default:
	}
	if ctx.Err() != nil || time.Since(ackAt) >= 4*time.Second || fingerprint() != before {
		t.Fatal("DHCPv6 Stop test required caller cancellation/expiry or native change")
	}
	t.Logf("real DHCPv6 Stop and Start completed before T1 with caller/original native interfaces live; raw RELEASEs=%d Start=%v", rawReleaseCount.Load(), startErr)
	if released == nil {
		t.Fatal("real DHCPv6 Stop ended without releasing active lease; actual raw receive controls and unchanged original native interfaces/caller verified")
	}
	addr := released.Options.OneIANA()
	if startErr != nil || addr == nil || addr.IaId != originalIAID || addr.Options.OneAddress() == nil || !addr.Options.OneAddress().IPv6Addr.Equal(leaseIP) ||
		released.Options.ClientID() == nil || !bytes.Equal(released.Options.ClientID().ToBytes(), clientID.ToBytes()) ||
		released.Options.ServerID() == nil || !bytes.Equal(released.Options.ServerID().ToBytes(), serverID.ToBytes()) || rawReleaseCount.Load() != 1 {
		t.Fatalf("real DHCPv6 RELEASE did not match original lease/DUID/server/IAID or clean Start completion: %v", startErr)
	}
	t.Log("real DHCPv6 Stop delivered one matching active-lease RELEASE and completed Start with original native interfaces/caller live")
	if !sharedParent {
		return
	}
	if otherCtx.Err() != nil || ctx.Err() != nil || fingerprint() != before {
		t.Fatal("shared DHCPv6 caller/native controls invalid after first owner Stop")
	}
	var renewal dhcpv6ObservedReply
	select {
	case renewal = <-otherReplies:
	case <-otherDone:
		t.Fatalf("stopping first DHCPv6 owner ended other live shared-parent Start: %v", <-otherResult)
	case err := <-otherErrors:
		t.Fatalf("other DHCPv6 owner errored after first Stop: %v", err)
	case err := <-peerErrors:
		t.Fatalf("shared DHCPv6 renewal peer failed: %v", err)
	case <-time.After(6 * time.Second):
		t.Fatal("other DHCPv6 owner sent no actual renewal after first owner Stop")
	}
	readOtherPair(dhcpv6.MessageTypeSolicit, true)
	renewRequest := readOtherPair(dhcpv6.MessageTypeRenew, true)
	if !bytes.Equal(renewRequest.ToBytes(), renewal.request.ToBytes()) || renewRequest.Options.OneIANA().IaId != otherIAID || renewal.at.Sub(otherAckAt) < 3*time.Second {
		t.Fatal("other DHCPv6 renewal lacks independent raw/peer/REPLY transaction and IAID/timing correlation")
	}
	oldExpiry := time.NewTimer(time.Until(otherAckAt.Add(8*time.Second + 250*time.Millisecond)))
	defer oldExpiry.Stop()
	select {
	case <-otherDone:
		t.Fatalf("other DHCPv6 shared-parent lease expired despite real renewal after first Stop: %v", <-otherResult)
	case err := <-otherErrors:
		t.Fatalf("other DHCPv6 owner errored before original expiry survival control: %v", err)
	case err := <-peerErrors:
		t.Fatalf("shared DHCPv6 renewal observer failed: %v", err)
	case <-oldExpiry.C:
	}
	if ctx.Err() != nil || otherCtx.Err() != nil || fingerprint() != before || time.Now().After(renewal.at.Add(8*time.Second)) || rawReleaseCount.Load() != 1 {
		t.Fatal("shared DHCPv6 old-expiry survival lacked live original native/callers/renewed lease or first-owner-only RELEASE")
	}
	t.Log("first DHCPv6 owner Stop/Start completed with matching RELEASE1; other active shared-parent owner received correlated renewal and survived original8s expiry with both callers/original interfaces live")
	otherStopped = true
	otherStopDone := make(chan struct{})
	go func() { otherClient.Stop(); otherClient.Stop(); close(otherStopDone) }()
	select {
	case <-otherStopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("other real DHCPv6 Stop did not return")
	}
	select {
	case <-otherDone:
	case <-time.After(2 * time.Second):
		t.Fatal("other real DHCPv6 Start did not finish after its Stop")
	}
	otherStartErr := <-otherResult
	var otherReleased *dhcpv6.Message
	timer2 := time.NewTimer(2 * time.Second)
	defer timer2.Stop()
otherReleaseWait:
	for {
		select {
		case msg := <-rawRequests:
			if msg.Options.ClientID() == nil || !bytes.Equal(msg.Options.ClientID().ToBytes(), otherClientID.ToBytes()) {
				t.Fatal("stopped first owner sent later shared-parent wire traffic")
			}
			if msg.MessageType == dhcpv6.MessageTypeRelease {
				otherReleased = msg
				break otherReleaseWait
			}
		case err := <-peerErrors:
			t.Fatalf("other DHCPv6 RELEASE peer failed: %v", err)
		case <-timer2.C:
			break otherReleaseWait
		}
	}
	if otherStartErr != nil || otherReleased == nil {
		t.Fatalf("other real DHCPv6 owner Stop failed matching wire RELEASE/clean Start completion: %v", otherStartErr)
	}
	otherAddr := otherReleased.Options.OneIANA()
	if otherAddr == nil || otherAddr.IaId != otherIAID || otherAddr.Options.OneAddress() == nil || !otherAddr.Options.OneAddress().IPv6Addr.Equal(otherLeaseIP) || otherReleased.Options.ServerID() == nil || !bytes.Equal(otherReleased.Options.ServerID().ToBytes(), serverID.ToBytes()) || rawReleaseCount.Load() != 2 || ctx.Err() != nil || otherCtx.Err() != nil || fingerprint() != before {
		t.Fatal("other shared-parent RELEASE did not preserve distinct lease/DUID/server/IAID/native/caller/count controls")
	}
	select {
	case err := <-peerErrors:
		t.Fatalf("final shared DHCPv6 observer failed: %v", err)
	default:
	}
	t.Log("real shared-parent Stop released each distinct owner's lease exactly once; second owner renewed past original expiry and final matching RELEASE2 completed both Starts with original native interfaces/callers live")
}

func TestDHCPv6PublicConstructorRecreatesInterfaceWithSameName(t *testing.T) {
	if os.Getenv("KUBE_VIP_ISOLATED_DHCP_TEST") != "1" {
		t.Fatal("requires explicit isolated network-none constructor test")
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	links, err := netlink.LinkList()
	must(err)
	if len(links) != 1 || links[0].Attrs().Name != "lo" {
		t.Fatal("constructor recreation fixture requires initially only loopback")
	}
	must(netlink.LinkSetUp(links[0]))
	const clientName, peerName = "kv6-ctor-c", "kv6-ctor-s"
	var current netlink.Link
	t.Cleanup(func() {
		if current != nil {
			_ = netlink.LinkDel(current)
		}
	})
	create := func() (netlink.Link, *net.Interface) {
		t.Helper()
		must(netlink.LinkAdd(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: clientName}, PeerName: peerName}))
		current, err = netlink.LinkByName(clientName)
		must(err)
		peer, err := netlink.LinkByName(peerName)
		must(err)
		for _, link := range []netlink.Link{current, peer} {
			must(netlink.LinkSetUp(link))
		}
		for i, link := range []netlink.Link{current, peer} {
			addr, err := netlink.ParseAddr([]string{"fe80::2/64", "fe80::1/64"}[i])
			must(err)
			addr.Flags = syscall.IFA_F_NODAD
			must(netlink.AddrAdd(link, addr))
		}
		addresses, err := netlink.AddrList(current, netlink.FAMILY_V6)
		must(err)
		ready := false
		for _, addr := range addresses {
			if addr.IP.Equal(net.ParseIP("fe80::2")) && addr.Flags&syscall.IFA_F_NODAD != 0 && addr.Flags&(syscall.IFA_F_TENTATIVE|syscall.IFA_F_DADFAILED) == 0 {
				ready = true
			}
		}
		iface, err := net.InterfaceByName(clientName)
		must(err)
		selected, err := dhcpv6.GetLinkLocalAddr(clientName)
		must(err)
		if !ready || !selected.Equal(net.ParseIP("fe80::2")) || iface.Index != current.Attrs().Index || iface.Index <= 0 || current.Type() != "veth" || iface.HardwareAddr.String() != current.Attrs().HardwareAddr.String() || iface.Flags&net.FlagUp == 0 {
			t.Fatal("constructor fixture lacks matching current ready native interface/address identity")
		}
		return current, iface
	}
	// Direct indexed kernel control must not refresh Go's name/zone cache.
	nativeBind := func(index int) error {
		t.Helper()
		fd, err := syscall.Socket(syscall.AF_INET6, syscall.SOCK_DGRAM|syscall.SOCK_CLOEXEC, syscall.IPPROTO_UDP)
		must(err)
		addr := &syscall.SockaddrInet6{Port: 546, ZoneId: uint32(index)}
		copy(addr.Addr[:], net.ParseIP("fe80::2").To16())
		bindErr := syscall.Bind(fd, addr)
		if bindErr == nil {
			bound, err := syscall.Getsockname(fd)
			must(err)
			bound6, ok := bound.(*syscall.SockaddrInet6)
			if !ok || bound6.ZoneId != uint32(index) || bound6.Port != 546 {
				t.Fatal("native indexed UDP6 control bound the wrong interface or port")
			}
		}
		must(syscall.Close(fd))
		return bindErr
	}
	original, iface := create()
	first, err := NewDHCPv6Client(iface, original, false, "", 1)
	must(err)
	t.Cleanup(first.Stop)
	if err := nativeBind(iface.Index); !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("real first public constructor did not reserve native UDP546: %v", err)
	}
	primedAt := time.Now()
	first.Stop()
	first.Stop()
	must(nativeBind(iface.Index))
	oldIndex := original.Attrs().Index
	must(netlink.LinkDel(original))
	_, lookupErr := netlink.LinkByIndex(oldIndex)
	var missing netlink.LinkNotFoundError
	if !errors.As(lookupErr, &missing) {
		t.Fatalf("original native interface index did not disappear: %v", lookupErr)
	}
	recreated, replacementIface := create()
	if replacementIface.Index == oldIndex {
		t.Fatal("recreated native interface did not receive a distinct positive index")
	}
	must(nativeBind(replacementIface.Index))
	must(t.Context().Err())
	if time.Since(primedAt) >= time.Minute {
		t.Fatal("constructor fixture overran observed Go interface-zone cache window")
	}
	t.Logf("real public constructor reserved UDP546 and repeated Stop freed original native transport; old index typed missing; same-name new index/ready link-local/native identity and direct indexed bind/caller/cache-window controls verified; oldIndex=%d newIndex=%d", oldIndex, replacementIface.Index)
	replacement, err := NewDHCPv6Client(replacementIface, recreated, false, "", 1)
	if err != nil {
		must(nativeBind(replacementIface.Index))
		must(t.Context().Err())
		if !errors.Is(err, syscall.ENODEV) {
			t.Fatalf("unexpected constructor failure after valid native recreation: %v", err)
		}
		t.Fatalf("real public DHCPv6 constructor refused same-name native interface recreation after closed original transport; current indexed bind/address/identity/caller controls verified: %v", err)
	}
	t.Cleanup(replacement.Stop)
	if err := nativeBind(replacementIface.Index); !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("recreated public constructor failed to reserve current native UDP546: %v", err)
	}
	replacement.Stop()
	replacement.Stop()
	must(nativeBind(replacementIface.Index))
	must(t.Context().Err())
	t.Log("real public DHCPv6 constructor recreated same-name interface on fresh native index and repeated Stop released its real UDP6 socket; original indexed identity absent/caller live")
}
