package vip

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/nclient4"
	"github.com/insomniacslk/dhcp/dhcpv4/server4"
	"github.com/vishvananda/netlink"
	"golang.org/x/net/ipv4"
)

func TestDHCPv4BackoffExhaustionDoesNotDeadlock(t *testing.T) {
	// DEFECT: requestWithBackoff calls Stop from the Start/request goroutine before Start can close releasedChan, so exhausted DHCPv4 retries deadlock (pkg/vip/dhcpv4.go:229).
	client := NewDHCPv4Client(
		&net.Interface{Name: "definitely-not-a-kube-vip-interface"},
		false,
		"",
		1,
		false,
	)

	done := make(chan struct{})
	go func() {
		_, _ = client.requestWithBackoff(context.Background())
		close(done)
	}()

	select {
	case err := <-client.ErrorChannel():
		if err == nil {
			t.Fatal("expected DHCPv4 request error")
		}
	case <-time.After(time.Second):
		t.Fatal("DHCPv4 did not report exhausted backoff")
	}

	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("DHCPv4 backoff exhaustion deadlocked")
	}
}

func TestDHCPv4StopBeforeStartCancelsAcquisition(t *testing.T) {
	client := NewDHCPv4Client(&net.Interface{Name: "definitely-not-a-kube-vip-interface"}, false, "", 0, false)
	ctx, cancel := context.WithCancel(context.Background())
	client.Stop()
	stopped := make(chan struct{})
	var result error
	go func() {
		result = client.Start(ctx)
		close(stopped)
	}()
	t.Cleanup(func() {
		cancel()
		client.Stop()
		select {
		case <-stopped:
		case <-time.After(2 * time.Second):
			t.Error("real DHCP client ignored teardown cancellation")
		}
	})

	select {
	case <-stopped:
		t.Logf("pre-stopped real DHCP client returned: %v", result)
	case <-time.After(time.Second):
		t.Fatal("Stop before Start did not cancel real DHCP acquisition/backoff")
	}
}

func TestDHCPv4ActiveLeaseExpiryWithoutErrorReceiver(t *testing.T) {
	checkDHCPv4LeaseExpiry(t, dhcpv4LeaseScenario{})
}

func TestDHCPv4ActiveLeaseNakExhaustionWithoutErrorReceiver(t *testing.T) {
	checkDHCPv4LeaseExpiry(t, dhcpv4LeaseScenario{nakRequests: true})
}

func TestDHCPv4RenewalExtendsLeasePastOldExpiry(t *testing.T) {
	checkDHCPv4LeaseExpiry(t, dhcpv4LeaseScenario{renewBeforeLoss: true})
}

func TestDHCPv4RenewalDoesNotUseChangedInterfaceOwner(t *testing.T) {
	checkDHCPv4LeaseExpiry(t, dhcpv4LeaseScenario{changeOwnerBeforeRenewal: true})
}

func TestDHCPv4RebindDoesNotUseChangedInterfaceOwner(t *testing.T) {
	checkDHCPv4LeaseExpiry(t, dhcpv4LeaseScenario{changeOwnerBeforeRebind: true})
}

func TestDHCPv4RejectsMismatchedRenewalACKWithoutExtendingLease(t *testing.T) {
	checkDHCPv4LeaseExpiry(t, dhcpv4LeaseScenario{mismatchedRenewalACK: true})
}

func TestDHCPv4RejectsMismatchedRebindACKWithoutExtendingLease(t *testing.T) {
	checkDHCPv4LeaseExpiry(t, dhcpv4LeaseScenario{mismatchedRenewalACK: true, mismatchedRebindACK: true})
}

func TestDHCPv4LeaseExpiryWithoutIPReceiver(t *testing.T) {
	checkDHCPv4LeaseExpiry(t, dhcpv4LeaseScenario{blockIPDelivery: true})
}

func TestDHCPv4DelayedIPReceiverDoesNotRestartLease(t *testing.T) {
	checkDHCPv4LeaseExpiry(t, dhcpv4LeaseScenario{ipReceiveDelay: 2 * time.Second})
}

func TestDHCPv4StopCancelsActiveAcquisition(t *testing.T) {
	checkDHCPv4LeaseExpiry(t, dhcpv4LeaseScenario{stopDuringAcquisition: true})
}

func TestDHCPv4StopCancelsActiveRenewal(t *testing.T) {
	checkDHCPv4LeaseExpiry(t, dhcpv4LeaseScenario{stopDuringRenewal: true})
}

func TestDHCPv4StopCancelsRejectedAcquisitionRetry(t *testing.T) {
	checkDHCPv4LeaseExpiry(t, dhcpv4LeaseScenario{stopAfterRejection: true})
}

type dhcpv4LeaseScenario struct {
	mismatchedRebindACK                           bool
	mismatchedRenewalACK                          bool
	changeOwnerBeforeRebind                       bool
	changeOwnerBeforeRenewal                      bool
	nakRequests, renewBeforeLoss, blockIPDelivery bool
	ipReceiveDelay                                time.Duration
	stopDuringAcquisition, stopDuringRenewal      bool
	stopAfterRejection                            bool
}

func checkDHCPv4LeaseExpiry(t *testing.T, scenario dhcpv4LeaseScenario) {
	t.Helper()
	nakRequests, renewBeforeLoss, blockIPDelivery := scenario.nakRequests, scenario.renewBeforeLoss, scenario.blockIPDelivery
	ipReceiveDelay, stopDuringAcquisition := scenario.ipReceiveDelay, scenario.stopDuringAcquisition
	if os.Getenv("KUBE_VIP_ISOLATED_DHCP_TEST") != "1" {
		t.Skip("requires a dedicated network-none Linux test process")
	}
	links, err := netlink.LinkList()
	if err != nil {
		t.Fatal(err)
	}
	for _, link := range links {
		if link.Attrs().Name != "lo" {
			t.Fatal("DHCP integration test requires an isolated namespace containing only loopback")
		}
	}

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(netlink.LinkAdd(&netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{Name: "kv-dhcp-c"},
		PeerName:  "kv-dhcp-s",
	}))
	clientLink, err := netlink.LinkByName("kv-dhcp-c")
	must(err)
	t.Cleanup(func() { _ = netlink.LinkDel(clientLink) })
	serverLink, err := netlink.LinkByName("kv-dhcp-s")
	must(err)
	must(netlink.LinkSetUp(clientLink))
	must(netlink.LinkSetUp(serverLink))
	serverAddr, err := netlink.ParseAddr("192.0.2.1/24")
	must(err)
	must(netlink.AddrAdd(serverLink, serverAddr))

	leaseTime := 4 * time.Second
	if scenario.changeOwnerBeforeRebind || scenario.mismatchedRebindACK {
		leaseTime = 8 * time.Second
	}
	var unavailable, initialAckSent, initialDiscoverSeen, initialRenewalSeen atomic.Bool
	var ownerMutationConfirmed atomic.Bool
	ownerRenewals := make(chan *dhcpv4.DHCPv4, 1)
	renewing := make(chan struct{})
	rejected := make(chan struct{})
	discovered := make(chan struct{})
	ackSent := make(chan struct{})
	var ackSentAt time.Time
	var nakReplies, requestsAfterAcquisition, replyErrors, renewalAcks atomic.Uint32
	t.Cleanup(func() {
		t.Logf("synthetic peer renewal ACKs: %d", renewalAcks.Load())
		t.Logf("synthetic peer NAK replies: %d", nakReplies.Load())
		t.Logf("[DEBUG-kv-peer-01] post-acquisition requests=%d reply-errors=%d", requestsAfterAcquisition.Load(), replyErrors.Load())
	})
	peerErrors := make(chan error, 1)
	initialACKWire := make(chan *dhcpv4.DHCPv4, 1)
	mismatchACKWire := make(chan *dhcpv4.DHCPv4, 1)
	mismatchRequest := make(chan *dhcpv4.DHCPv4, 1)
	mismatchReplySent := make(chan struct{})
	rebindACKWire := make(chan *dhcpv4.DHCPv4, 1)
	rebindReplySent := make(chan struct{})
	releaseWire := make(chan *dhcpv4.DHCPv4, 1)
	var mismatchReplies atomic.Uint32
	if scenario.mismatchedRenewalACK {
		// ponytail: local unicast ACKs use lo; observe both existing delivery paths.
		for _, wireInterface := range []string{clientLink.Attrs().Name, "lo"} {
			raw, err := nclient4.NewRawUDPConn(wireInterface, 68)
			must(err)
			var closing atomic.Bool
			done := make(chan struct{})
			t.Cleanup(func() {
				closing.Store(true)
				_ = raw.Close()
				select {
				case <-done:
				case <-time.After(2 * time.Second):
					t.Error("synthetic client-wire ACK observer did not stop")
				}
			})
			go func() {
				defer close(done)
				buffer := make([]byte, 4096)
				for {
					n, _, readErr := raw.ReadFrom(buffer)
					if readErr != nil {
						if !closing.Load() {
							select {
							case peerErrors <- fmt.Errorf("synthetic client-wire ACK read failed: %w", readErr):
							default:
							}
						}
						return
					}
					packet, parseErr := dhcpv4.FromBytes(append([]byte(nil), buffer[:n]...))
					if parseErr != nil {
						select {
						case peerErrors <- fmt.Errorf("synthetic client-wire ACK parse failed: %w", parseErr):
						default:
						}
						return
					}
					if packet.MessageType() != dhcpv4.MessageTypeAck || packet.ClientHWAddr.String() != clientLink.Attrs().HardwareAddr.String() {
						continue
					}
					if packet.YourIPAddr.Equal(net.ParseIP("192.0.2.20")) {
						select {
						case initialACKWire <- packet:
						default:
						}
					}
					if packet.YourIPAddr.Equal(net.ParseIP("192.0.2.21")) {
						if scenario.mismatchedRebindACK && packet.ClientIPAddr.IsUnspecified() {
							select {
							case rebindACKWire <- packet:
							default:
							}
							continue
						}
						select {
						case mismatchACKWire <- packet:
						default:
						}
					}
				}
			}()
		}
	}
	rawInitial := make(chan struct{})
	rawRebinds := make(chan *dhcpv4.DHCPv4, 1)
	var rawInitialRequests, rawRebindRequests atomic.Uint32
	if scenario.changeOwnerBeforeRebind || scenario.mismatchedRenewalACK {
		// ponytail: observe the external wire; UDP silence alone is not a frame check.
		raw, err := nclient4.NewRawUDPConn(serverLink.Attrs().Name, 67)
		must(err)
		var rawClosing atomic.Bool
		rawStopped := make(chan struct{})
		t.Cleanup(func() {
			rawClosing.Store(true)
			_ = raw.Close()
			select {
			case <-rawStopped:
			case <-time.After(2 * time.Second):
				t.Error("synthetic rebind raw peer did not stop")
			}
			if scenario.changeOwnerBeforeRebind {
				t.Logf("synthetic raw peer initial client requests=%d changed-owner T2 requests=%d", rawInitialRequests.Load(), rawRebindRequests.Load())
			} else {
				t.Logf("synthetic mismatched-ACK raw peer client requests=%d unexpected ACK replies=%d", rawInitialRequests.Load(), mismatchReplies.Load())
			}
		})
		go func() {
			defer close(rawStopped)
			buffer := make([]byte, 4096)
			for {
				n, _, readErr := raw.ReadFrom(buffer)
				if readErr != nil {
					if !rawClosing.Load() {
						select {
						case peerErrors <- fmt.Errorf("synthetic rebind raw receive failed: %w", readErr):
						default:
						}
					}
					return
				}
				packet, parseErr := dhcpv4.FromBytes(append([]byte(nil), buffer[:n]...))
				if parseErr != nil {
					select {
					case peerErrors <- fmt.Errorf("synthetic rebind raw parse failed: %w", parseErr):
					default:
					}
					return
				}
				if scenario.mismatchedRenewalACK && packet.MessageType() == dhcpv4.MessageTypeRelease &&
					packet.ClientHWAddr.String() == clientLink.Attrs().HardwareAddr.String() {
					select {
					case releaseWire <- packet:
					default:
					}
					continue
				}
				if packet.ClientHWAddr.String() != clientLink.Attrs().HardwareAddr.String() ||
					(packet.MessageType() != dhcpv4.MessageTypeDiscover && packet.MessageType() != dhcpv4.MessageTypeRequest) {
					continue
				}
				if !ownerMutationConfirmed.Load() {
					if rawInitialRequests.Add(1) == 2 {
						close(rawInitial)
					}
					continue
				}
				if !packet.ClientIPAddr.IsUnspecified() || !packet.RequestedIPAddress().Equal(net.ParseIP("192.0.2.20")) ||
					time.Now().Before(ackSentAt.Add(7*leaseTime/8)) || !time.Now().Before(ackSentAt.Add(leaseTime)) {
					select {
					case peerErrors <- errors.New("changed-owner raw packet lacks original requested lease or valid T2 pre-expiry control"):
					default:
					}
					return
				}
				rawRebindRequests.Add(1)
				select {
				case rawRebinds <- packet:
				default:
				}
			}
		}()
	}
	server, err := server4.NewServer("", &net.UDPAddr{Port: 67},
		func(conn net.PacketConn, peer net.Addr, request *dhcpv4.DHCPv4) {
			if request.MessageType() == dhcpv4.MessageTypeDiscover && initialDiscoverSeen.CompareAndSwap(false, true) {
				close(discovered)
			}
			if stopDuringAcquisition {
				return
			}
			if request.MessageType() == dhcpv4.MessageTypeRequest && !request.ClientIPAddr.IsUnspecified() && initialRenewalSeen.CompareAndSwap(false, true) {
				close(renewing)
			}
			if scenario.changeOwnerBeforeRebind && ownerMutationConfirmed.Load() &&
				(request.MessageType() == dhcpv4.MessageTypeDiscover || request.MessageType() == dhcpv4.MessageTypeRequest) {
				requestsAfterAcquisition.Add(1)
				return
			}
			if scenario.changeOwnerBeforeRenewal && request.MessageType() == dhcpv4.MessageTypeRequest && !request.ClientIPAddr.IsUnspecified() {
				peerUDP, validSource := peer.(*net.UDPAddr)
				if !ownerMutationConfirmed.Load() || !validSource || peerUDP == nil || !peerUDP.IP.Equal(request.ClientIPAddr) {
					select {
					case peerErrors <- errors.New("synthetic renewal peer lacks confirmed changed owner or matching wire source"):
					default:
					}
					return
				}
				requestsAfterAcquisition.Add(1)
				select {
				case ownerRenewals <- request:
				default:
				}
				return
			}
			if unavailable.Load() && request.MessageType() == dhcpv4.MessageTypeRequest {
				requestsAfterAcquisition.Add(1)
			}
			if unavailable.Load() && !nakRequests {
				return
			}
			var replyType dhcpv4.MessageType
			switch request.MessageType() {
			case dhcpv4.MessageTypeDiscover:
				replyType = dhcpv4.MessageTypeOffer
			case dhcpv4.MessageTypeRequest:
				replyType = dhcpv4.MessageTypeAck
				if unavailable.Load() || scenario.stopAfterRejection {
					replyType = dhcpv4.MessageTypeNak
				}
			default:
				return
			}
			modifiers := []dhcpv4.Modifier{
				dhcpv4.WithMessageType(replyType),
				dhcpv4.WithServerIP(net.ParseIP("192.0.2.1")),
				dhcpv4.WithOption(dhcpv4.OptServerIdentifier(net.ParseIP("192.0.2.1"))),
			}
			if replyType != dhcpv4.MessageTypeNak {
				modifiers = append(modifiers,
					dhcpv4.WithYourIP(net.ParseIP("192.0.2.20")),
					dhcpv4.WithOption(dhcpv4.OptIPAddressLeaseTime(leaseTime)))
			}
			if scenario.mismatchedRenewalACK && replyType == dhcpv4.MessageTypeAck &&
				(!request.ClientIPAddr.IsUnspecified() || (scenario.mismatchedRebindACK && initialAckSent.Load())) {
				// Deliberately unexpected extension ACK, not normal DHCP renumbering.
				modifiers = append(modifiers, dhcpv4.WithYourIP(net.ParseIP("192.0.2.21")),
					dhcpv4.WithOption(dhcpv4.OptIPAddressLeaseTime(2*leaseTime)))
				if scenario.mismatchedRebindACK {
					modifiers = append(modifiers, dhcpv4.WithClientIP(request.ClientIPAddr))
				}
				select {
				case mismatchRequest <- request:
				default:
				}
			}
			reply, replyErr := dhcpv4.NewReplyFromRequest(request, modifiers...)
			if replyErr == nil {
				control := &ipv4.ControlMessage{Src: serverAddr.IP}
				if request.ClientIPAddr.IsUnspecified() {
					control.IfIndex = serverLink.Attrs().Index
				}
				_, replyErr = ipv4.NewPacketConn(conn).WriteTo(reply.ToBytes(), control, peer)
				if replyErr == nil && replyType == dhcpv4.MessageTypeNak {
					if nakReplies.Add(1) == 1 {
						close(rejected)
					}
				}
				if replyErr == nil && replyType == dhcpv4.MessageTypeAck && !request.ClientIPAddr.IsUnspecified() && !scenario.mismatchedRenewalACK {
					renewalAcks.Add(1)
				}
				if replyErr == nil && replyType == dhcpv4.MessageTypeAck && scenario.mismatchedRenewalACK &&
					(!request.ClientIPAddr.IsUnspecified() || (scenario.mismatchedRebindACK && initialAckSent.Load())) {
					count := mismatchReplies.Add(1)
					if count == 1 {
						close(mismatchReplySent)
					}
					if !scenario.mismatchedRebindACK || count == 2 {
						unavailable.Store(true)
					}
					if scenario.mismatchedRebindACK && count == 2 {
						close(rebindReplySent)
					}
				}
				if replyErr == nil && replyType == dhcpv4.MessageTypeAck && initialAckSent.CompareAndSwap(false, true) {
					ackSentAt = time.Now()
					close(ackSent)
				}
			}
			if replyErr != nil {
				replyErrors.Add(1)
				select {
				case peerErrors <- replyErr:
				default:
				}
			}
		})
	must(err)
	t.Cleanup(func() { _ = server.Close() })
	go func() {
		if err := server.Serve(); err != nil {
			select {
			case peerErrors <- err:
			default:
			}
		}
	}()

	iface, err := net.InterfaceByName("kv-dhcp-c")
	must(err)
	backoffAttempts := uint(1)
	if scenario.stopAfterRejection {
		backoffAttempts = 0
	}
	client := NewDHCPv4Client(iface, false, "", backoffAttempts, false)
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	var result error
	go func() {
		result = client.Start(ctx)
		close(stopped)
	}()
	t.Cleanup(func() {
		cancel()
		client.Stop()
		// Only teardown drains errors; expiry must not depend on an external receiver.
		go func() {
			select {
			case <-client.ErrorChannel():
			case <-stopped:
			}
		}()
		select {
		case <-stopped:
		case <-time.After(2 * time.Second):
			t.Error("real DHCP client did not stop after teardown cancellation")
		}
	})

	if stopDuringAcquisition {
		select {
		case <-discovered:
			t.Log("synthetic peer witnessed real DISCOVER; calling Stop during acquisition")
		case err := <-peerErrors:
			t.Fatalf("synthetic peer failed before acquisition Stop: %v", err)
		case <-stopped:
			t.Fatalf("client returned before acquisition was observed: %v", result)
		case <-time.After(15 * time.Second):
			t.Fatal("synthetic peer saw no DISCOVER; active-acquisition Stop boundary was not exercised")
		}
		client.Stop()
		select {
		case <-stopped:
			t.Logf("real in-flight acquisition stopped without parent cancellation: %v", result)
		case err := <-peerErrors:
			t.Fatalf("synthetic peer failed during acquisition Stop: %v", err)
		case <-time.After(time.Second):
			t.Fatal("Stop did not cancel real in-flight DHCP acquisition")
		}
		return
	}

	if scenario.stopAfterRejection {
		select {
		case <-rejected:
			t.Log("synthetic peer sent real NAK; calling Stop with unlimited retries")
		case err := <-peerErrors:
			t.Fatalf("synthetic peer failed before rejected-acquisition Stop: %v", err)
		case <-stopped:
			t.Fatalf("client returned before rejection was observed: %v", result)
		case <-time.After(15 * time.Second):
			t.Fatal("synthetic peer sent no NAK; rejected-retry Stop boundary was not exercised")
		}
		client.Stop()
		select {
		case <-stopped:
			t.Logf("real unlimited-retry acquisition stopped without parent cancellation: %v", result)
		case err := <-peerErrors:
			t.Fatalf("synthetic peer failed during rejected-acquisition Stop: %v", err)
		case <-time.After(time.Second):
			t.Fatal("Stop did not cancel real rejected acquisition with unlimited retries")
		}
		return
	}

	expireBefore := time.Time{}
	if ipReceiveDelay > 0 {
		select {
		case <-ackSent:
			expireBefore = ackSentAt.Add(leaseTime + time.Second)
		case err := <-peerErrors:
			t.Fatalf("synthetic DHCP peer failed before delayed delivery: %v", err)
		case <-time.After(15 * time.Second):
			t.Fatal("synthetic peer sent no ACK; delayed-receiver boundary was not exercised")
		}
		select {
		case <-time.After(ipReceiveDelay):
		case <-stopped:
			t.Fatalf("client stopped before delayed IP delivery: %v", result)
		case err := <-peerErrors:
			t.Fatalf("synthetic DHCP peer failed during receiver delay: %v", err)
		}
		t.Log("synthetic ACK witnessed; IP receiver delayed within lease lifetime")
	}
	if blockIPDelivery {
		select {
		case <-ackSent:
			t.Log("synthetic ACK sent; IPChannel deliberately has no receiver")
		case err := <-peerErrors:
			t.Fatalf("synthetic DHCP peer failed before ACK: %v", err)
		case <-time.After(15 * time.Second):
			t.Fatal("synthetic peer sent no ACK; blocked-delivery boundary was not exercised")
		}
	} else {
		select {
		case ip := <-client.IPChannel():
			if ip != "192.0.2.20" {
				t.Fatalf("unexpected synthetic lease address %q", ip)
			}
		case err := <-peerErrors:
			t.Fatalf("synthetic DHCP peer failed before acquisition: %v", err)
		case <-time.After(15 * time.Second):
			t.Fatal("real DHCP client did not acquire the synthetic lease")
		}
		address, err := netlink.ParseAddr("192.0.2.20/24")
		must(err)
		must(netlink.AddrAdd(clientLink, address))
		if scenario.mismatchedRenewalACK {
			select {
			case <-ackSent:
			case err := <-peerErrors:
				t.Fatalf("synthetic peer failed before mismatched-ACK timing witness: %v", err)
			case <-time.After(time.Second):
				t.Fatal("original ACK timing witness missing for mismatched-extension control")
			}
			select {
			case packet := <-initialACKWire:
				if !packet.ServerIdentifier().Equal(serverAddr.IP) || packet.IPAddressLeaseTime(0) != leaseTime {
					t.Fatal("initial raw ACK lacks original server and lifetime control")
				}
			case err := <-peerErrors:
				t.Fatalf("synthetic peer failed before original client-wire ACK: %v", err)
			case <-time.After(time.Second):
				t.Fatal("client-wire observer saw no original ACK; receive control unproved")
			}
			select {
			case <-rawInitial:
			case err := <-peerErrors:
				t.Fatalf("synthetic peer failed before initial raw client requests: %v", err)
			case <-time.After(time.Second):
				t.Fatal("raw peer saw no original DISCOVER and REQUEST for mismatched-ACK control")
			}
			if time.Since(ackSentAt) >= leaseTime/4 || ctx.Err() != nil {
				t.Fatal("mismatched-ACK control missed live original lease/pre-T1 margin")
			}
			fingerprint := func() string {
				link, err := netlink.LinkByIndex(iface.Index)
				must(err)
				addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
				must(err)
				a := link.Attrs()
				return fmt.Sprintf("%T|%d|%d|%s|%s|%s|%d|%s|%v", link, a.Index, a.ParentIndex, a.Name, a.HardwareAddr, a.Alias, a.MTU, a.Flags, addrs)
			}
			original := fingerprint()
			var packet, request *dhcpv4.DHCPv4
			select {
			case packet = <-mismatchACKWire:
			case err := <-peerErrors:
				t.Fatalf("synthetic peer failed before unexpected client-wire ACK: %v", err)
			case <-stopped:
				t.Fatalf("real client ended before mismatched-ACK wire control: %v", result)
			case <-time.After(3 * leaseTime / 4):
				t.Fatal("unexpected ACK did not reach actual client wire; sender success is insufficient")
			}
			select {
			case <-mismatchReplySent:
			case err := <-peerErrors:
				t.Fatalf("synthetic peer failed while confirming unexpected ACK send: %v", err)
			case <-time.After(time.Second):
				t.Fatal("unexpected ACK send completion witness missing")
			}
			select {
			case request = <-mismatchRequest:
			default:
				t.Fatal("unexpected wire ACK lacks actual renewal REQUEST witness")
			}
			if packet.TransactionID != request.TransactionID || !request.ClientIPAddr.Equal(address.IP) ||
				packet.ClientHWAddr.String() != iface.HardwareAddr.String() || !packet.ServerIdentifier().Equal(serverAddr.IP) ||
				!packet.YourIPAddr.Equal(net.ParseIP("192.0.2.21")) || packet.IPAddressLeaseTime(0) != 2*leaseTime ||
				mismatchReplies.Load() != 1 || time.Since(ackSentAt) >= 3*leaseTime/4 || fingerprint() != original || ctx.Err() != nil {
				t.Fatal("unexpected ACK wire identity/transaction/server/lifetime/timing/native/caller control invalid")
			}
			t.Log("original real lease/address and raw DORA/ACK verified; actual T1 request received unexpected different-address ACK on client wire with matching transaction/MAC/server; no valid renewal ACK claimed")
			if scenario.mismatchedRebindACK {
				select {
				case packet = <-rebindACKWire:
				case err := <-peerErrors:
					t.Fatalf("synthetic peer failed before unexpected rebind client-wire ACK: %v", err)
				case <-stopped:
					t.Fatalf("real client ended before rebind wire control: %v", result)
				case <-time.After(time.Until(ackSentAt.Add(leaseTime))):
					t.Fatal("unexpected rebind ACK did not reach client wire before original expiry")
				}
				select {
				case <-rebindReplySent:
				case err := <-peerErrors:
					t.Fatalf("synthetic peer failed confirming rebind ACK send: %v", err)
				case <-time.After(time.Second):
					t.Fatal("unexpected rebind ACK send completion witness missing")
				}
				select {
				case request = <-mismatchRequest:
				default:
					t.Fatal("unexpected rebind wire ACK lacks actual second REQUEST witness")
				}
				if packet.TransactionID != request.TransactionID || !request.ClientIPAddr.IsUnspecified() ||
					!request.RequestedIPAddress().Equal(address.IP) || !packet.ClientIPAddr.IsUnspecified() ||
					packet.ClientHWAddr.String() != iface.HardwareAddr.String() || !packet.ServerIdentifier().Equal(serverAddr.IP) ||
					!packet.YourIPAddr.Equal(net.ParseIP("192.0.2.21")) || packet.IPAddressLeaseTime(0) != 2*leaseTime ||
					mismatchReplies.Load() != 2 || time.Now().Before(ackSentAt.Add(7*leaseTime/8)) ||
					!time.Now().Before(ackSentAt.Add(leaseTime)) || fingerprint() != original || ctx.Err() != nil {
					t.Fatal("unexpected rebind ACK lacks second transaction/old requested-IP/native/T2 pre-expiry caller controls")
				}
				t.Log("T1 unexpected ACK rejection control reached real T2 legacy DORA; peer offered and client requested original IP but second unexpected ACK arrived with another IP and matching transaction/MAC/server; caller live")
			}
			expired := false
			select {
			case <-stopped:
				expired = true
				if time.Now().Before(ackSentAt.Add(leaseTime-250*time.Millisecond)) || !errors.Is(result, context.DeadlineExceeded) {
					t.Fatalf("rejected extension did not preserve original expiry/error: %v", result)
				}
			case err := <-peerErrors:
				t.Fatalf("synthetic peer failed while awaiting original expiry: %v", err)
			case <-time.After(time.Until(ackSentAt.Add(leaseTime + time.Second))):
				client.Stop()
				select {
				case <-stopped:
				case err := <-peerErrors:
					t.Fatalf("synthetic peer failed during actual Stop after old expiry: %v", err)
				case <-time.After(time.Second):
					t.Fatal("real client did not stop after original-expiry overrun")
				}
			}
			var released *dhcpv4.DHCPv4
			select {
			case released = <-releaseWire:
			case err := <-peerErrors:
				t.Fatalf("synthetic peer failed while receiving real RELEASE: %v", err)
			case <-time.After(time.Second):
				t.Fatal("raw peer saw no RELEASE; accepted-lease identity is unproved")
			}
			if released.ClientHWAddr.String() != iface.HardwareAddr.String() || !released.ServerIdentifier().Equal(serverAddr.IP) ||
				fingerprint() != original || ctx.Err() != nil {
				t.Fatal("real RELEASE lacks unchanged native/MAC/server/caller control")
			}
			if !expired {
				if !released.ClientIPAddr.Equal(packet.YourIPAddr) {
					t.Fatal("original expiry overrun lacks RELEASE proof of unexpected ACK consumption")
				}
				if scenario.mismatchedRebindACK {
					t.Fatal("unexpected different-address rebind ACK extended the real client past original expiry and became its RELEASE address; second client-wire ACK and caller live")
				}
				t.Fatal("unexpected different-address renewal ACK extended the real client past original expiry and became its RELEASE address; client-wire ACK and caller live")
			}
			if !released.ClientIPAddr.Equal(address.IP) {
				t.Fatal("rejected mismatched renewal ACK polluted original lease RELEASE identity")
			}
			if scenario.mismatchedRebindACK {
				t.Log("unexpected different-address rebind ACK did not extend original expiry; real expiry/old-IP RELEASE/exact native attachment/caller preserved")
				return
			}
			t.Log("unexpected different-address renewal ACK did not extend original expiry; real expiry/old-IP RELEASE/exact native attachment/caller preserved")
			return
		}
		if scenario.changeOwnerBeforeRebind {
			select {
			case <-ackSent:
			case err := <-peerErrors:
				t.Fatalf("synthetic peer failed before rebind-owner ACK witness: %v", err)
			case <-stopped:
				t.Fatalf("real client ended before rebind-owner ACK witness: %v", result)
			case <-time.After(time.Second):
				t.Fatal("synthetic ACK timing witness missing for rebind-owner control")
			}
			select {
			case <-rawInitial:
			case err := <-peerErrors:
				t.Fatalf("synthetic raw peer failed before rebind-owner mutation: %v", err)
			case <-stopped:
				t.Fatalf("real client ended before initial raw receive control: %v", result)
			case <-time.After(time.Second):
				t.Fatal("raw peer observed no initial real DISCOVER and REQUEST; T2 receive boundary unproved")
			}
			if rawInitialRequests.Load() != 2 || time.Since(ackSentAt) >= leaseTime/4 || initialRenewalSeen.Load() || ctx.Err() != nil {
				t.Fatal("rebind-owner control lacks two raw initial client requests and live pre-T1 lease margin")
			}
			fingerprint := func(link netlink.Link) string {
				addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
				must(err)
				a := link.Attrs()
				return fmt.Sprintf("%T|%d|%d|%s|%s|%d|%s|%v", link, a.Index, a.ParentIndex, a.Name, a.HardwareAddr, a.MTU, a.Flags, addrs)
			}
			original, err := netlink.LinkByIndex(iface.Index)
			must(err)
			beforeOwnerChange := fingerprint(original)
			const foreignAlias = "external-owner/t2-rebind"
			if _, ok := original.(*netlink.Veth); !ok || original.Attrs().Alias == foreignAlias {
				t.Fatal("rebind-owner control lacks original distinct-owner indexed veth")
			}
			unavailable.Store(true)
			must(netlink.LinkSetAlias(original, foreignAlias))
			changed, err := netlink.LinkByIndex(iface.Index)
			must(err)
			if changed.Attrs().Alias != foreignAlias || fingerprint(changed) != beforeOwnerChange ||
				time.Since(ackSentAt) >= leaseTime/4 || initialRenewalSeen.Load() || ctx.Err() != nil {
				t.Fatal("rebind-owner mutation changed more than native alias or missed pre-T1 live-lease control")
			}
			ownerMutationConfirmed.Store(true)
			t.Log("raw peer witnessed initial real DISCOVER and REQUEST plus ACK/address; only native alias changed on same indexed attachment before T1; caller live")
			select {
			case packet := <-rawRebinds:
				live, err := netlink.LinkByIndex(iface.Index)
				must(err)
				if packet.ClientHWAddr.String() != iface.HardwareAddr.String() || !packet.RequestedIPAddress().Equal(address.IP) ||
					!packet.ClientIPAddr.IsUnspecified() || live.Attrs().Alias != foreignAlias || fingerprint(live) != beforeOwnerChange ||
					ctx.Err() != nil || time.Now().Before(ackSentAt.Add(7*leaseTime/8)) || !time.Now().Before(ackSentAt.Add(leaseTime)) {
					t.Fatal("observed raw rebind packet lacks old lease/MAC/changed-owner attachment or live T2 pre-expiry control")
				}
				t.Logf("raw T2 request type=%s requested-IP=%s ciaddr=%s MAC=%s after-ACK=%s; no reply or renewal ACK", packet.MessageType(), packet.RequestedIPAddress(), packet.ClientIPAddr, packet.ClientHWAddr, time.Since(ackSentAt))
				t.Fatal("real DHCP client sent old-lease T2 rebind DISCOVER/REQUEST through same-index changed native owner; raw wire and caller live")
			case err := <-peerErrors:
				t.Fatalf("synthetic peer failed during rebind-owner raw observation: %v", err)
			case <-stopped:
				t.Fatalf("real client ended before valid T2 rebind-owner raw observation completed: %v", result)
			case <-time.After(time.Until(ackSentAt.Add(15 * leaseTime / 16))):
			}
			if time.Now().Before(ackSentAt.Add(7*leaseTime/8)) || !time.Now().Before(ackSentAt.Add(31*leaseTime/32)) || ctx.Err() != nil {
				t.Fatal("rebind-owner observation missed bounded T2 pre-expiry window or cancelled caller")
			}
			client.Stop()
			select {
			case <-stopped:
				t.Logf("real client Stop completed after changed-owner T2 raw window; caller live: %v", result)
			case err := <-peerErrors:
				t.Fatalf("synthetic peer failed during rebind-owner Stop: %v", err)
			case <-time.After(time.Second):
				t.Fatal("real client did not stop after changed-owner T2 raw observation")
			}
			select {
			case <-rawRebinds:
				t.Fatal("raw peer observed late old-lease rebind after Stop with changed native owner")
			case err := <-peerErrors:
				t.Fatalf("synthetic peer failed during rebind-owner post-Stop raw drain: %v", err)
			case <-time.After(50 * time.Millisecond):
			}
			live, err := netlink.LinkByIndex(iface.Index)
			must(err)
			if live.Attrs().Alias != foreignAlias || fingerprint(live) != beforeOwnerChange || rawInitialRequests.Load() != 2 ||
				rawRebindRequests.Load() != 0 || requestsAfterAcquisition.Load() != 0 || renewalAcks.Load() != 0 || ctx.Err() != nil {
				t.Fatal("changed-owner T2 Stop emitted wire requests or changed native attachment/caller")
			}
			t.Log("raw peer observed no old-lease T2 rebind through changed native owner; initial raw receive control and exact attachment preserved; Stop completed with caller live")
			return
		}
		if scenario.changeOwnerBeforeRenewal {
			select {
			case <-ackSent:
			case err := <-peerErrors:
				t.Fatalf("synthetic peer failed before renewal-owner mutation: %v", err)
			case <-stopped:
				t.Fatalf("real client ended before renewal-owner mutation: %v", result)
			case <-time.After(time.Second):
				t.Fatal("synthetic ACK timing witness missing for renewal-owner control")
			}
			if time.Since(ackSentAt) >= leaseTime/4 || initialRenewalSeen.Load() || ctx.Err() != nil {
				t.Fatal("renewal-owner control lacks live original lease and pre-T1 preparation margin")
			}
			fingerprint := func(link netlink.Link) string {
				addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
				must(err)
				a := link.Attrs()
				return fmt.Sprintf("%T|%d|%d|%s|%s|%d|%s|%v", link, a.Index, a.ParentIndex, a.Name, a.HardwareAddr, a.MTU, a.Flags, addrs)
			}
			original, err := netlink.LinkByIndex(iface.Index)
			must(err)
			beforeOwnerChange := fingerprint(original)
			const foreignAlias = "external-owner/t1-renewal"
			if _, ok := original.(*netlink.Veth); !ok || original.Attrs().Alias == foreignAlias {
				t.Fatal("renewal-owner control lacks original distinct-owner indexed veth")
			}
			unavailable.Store(true)
			must(netlink.LinkSetAlias(original, foreignAlias))
			changed, err := netlink.LinkByIndex(iface.Index)
			must(err)
			if changed.Attrs().Alias != foreignAlias || fingerprint(changed) != beforeOwnerChange ||
				time.Since(ackSentAt) >= leaseTime/4 || initialRenewalSeen.Load() || ctx.Err() != nil {
				t.Fatal("renewal-owner mutation changed more than native alias or missed pre-T1 live-lease control")
			}
			ownerMutationConfirmed.Store(true)
			t.Log("real wire ACK/address installed; external actor changed only native owner on same veth index/name/type/parent/MAC/MTU/flags/addresses before T1; caller live")
			// ponytail: cover T1 before T2; rebind ownership needs its own wire proof.
			observeUntil := ackSentAt.Add(3 * leaseTime / 4)
			select {
			case packet := <-ownerRenewals:
				if !packet.ClientIPAddr.Equal(address.IP) || packet.ClientHWAddr.String() != iface.HardwareAddr.String() ||
					ctx.Err() != nil || !time.Now().Before(ackSentAt.Add(7*leaseTime/8)) {
					t.Fatal("observed renewal REQUEST lacks original wire lease/MAC or valid pre-T2 caller control")
				}
				t.Fatal("real DHCP client sent old-lease renewal REQUEST through same-index changed native owner; caller live")
			case err := <-peerErrors:
				t.Fatalf("synthetic peer failed during renewal-owner observation: %v", err)
			case <-stopped:
				t.Fatalf("real client ended before valid T1 renewal-owner observation completed: %v", result)
			case <-time.After(time.Until(observeUntil)):
			}
			if !time.Now().Before(ackSentAt.Add(7*leaseTime/8)) || ctx.Err() != nil {
				t.Fatal("renewal-owner observation crossed T2 or cancelled caller")
			}
			client.Stop()
			select {
			case <-stopped:
				t.Logf("real client Stop completed after owner-change T1 window; caller live: %v", result)
			case err := <-peerErrors:
				t.Fatalf("synthetic peer failed during renewal-owner Stop: %v", err)
			case <-time.After(time.Second):
				t.Fatal("real client did not stop after changed-owner T1 observation")
			}
			live, err := netlink.LinkByIndex(iface.Index)
			must(err)
			if live.Attrs().Alias != foreignAlias || fingerprint(live) != beforeOwnerChange ||
				requestsAfterAcquisition.Load() != 0 || renewalAcks.Load() != 0 || ctx.Err() != nil {
				t.Fatal("changed-owner T1 Stop emitted renewal traffic or changed native attachment/caller")
			}
			t.Log("real DHCP client sent no old-lease T1 renewal through changed native owner; exact attachment preserved and Stop completed with caller live")
			return
		}
		if ipReceiveDelay > 0 {
			t.Log("delayed IP receiver consumed the current lease address")
		}
		if renewBeforeLoss {
			select {
			case err := <-peerErrors:
				t.Fatalf("synthetic DHCP peer failed during renewal: %v", err)
			case <-stopped:
				t.Fatalf("renewed lease stopped at its obsolete expiry: %v", result)
			case <-time.After(leaseTime + time.Second):
			}
			if renewalAcks.Load() == 0 {
				t.Fatal("synthetic peer acknowledged no renewal; old-expiry boundary was not exercised")
			}
			t.Log("renewed lease survived its original expiry deadline")
		}
	}
	unavailable.Store(true)
	t.Logf("synthetic lease acquired; renewal peer unavailable; NAK mode: %t", nakRequests)

	if scenario.stopDuringRenewal {
		select {
		case <-renewing:
			t.Log("synthetic peer witnessed real renewal REQUEST; calling Stop")
		case err := <-peerErrors:
			t.Fatalf("synthetic peer failed before renewal Stop: %v", err)
		case <-stopped:
			t.Fatalf("client returned before renewal was observed: %v", result)
		case <-time.After(leaseTime):
			t.Fatal("synthetic peer saw no renewal REQUEST; Stop boundary was not exercised")
		}
		client.Stop()
		select {
		case <-stopped:
			t.Logf("real renewal stopped without parent cancellation: %v", result)
		case err := <-peerErrors:
			t.Fatalf("synthetic peer failed during renewal Stop: %v", err)
		case <-time.After(time.Second):
			t.Fatal("Stop did not cancel real in-flight DHCP renewal")
		}
		return
	}

	expiryTimeout := leaseTime + time.Second
	if !expireBefore.IsZero() {
		expiryTimeout = time.Until(expireBefore)
	}
	select {
	case err := <-peerErrors:
		t.Fatalf("synthetic DHCP peer failed after acquisition: %v", err)
	case <-stopped:
		if result == nil {
			t.Fatal("expired active lease must report a terminal error")
		}
		if (blockIPDelivery || ipReceiveDelay > 0) && !errors.Is(result, context.DeadlineExceeded) {
			t.Fatalf("blocked lease delivery returned a non-expiry error: %v", result)
		}
	case <-time.After(expiryTimeout):
		if ipReceiveDelay > 0 {
			t.Fatal("delayed IP delivery restarted the lifetime of the original ACK lease")
		}
		t.Fatal("active lease expiry did not terminate the real DHCP client without an error receiver")
	}
	if nakRequests && nakReplies.Load() == 0 {
		t.Fatal("synthetic peer sent no NAK; retry/error-delivery boundary was not exercised")
	}
}
