package services

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/nclient4"
	"github.com/insomniacslk/dhcp/dhcpv4/server4"
	"github.com/insomniacslk/dhcp/dhcpv6"
	"github.com/insomniacslk/dhcp/dhcpv6/server6"
	"github.com/insomniacslk/dhcp/iana"
	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/networkinterface"
	"github.com/kube-vip/kube-vip/pkg/node/noop"
	"github.com/mdlayher/packet"
	"github.com/vishvananda/netlink"
	"golang.org/x/net/ipv4"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type dhcpv4ServiceScenario uint8

const (
	serviceLeaseExpiry dhcpv4ServiceScenario = iota
	serviceDeleteWhileAPIStalled
	serviceWatchCurrentLease
	serviceReacquireChangedAddress
	serviceReacquireWhileAPIStalled
	serviceRejectUnownedInterface
	serviceOwnedInstanceRestart
	serviceOwnedInstanceRestartWithoutMAC
	serviceRejectOwnedInterfaceUIDCollision
	serviceCleanupReplacedInterface
	serviceReleaseOriginalInterface
	serviceReleaseReplacedInterface
	serviceReleaseChangedOwner
	serviceCleanupReleasesLease
	serviceWatchDeleteReleasesLease
	serviceWatchDeleteDualStackReleasesLeases
	serviceWatchDeleteIPv6OnlyReleasesLease
)

func TestDHCPv4ServiceWithdrawsExpiredVIPWhileAPIStalled(t *testing.T) {
	checkDHCPv4ServiceLease(t, serviceLeaseExpiry)
}

func TestDHCPv4ServiceDeleteCancelsStalledAPIAndStopsWorkers(t *testing.T) {
	checkDHCPv4ServiceLease(t, serviceDeleteWhileAPIStalled)
}

func TestDHCPv4ServiceWatchRetainsCurrentLeaseAfterStatusFeedback(t *testing.T) {
	checkDHCPv4ServiceLease(t, serviceWatchCurrentLease)
}

func TestDHCPv4ServiceReacquiredAddressReconcilesThroughWatch(t *testing.T) {
	checkDHCPv4ServiceLease(t, serviceReacquireChangedAddress)
}

func TestDHCPv4ServiceWithdrawsReplacedVIPWhileAPIStalled(t *testing.T) {
	checkDHCPv4ServiceLease(t, serviceReacquireWhileAPIStalled)
}

func TestDHCPv4InstanceRejectsUnownedNamedInterfaceBeforeDHCP(t *testing.T) {
	checkDHCPv4ServiceLease(t, serviceRejectUnownedInterface)
}

func TestDHCPv4InstanceRestartsOwnedInterfaceWithSameService(t *testing.T) {
	checkDHCPv4ServiceLease(t, serviceOwnedInstanceRestart)
}

func TestDHCPv4InstanceRestartsOwnedInterfaceWithoutMACAnnotation(t *testing.T) {
	checkDHCPv4ServiceLease(t, serviceOwnedInstanceRestartWithoutMAC)
}

func TestDHCPv4InstanceRejectsOwnedInterfaceWithSameUIDPrefix(t *testing.T) {
	checkDHCPv4ServiceLease(t, serviceRejectOwnedInterfaceUIDCollision)
}

func TestDHCPv4InstanceCleanupPreservesReplacedInterface(t *testing.T) {
	checkDHCPv4ServiceLease(t, serviceCleanupReplacedInterface)
}

func TestDHCPv4InstanceStopReleasesLeaseOnOriginalInterface(t *testing.T) {
	checkDHCPv4ServiceLease(t, serviceReleaseOriginalInterface)
}

func TestDHCPv4InstanceStopDoesNotReleaseThroughReplacedInterface(t *testing.T) {
	checkDHCPv4ServiceLease(t, serviceReleaseReplacedInterface)
}

func TestDHCPv4InstanceStopDoesNotReleaseThroughChangedInterfaceOwner(t *testing.T) {
	checkDHCPv4ServiceLease(t, serviceReleaseChangedOwner)
}

func TestDHCPv4InstanceCleanupReleasesLeaseBeforeDeletingInterface(t *testing.T) {
	checkDHCPv4ServiceLease(t, serviceCleanupReleasesLease)
}

func TestDHCPv4ServiceWatchDeleteReleasesLeaseBeforeRemovingInterface(t *testing.T) {
	checkDHCPv4ServiceLease(t, serviceWatchDeleteReleasesLease)
}

func TestDHCPDualStackServiceWatchDeleteStopsClientsAndReleasesLeases(t *testing.T) {
	checkDHCPv4ServiceLease(t, serviceWatchDeleteDualStackReleasesLeases)
}

func TestDHCPv6ServiceWatchDeleteStopsClientAndReleasesLease(t *testing.T) {
	checkDHCPv4ServiceLease(t, serviceWatchDeleteIPv6OnlyReleasesLease)
}

func checkDHCPv4ServiceLease(t *testing.T, scenario dhcpv4ServiceScenario) {
	t.Helper()
	ownedRestart := scenario == serviceOwnedInstanceRestart || scenario == serviceOwnedInstanceRestartWithoutMAC || scenario == serviceRejectOwnedInterfaceUIDCollision || scenario == serviceCleanupReplacedInterface || scenario == serviceReleaseOriginalInterface || scenario == serviceReleaseReplacedInterface || scenario == serviceReleaseChangedOwner || scenario == serviceCleanupReleasesLease
	deleteBeforeExpiry := scenario == serviceDeleteWhileAPIStalled
	stallAfterReplacement := scenario == serviceReacquireWhileAPIStalled
	changeAfterRejection := scenario == serviceReacquireChangedAddress || stallAfterReplacement
	dualStackWatch := scenario == serviceWatchDeleteDualStackReleasesLeases
	ipv6OnlyWatch := scenario == serviceWatchDeleteIPv6OnlyReleasesLease
	v6Watch := dualStackWatch || ipv6OnlyWatch
	watchDelete := scenario == serviceWatchDeleteReleasesLease || v6Watch
	watchStatusFeedback := scenario == serviceWatchCurrentLease || changeAfterRejection || watchDelete
	if os.Getenv("KUBE_VIP_ISOLATED_DHCP_TEST") != "1" {
		t.Skip("requires a dedicated network-none Linux test process")
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	links, err := netlink.LinkList()
	must(err)
	for _, link := range links {
		if link.Attrs().Name != "lo" {
			t.Fatal("Service DHCP integration test requires an initially loopback-only namespace")
		}
	}
	must(netlink.LinkAdd(&netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{Name: "kv-svc-c"}, PeerName: "kv-svc-s",
	}))
	parent, err := netlink.LinkByName("kv-svc-c")
	must(err)
	t.Cleanup(func() { _ = netlink.LinkDel(parent) })
	peerLink, err := netlink.LinkByName("kv-svc-s")
	must(err)
	must(netlink.LinkSetUp(parent))
	must(netlink.LinkSetUp(peerLink))
	peerAddress, err := netlink.ParseAddr("192.0.2.1/24")
	must(err)
	must(netlink.AddrAdd(peerLink, peerAddress))

	const leasedIP = "192.0.2.20"
	expectedIP := leasedIP
	if ipv6OnlyWatch {
		expectedIP = "2001:db8:1::30"
	}
	const changedIP = "192.0.2.21"
	leaseLifetime := 8 * time.Second
	if changeAfterRejection {
		leaseLifetime = 16 * time.Second
	}
	ackSent := make(chan time.Time, 1)
	changedAckSent := make(chan time.Time, 1)
	peerErrors := make(chan error, 1)
	releaseSeen := make(chan *dhcpv4.DHCPv4, 4)
	cleanupInitialRequests := make(chan *dhcpv4.DHCPv4, 2)
	rawReleaseObservation := scenario == serviceReleaseOriginalInterface || scenario == serviceReleaseReplacedInterface || scenario == serviceReleaseChangedOwner || scenario == serviceCleanupReleasesLease || watchDelete
	var acquired, replacementAcknowledged atomic.Bool
	var rejectedOldRequests, discoverRequests, ackReplies, releaseRequests, udpReleaseRequests, rawPeerPackets atomic.Int32
	reportPeerError := func(err error) {
		select {
		case peerErrors <- err:
		default:
		}
	}
	if rawReleaseObservation {
		// Observe only the external peer wire; leave the proven UDP DORA/reply path unchanged.
		rawPeer, err := nclient4.NewRawUDPConn(peerLink.Attrs().Name, 67)
		must(err)
		rawStopped, rawDone := make(chan struct{}), make(chan struct{})
		t.Cleanup(func() {
			close(rawStopped)
			_ = rawPeer.Close()
			select {
			case <-rawDone:
			case <-time.After(2 * time.Second):
				t.Error("synthetic raw release observer did not stop")
			}
			t.Logf("[DEBUG-kv-release-raw-01] peer DHCP packets=%d raw RELEASEs=%d UDP RELEASEs=%d", rawPeerPackets.Load(), releaseRequests.Load(), udpReleaseRequests.Load())
		})
		go func() {
			defer close(rawDone)
			buffer := make([]byte, 4096)
			for {
				n, _, err := rawPeer.ReadFrom(buffer)
				if err != nil {
					select {
					case <-rawStopped:
						return
					default:
						reportPeerError(fmt.Errorf("synthetic raw release receive: %w", err))
						return
					}
				}
				packet, err := dhcpv4.FromBytes(append([]byte(nil), buffer[:n]...))
				if err != nil {
					reportPeerError(fmt.Errorf("synthetic raw DHCP decode: %w", err))
					return
				}
				rawPeerPackets.Add(1)
				if (scenario == serviceCleanupReleasesLease || watchDelete) && (packet.MessageType() == dhcpv4.MessageTypeDiscover || packet.MessageType() == dhcpv4.MessageTypeRequest) {
					select {
					case cleanupInitialRequests <- packet:
					default:
					}
				}
				if packet.MessageType() != dhcpv4.MessageTypeRelease {
					continue
				}
				releaseRequests.Add(1)
				select {
				case releaseSeen <- packet:
				default:
				}
			}
		}()
	}
	dhcpPeer, err := server4.NewServer("", &net.UDPAddr{Port: 67},
		func(conn net.PacketConn, peer net.Addr, request *dhcpv4.DHCPv4) {
			if request.MessageType() == dhcpv4.MessageTypeRelease {
				udpReleaseRequests.Add(1)
				if rawReleaseObservation {
					return // Single wire source avoids counting one packet twice if UDP also accepts it.
				}
				releaseRequests.Add(1)
				select {
				case releaseSeen <- request:
				default:
				}
				return
			}
			if request.MessageType() == dhcpv4.MessageTypeDiscover {
				discoverRequests.Add(1)
			}
			if acquired.Load() && !changeAfterRejection && !ownedRestart {
				return // The other scenarios blackhole renewal/reacquisition traffic.
			}
			replyIP := net.ParseIP(leasedIP)
			var replyType dhcpv4.MessageType
			switch request.MessageType() {
			case dhcpv4.MessageTypeDiscover:
				replyType = dhcpv4.MessageTypeOffer
				if acquired.Load() && changeAfterRejection {
					replyIP = net.ParseIP(changedIP)
				}
			case dhcpv4.MessageTypeRequest:
				replyType = dhcpv4.MessageTypeAck
				if acquired.Load() && changeAfterRejection {
					requested := request.RequestedIPAddress()
					switch {
					case request.ClientIPAddr.Equal(net.ParseIP(leasedIP)) || requested.Equal(net.ParseIP(leasedIP)):
						replyType = dhcpv4.MessageTypeNak
						replyIP = net.IPv4zero
					case request.ClientIPAddr.Equal(net.ParseIP(changedIP)) || requested.Equal(net.ParseIP(changedIP)):
						replyIP = net.ParseIP(changedIP)
					default:
						reportPeerError(fmt.Errorf("unexpected post-acquisition DHCP request: ciaddr=%v requested=%v", request.ClientIPAddr, requested))
						return
					}
				}
			default:
				return
			}
			modifiers := []dhcpv4.Modifier{
				dhcpv4.WithMessageType(replyType),
				dhcpv4.WithServerIP(peerAddress.IP),
				dhcpv4.WithOption(dhcpv4.OptServerIdentifier(peerAddress.IP)),
				dhcpv4.WithYourIP(replyIP),
			}
			if replyType == dhcpv4.MessageTypeNak {
				modifiers = append(modifiers, dhcpv4.WithClientIP(net.IPv4zero))
			} else {
				modifiers = append(modifiers, dhcpv4.WithOption(dhcpv4.OptIPAddressLeaseTime(leaseLifetime)))
			}
			if replyType == dhcpv4.MessageTypeAck && acquired.Load() && replyIP.Equal(net.ParseIP(changedIP)) {
				// Set the external API outage before this replacement ACK can reach the real client.
				replacementAcknowledged.Store(true)
			}
			reply, err := dhcpv4.NewReplyFromRequest(request, modifiers...)
			if err == nil {
				control := &ipv4.ControlMessage{Src: peerAddress.IP}
				if request.ClientIPAddr.IsUnspecified() {
					control.IfIndex = peerLink.Attrs().Index
				}
				_, err = ipv4.NewPacketConn(conn).WriteTo(reply.ToBytes(), control, peer)
			}
			if err != nil {
				reportPeerError(err)
				return
			}
			if replyType == dhcpv4.MessageTypeNak {
				rejectedOldRequests.Add(1)
			}
			if replyType == dhcpv4.MessageTypeAck {
				ackReplies.Add(1)
				if acquired.CompareAndSwap(false, true) {
					ackSent <- time.Now()
				} else if replyIP.Equal(net.ParseIP(changedIP)) {
					select {
					case changedAckSent <- time.Now():
					default:
					}
				}
			}
		})
	must(err)
	t.Cleanup(func() { _ = dhcpPeer.Close() })
	go func() {
		if err := dhcpPeer.Serve(); err != nil {
			reportPeerError(err)
		}
	}()

	var v6 *dualStackServiceDHCPv6Peer
	if v6Watch {
		v6 = newDualStackServiceDHCPv6Peer(t, parent, peerLink)
	}
	service := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "lease-proof", Namespace: "default", UID: "12345678-1234-1234-1234-123456789abc",
			Annotations: map[string]string{kubevip.LoadbalancerIPAnnotation: "0.0.0.0"},
		},
		Spec: v1.ServiceSpec{
			Type: v1.ServiceTypeLoadBalancer, IPFamilies: []v1.IPFamily{v1.IPv4Protocol},
			ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyCluster,
			Ports:                 []v1.ServicePort{{Port: 32400, Protocol: v1.ProtocolTCP}},
		},
	}
	if dualStackWatch {
		service.Annotations[kubevip.LoadbalancerIPAnnotation] = "0.0.0.0,::"
		service.Annotations[kubevip.LoadbalancerHostname] = "public-dual-proof"
		service.Spec.IPFamilies = []v1.IPFamily{v1.IPv4Protocol, v1.IPv6Protocol}
		policy := v1.IPFamilyPolicyRequireDualStack
		service.Spec.IPFamilyPolicy = &policy
	} else if ipv6OnlyWatch {
		service.Annotations[kubevip.LoadbalancerIPAnnotation] = "::"
		service.Annotations[kubevip.LoadbalancerHostname] = "public-dual-proof"
		service.Spec.IPFamilies = []v1.IPFamily{v1.IPv6Protocol}
		policy := v1.IPFamilyPolicySingleStack
		service.Spec.IPFamilyPolicy = &policy
	}
	const nodeName = "offline-node"
	if ownedRestart {
		ctx, cancel := context.WithCancel(context.Background())
		var workers sync.WaitGroup
		var built []*instance.Instance
		t.Cleanup(func() {
			cancel()
			for _, inst := range built {
				if err := inst.CleanupLinkAttachments(); err != nil {
					t.Errorf("real owned-restart fixture teardown: %v", err)
				}
			}
			stopped := make(chan struct{})
			go func() { workers.Wait(); close(stopped) }()
			select {
			case <-stopped:
			case <-time.After(3 * time.Second):
				t.Error("real owned-restart workers ignored teardown cancellation")
			}
		})
		config := &kubevip.Config{
			Interface: parent.Attrs().Name, VIPSubnet: "32", DHCPMode: "ipv4", DNSMode: "ipv4",
			DHCPBackoffAttempts: 1, NodeName: nodeName,
		}
		constructorReturned := false
		makeInstance := func(svc *v1.Service) (*instance.Instance, error) {
			constructorReturned = false
			type constructorResult struct {
				inst *instance.Instance
				err  error
			}
			constructed := make(chan constructorResult, 1)
			go func() {
				inst, err := instance.NewInstance(ctx, svc, config, networkinterface.NewManager(),
					nil, nil, noop.NewManager(), &workers)
				constructed <- constructorResult{inst: inst, err: err}
			}()
			select {
			case result := <-constructed:
				constructorReturned = true
				if result.inst != nil {
					built = append(built, result.inst)
				}
				return result.inst, result.err
			case err := <-peerErrors:
				return nil, fmt.Errorf("synthetic owned-restart peer: %w", err)
			case <-time.After(2 * time.Second):
				return nil, fmt.Errorf("real owned-restart constructor did not return with caller live")
			}
		}
		first, err := makeInstance(service.DeepCopy())
		if err != nil || first == nil || !first.IsDHCPv4 || first.DHCPInterfaceIPv4 != leasedIP || ackReplies.Load() < 1 {
			t.Fatalf("fresh real owned-interface control failed: instance=%t ACKs=%d err=%v", first != nil, ackReplies.Load(), err)
		}
		t.Log("fresh real instance created its DHCP attachment and acquired first wire lease")
		fingerprint := func() string {
			t.Helper()
			link, err := netlink.LinkByName(first.DHCPInterface)
			must(err)
			addresses, err := netlink.AddrList(link, netlink.FAMILY_V4)
			must(err)
			attrs := link.Attrs()
			return fmt.Sprintf("%s/%d/%d/%s/%d/%v/%s/%v", link.Type(), attrs.Index,
				attrs.ParentIndex, attrs.HardwareAddr, attrs.MTU, attrs.Flags, attrs.Alias, addresses)
		}
		initialFingerprint := fingerprint()
		if scenario == serviceCleanupReleasesLease {
			clientDone := first.DHCPv4Done()
			if clientDone == nil {
				t.Fatal("real cleanup control lacks Start completion signal")
			}
			select {
			case <-clientDone:
				t.Fatal("real DHCP client ended before immediate cleanup control")
			default:
			}
			var ackAt time.Time
			select {
			case ackAt = <-ackSent:
			default:
				t.Fatal("immediate cleanup control lacks original ACK timestamp")
			}
			var initial [2]*dhcpv4.DHCPv4
			for i := range initial {
				select {
				case initial[i] = <-cleanupInitialRequests:
				case err := <-peerErrors:
					t.Fatalf("synthetic raw cleanup control failed: %v", err)
				case <-time.After(2 * time.Second):
					t.Fatal("raw cleanup observer missed initial real DORA receive control")
				}
			}
			if initial[0].MessageType() != dhcpv4.MessageTypeDiscover || initial[1].MessageType() != dhcpv4.MessageTypeRequest ||
				initial[0].TransactionID != initial[1].TransactionID || initial[0].ClientHWAddr.String() != first.DHCPInterfaceHwaddr ||
				initial[1].ClientHWAddr.String() != first.DHCPInterfaceHwaddr || !initial[1].RequestedIPAddress().Equal(net.ParseIP(leasedIP)) ||
				!initial[1].ServerIdentifier().Equal(peerAddress.IP) {
				t.Fatal("raw cleanup DORA control lacks matching transaction/MAC/server/requested lease")
			}
			original, err := netlink.LinkByName(first.DHCPInterface)
			must(err)
			attrs := original.Attrs()
			if original.Type() != "macvlan" || attrs.Index <= 0 || attrs.ParentIndex != parent.Attrs().Index ||
				attrs.Alias != "kube-vip:dhcp:v1:"+string(service.UID)+":"+first.DHCPInterfaceHwaddr || attrs.Flags&net.FlagUp == 0 {
				t.Fatal("immediate cleanup control lacks original live indexed owned macvlan")
			}
			originalIndex, originalName, originalMAC := attrs.Index, attrs.Name, attrs.HardwareAddr.String()
			updates, subscriptionStop := make(chan netlink.LinkUpdate, 16), make(chan struct{})
			deleted, subscriptionDone := make(chan netlink.LinkUpdate, 1), make(chan struct{})
			must(netlink.LinkSubscribeWithOptions(updates, subscriptionStop, netlink.LinkSubscribeOptions{
				ErrorCallback: func(err error) {
					select {
					case <-subscriptionStop:
						return
					default:
						reportPeerError(fmt.Errorf("synthetic cleanup link observer: %w", err))
					}
				},
			}))
			t.Cleanup(func() {
				close(subscriptionStop)
				select {
				case <-subscriptionDone:
				case <-time.After(2 * time.Second):
					t.Error("synthetic cleanup link observer did not stop")
				}
			})
			go func() {
				defer close(subscriptionDone)
				for update := range updates {
					if update.Header.Type == syscall.RTM_DELLINK && update.Attrs().Index == originalIndex {
						select {
						case deleted <- update:
						default:
						}
					}
				}
			}()
			if time.Since(ackAt) >= leaseLifetime/4 || ctx.Err() != nil || releaseRequests.Load() != 0 || fingerprint() != initialFingerprint {
				t.Fatal("immediate cleanup control lacks unreleased original lease/native identity/caller before T1")
			}
			t.Log("real active lease, raw initial DORA transaction/MAC/server and exact owned native attachment verified before T1; call public cleanup without manual Stop or worker drain")
			// ponytail: exercise public cleanup once; observing Stop before it would hide this race.
			built = nil
			cleaned := make(chan error, 1)
			go func() { cleaned <- first.CleanupLinkAttachments() }()
			select {
			case cleanupErr := <-cleaned:
				must(cleanupErr)
			case err := <-peerErrors:
				t.Fatalf("synthetic peer failed during immediate cleanup: %v", err)
			case <-time.After(2 * time.Second):
				t.Fatal("real public immediate cleanup did not return with caller live")
			}
			startCompletedAtReturn := false
			select {
			case <-clientDone:
				startCompletedAtReturn = true
			default:
			}
			select {
			case <-clientDone:
			case <-time.After(2 * time.Second):
				t.Fatal("real DHCP Start did not complete after immediate cleanup")
			}
			stopped := make(chan struct{})
			go func() { workers.Wait(); close(stopped) }()
			select {
			case <-stopped:
			case <-time.After(2 * time.Second):
				t.Fatal("real DHCP workers did not drain after immediate cleanup")
			}
			select {
			case event := <-deleted:
				if event.Attrs().Name != originalName || event.Attrs().HardwareAddr.String() != originalMAC {
					t.Fatal("kernel deletion event did not identify original cleanup attachment")
				}
			case err := <-peerErrors:
				t.Fatalf("synthetic cleanup kernel observer failed: %v", err)
			case <-time.After(2 * time.Second):
				t.Fatal("kernel observer missed original-index RTM_DELLINK after cleanup")
			}
			_, lookupErr := netlink.LinkByIndex(originalIndex)
			var notFound netlink.LinkNotFoundError
			if !errors.As(lookupErr, &notFound) {
				t.Fatalf("original cleanup attachment absence not verified: %v", lookupErr)
			}
			var released *dhcpv4.DHCPv4
			select {
			case released = <-releaseSeen:
			case err := <-peerErrors:
				t.Fatalf("synthetic cleanup RELEASE observer failed: %v", err)
			case <-time.After(2 * time.Second):
			}
			select {
			case err := <-peerErrors:
				t.Fatalf("synthetic immediate cleanup observer failed: %v", err)
			default:
			}
			if ctx.Err() != nil || discoverRequests.Load() != 1 || ackReplies.Load() != 1 || rawPeerPackets.Load() < 2 {
				t.Fatal("immediate cleanup lost caller/initial wire controls or unexpectedly reacquired")
			}
			t.Logf("kernel RTM_DELLINK/native absence verified; real cleanup/Start/workers complete with caller live; Start complete at cleanup return=%t raw RELEASEs=%d", startCompletedAtReturn, releaseRequests.Load())
			if released == nil {
				if releaseRequests.Load() != 0 {
					t.Fatal("raw cleanup RELEASE count/channel observation disagreed")
				}
				t.Fatal("real instance cleanup deleted its original DHCP interface before the stopped client emitted lease RELEASE; RTM_DELLINK/raw receive control and caller live")
			}
			if !released.ClientIPAddr.Equal(net.ParseIP(leasedIP)) || released.ClientHWAddr.String() != originalMAC ||
				!released.ServerIdentifier().Equal(peerAddress.IP) || releaseRequests.Load() != 1 {
				t.Fatal("immediate cleanup wire RELEASE did not match original lease/MAC/server")
			}
			if !startCompletedAtReturn {
				t.Fatal("real public cleanup returned before Start completion despite receiving RELEASE")
			}
			t.Log("real instance cleanup awaited Start and emitted matching lease RELEASE before deleting its original attachment; RTM_DELLINK/raw control and caller live")
			return
		}
		if scenario == serviceReleaseChangedOwner {
			select {
			case <-first.DHCPv4Done():
				t.Fatal("real lease client ended before same-index owner mutation control")
			default:
			}
			if ctx.Err() != nil || releaseRequests.Load() != 0 {
				t.Fatal("same-index owner mutation control lacks live caller and unreleased lease")
			}
			original, err := netlink.LinkByName(first.DHCPInterface)
			must(err)
			originalMAC, ok := original.(*netlink.Macvlan)
			if !ok || original.Attrs().Index <= 0 || original.Attrs().Alias == "" {
				t.Fatal("same-index owner control lacks authentic indexed marked macvlan")
			}
			originalIndex, originalAlias := original.Attrs().Index, original.Attrs().Alias
			const foreignAlias = "external-owner/changed-active-dhcp-interface"
			// ponytail: mutate only the owner, independently of the frozen replacement trace.
			must(netlink.LinkSetAlias(original, foreignAlias))
			changed, err := netlink.LinkByIndex(originalIndex)
			must(err)
			changedMAC, ok := changed.(*netlink.Macvlan)
			beforeStop := fingerprint()
			if !ok || changed.Attrs().Index != originalIndex || changed.Attrs().Alias != foreignAlias ||
				changedMAC.Mode != originalMAC.Mode || beforeStop != strings.Replace(initialFingerprint, originalAlias, foreignAlias, 1) || ctx.Err() != nil {
				t.Fatal("same-index owner mutation changed more than the native owner alias")
			}
			select {
			case <-first.DHCPv4Done():
				t.Fatal("real lease client ended before Stop with changed native owner")
			default:
			}
			if releaseRequests.Load() != 0 {
				t.Fatal("same-index owner mutation control already released its active lease")
			}
			discoversBefore, acksBefore := discoverRequests.Load(), ackReplies.Load()
			t.Log("external actor changed only native owner alias on same real index/name/type/parent/MAC/mode/up/address attachment; active lease unreleased and caller live")
			first.DHCPv4Client.Stop()
			select {
			case <-first.DHCPv4Done():
			case <-time.After(2 * time.Second):
				t.Fatal("real client Start did not finish after same-index owner mutation and Stop")
			}
			stopped := make(chan struct{})
			go func() { workers.Wait(); close(stopped) }()
			select {
			case <-stopped:
			case <-time.After(2 * time.Second):
				t.Fatal("real client workers did not drain after same-index owner mutation and Stop")
			}
			built = nil // Keep product cleanup out of the active transport observation.
			if ctx.Err() != nil {
				t.Fatal("caller cancelled during same-index owner mutation Stop")
			}
			t.Log("real client Stop and Start/workers completed after same-index native owner mutation; caller live")
			select {
			case packet := <-releaseSeen:
				if !packet.ClientIPAddr.Equal(net.ParseIP(leasedIP)) || packet.ClientHWAddr.String() != first.DHCPInterfaceHwaddr {
					t.Fatal("wire RELEASE after same-index owner mutation did not match real old lease identity")
				}
				t.Fatal("real DHCP client released its old lease through a same-index attachment with changed native owner; caller live")
			case err := <-peerErrors:
				t.Fatalf("synthetic peer failed during same-index owner release observation: %v", err)
			case <-time.After(250 * time.Millisecond):
			}
			live, err := netlink.LinkByIndex(originalIndex)
			must(err)
			liveMAC, ok := live.(*netlink.Macvlan)
			if !ok || liveMAC.Mode != changedMAC.Mode || fingerprint() != beforeStop || releaseRequests.Load() != 0 ||
				discoverRequests.Load() != discoversBefore || ackReplies.Load() != acksBefore || ctx.Err() != nil {
				t.Fatal("same-index owner mutation Stop changed attachment or emitted DHCP traffic")
			}
			t.Log("real DHCP client stopped without releasing old lease after same-index native owner mutation; exact changed-owner attachment preserved and caller live")
			return
		}
		if scenario == serviceReleaseReplacedInterface {
			select {
			case <-first.DHCPv4Done():
				t.Fatal("real lease client ended before active release-substitution control")
			default:
			}
			if ctx.Err() != nil || releaseRequests.Load() != 0 {
				t.Fatal("active release-substitution control lacks live caller and unreleased lease")
			}
			// ponytail: keep this active replacement independent of the frozen stopped-client cleanup trace.
			original, err := netlink.LinkByName(first.DHCPInterface)
			must(err)
			originalMAC, ok := original.(*netlink.Macvlan)
			if !ok || original.Attrs().Index <= 0 || original.Attrs().Alias == "" {
				t.Fatal("active release control lacks authentic indexed marked macvlan")
			}
			originalIndex := original.Attrs().Index
			must(netlink.LinkDel(original))
			foreign := &netlink.Macvlan{
				LinkAttrs: netlink.LinkAttrs{
					Name: original.Attrs().Name, ParentIndex: original.Attrs().ParentIndex,
					HardwareAddr: original.Attrs().HardwareAddr, Alias: "external-owner/replaced-active-dhcp-interface",
				},
				Mode: netlink.MACVLAN_MODE_DEFAULT,
			}
			must(netlink.LinkAdd(foreign))
			must(netlink.LinkSetAlias(foreign, foreign.Attrs().Alias))
			must(netlink.LinkSetUp(foreign))
			replacement, err := netlink.LinkByName(foreign.Attrs().Name)
			must(err)
			replacementMAC, ok := replacement.(*netlink.Macvlan)
			attrs := replacement.Attrs()
			if !ok || attrs.Index <= 0 || attrs.Index == originalIndex || attrs.Name != original.Attrs().Name ||
				attrs.ParentIndex != original.Attrs().ParentIndex || attrs.HardwareAddr.String() != original.Attrs().HardwareAddr.String() ||
				attrs.Alias != foreign.Attrs().Alias || attrs.Flags&net.FlagUp == 0 || replacementMAC.Mode != originalMAC.Mode || ctx.Err() != nil {
				t.Fatal("active release foreign replacement readback failed")
			}
			_, err = netlink.LinkByIndex(originalIndex)
			var notFound netlink.LinkNotFoundError
			if !errors.As(err, &notFound) {
				t.Fatalf("active release original index was not absent: %v", err)
			}
			beforeReplacement := fingerprint()
			discoversBefore, acksBefore := discoverRequests.Load(), ackReplies.Load()
			t.Log("active original lease attachment replaced with same-name/type/parent/MAC/mode foreign-alias distinct-index macvlan; original index absent and caller live")
			first.DHCPv4Client.Stop()
			select {
			case <-first.DHCPv4Done():
			case <-time.After(2 * time.Second):
				t.Fatal("real client Start did not finish after replacement and Stop")
			}
			stopped := make(chan struct{})
			go func() { workers.Wait(); close(stopped) }()
			select {
			case <-stopped:
			case <-time.After(2 * time.Second):
				t.Fatal("real client workers did not drain after replacement and Stop")
			}
			built = nil // Real client/workers ended; fixture must not invoke product cleanup to hide transport behavior.
			if ctx.Err() != nil {
				t.Fatal("caller cancelled during active release-substitution Stop")
			}
			t.Log("real client Stop and Start/workers completed after foreign replacement; caller live")
			select {
			case packet := <-releaseSeen:
				if !packet.ClientIPAddr.Equal(net.ParseIP(leasedIP)) || packet.ClientHWAddr.String() != first.DHCPInterfaceHwaddr {
					t.Fatal("wire RELEASE after replacement did not match the real old lease identity")
				}
				t.Fatal("real DHCP client released its old lease through a same-name foreign replacement; caller live")
			case err := <-peerErrors:
				t.Fatalf("synthetic peer failed during active release observation: %v", err)
			case <-time.After(250 * time.Millisecond): // Bounded local-peer silence after actual client completion.
			}
			live, err := netlink.LinkByIndex(attrs.Index)
			must(err)
			liveMAC, ok := live.(*netlink.Macvlan)
			if !ok || live.Attrs().Name != attrs.Name || liveMAC.Mode != replacementMAC.Mode || fingerprint() != beforeReplacement ||
				releaseRequests.Load() != 0 || discoverRequests.Load() != discoversBefore || ackReplies.Load() != acksBefore || ctx.Err() != nil {
				t.Fatal("active release-substitution Stop changed foreign replacement or emitted DHCP traffic")
			}
			t.Log("real DHCP client stopped without releasing old lease through foreign attachment; replacement unchanged and caller live")
			return
		}
		first.DHCPv4Client.Stop()
		select {
		case <-first.DHCPv4Done():
		case <-time.After(2 * time.Second):
			t.Fatal("first real DHCP client did not stop before owned restart")
		}
		stopped := make(chan struct{})
		go func() { workers.Wait(); close(stopped) }()
		select {
		case <-stopped:
		case <-time.After(2 * time.Second):
			t.Fatal("first real DHCP workers did not drain before owned restart")
		}
		before := fingerprint()
		if ctx.Err() != nil {
			t.Fatal("caller cancelled before owned-interface restart boundary")
		}
		t.Log("first real DHCP client and workers stopped; authentic attachment retained and caller live")
		if scenario == serviceReleaseOriginalInterface {
			select {
			case packet := <-releaseSeen:
				if !packet.ClientIPAddr.Equal(net.ParseIP(leasedIP)) || packet.ClientHWAddr.String() != first.DHCPInterfaceHwaddr {
					t.Fatal("normal wire RELEASE control did not match the real lease identity")
				}
			case err := <-peerErrors:
				t.Fatalf("synthetic peer failed during normal wire RELEASE control: %v", err)
			case <-time.After(2 * time.Second):
				t.Fatal("synthetic peer observed no normal wire RELEASE after real Stop")
			}
			if ctx.Err() != nil || releaseRequests.Load() != 1 || before != initialFingerprint || fingerprint() != before {
				t.Fatal("normal real RELEASE did not preserve original attachment and live caller")
			}
			t.Log("synthetic peer received valid normal wire RELEASE for real lease and MAC after Stop; original attachment unchanged and caller live")
			return
		}
		if scenario == serviceCleanupReplacedInterface {
			original, err := netlink.LinkByName(first.DHCPInterface)
			must(err)
			originalMAC, ok := original.(*netlink.Macvlan)
			if !ok || original.Attrs().Index <= 0 || original.Attrs().Alias == "" {
				t.Fatal("real owned cleanup control lacks authentic indexed marked macvlan")
			}
			originalIndex := original.Attrs().Index
			must(netlink.LinkDel(original)) // External actor replaces a link only after its real client stopped.
			foreign := &netlink.Macvlan{
				LinkAttrs: netlink.LinkAttrs{
					Name: original.Attrs().Name, ParentIndex: original.Attrs().ParentIndex,
					HardwareAddr: original.Attrs().HardwareAddr, Alias: "external-owner/replaced-dhcp-interface",
				},
				Mode: netlink.MACVLAN_MODE_DEFAULT,
			}
			must(netlink.LinkAdd(foreign))
			must(netlink.LinkSetAlias(foreign, foreign.Attrs().Alias))
			must(netlink.LinkSetUp(foreign))
			replacement, err := netlink.LinkByName(foreign.Attrs().Name)
			must(err)
			replacementMAC, ok := replacement.(*netlink.Macvlan)
			attrs := replacement.Attrs()
			if !ok || attrs.Index <= 0 || attrs.Index == originalIndex || attrs.Name != original.Attrs().Name ||
				attrs.ParentIndex != original.Attrs().ParentIndex || attrs.HardwareAddr.String() != original.Attrs().HardwareAddr.String() ||
				attrs.Alias != foreign.Attrs().Alias || attrs.Flags&net.FlagUp == 0 || replacementMAC.Mode != originalMAC.Mode || ctx.Err() != nil {
				t.Fatalf("foreign cleanup replacement readback failed: type=%s index=%d original-index=%d alias=%q", replacement.Type(), attrs.Index, originalIndex, attrs.Alias)
			}
			foreignIndex, foreignName, foreignMode := attrs.Index, attrs.Name, replacementMAC.Mode
			beforeReplacement := fingerprint()
			discoversBefore, acksBefore := discoverRequests.Load(), ackReplies.Load()
			t.Log("external actor replaced original attachment: same name/type/parent/MAC/mode, distinct index and foreign alias; caller live")
			// The actual client/workers are stopped; exercise public cleanup once, not again during fixture teardown.
			built = nil
			cleaned := make(chan error, 1)
			go func() { cleaned <- first.CleanupLinkAttachments() }()
			select {
			case cleanupErr := <-cleaned:
				if ctx.Err() != nil {
					t.Fatal("caller cancelled during real replaced-interface cleanup")
				}
				t.Logf("real instance cleanup returned with caller live: %v", cleanupErr)
			case err := <-peerErrors:
				t.Fatalf("synthetic peer failed during real replaced-interface cleanup: %v", err)
			case <-time.After(2 * time.Second):
				t.Fatal("real replaced-interface cleanup did not return with caller live")
			}
			live, err := netlink.LinkByIndex(foreignIndex)
			if err != nil {
				var notFound netlink.LinkNotFoundError
				if errors.As(err, &notFound) {
					t.Fatal("real instance cleanup deleted a same-name foreign DHCP replacement after original client/workers stopped; caller live")
				}
				t.Fatalf("foreign replacement readback failed after real cleanup: %v", err)
			}
			liveMAC, ok := live.(*netlink.Macvlan)
			addresses, err := netlink.AddrList(live, netlink.FAMILY_V4)
			must(err)
			attrs = live.Attrs()
			afterReplacement := fmt.Sprintf("%s/%d/%d/%s/%d/%v/%s/%v", live.Type(), attrs.Index,
				attrs.ParentIndex, attrs.HardwareAddr, attrs.MTU, attrs.Flags, attrs.Alias, addresses)
			if !ok || attrs.Name != foreignName || liveMAC.Mode != foreignMode || afterReplacement != beforeReplacement ||
				discoverRequests.Load() != discoversBefore || ackReplies.Load() != acksBefore || ctx.Err() != nil {
				t.Fatal("real instance cleanup mutated foreign replacement or reached DHCP after original client/workers stopped")
			}
			t.Log("real instance cleanup preserved exact same-name foreign DHCP replacement after original client/workers stopped; caller live")
			return
		}
		restartService := service.DeepCopy()
		if scenario == serviceOwnedInstanceRestartWithoutMAC {
			delete(restartService.Annotations, kubevip.HwAddrKey)
			if _, present := restartService.Annotations[kubevip.HwAddrKey]; present {
				t.Fatal("absent-MAC restart control unexpectedly supplied a MAC annotation")
			}
			t.Log("same-Service restart caller supplied no MAC annotation; native attachment retained")
		} else {
			restartService.Annotations[kubevip.HwAddrKey] = first.DHCPInterfaceHwaddr
		}
		if scenario == serviceRejectOwnedInterfaceUIDCollision {
			restartService.UID = "12345678-1234-1234-1234-123456789abd"
			if restartService.UID == service.UID || restartService.UID[:8] != service.UID[:8] {
				t.Fatal("full-UID collision control does not share only the interface-name prefix")
			}
			t.Log("different full Service UID shares interface-name prefix; explicit original MAC and authentic attachment retained")
		}
		discoversBefore, acksBefore := discoverRequests.Load(), ackReplies.Load()
		second, err := makeInstance(restartService)
		if scenario == serviceRejectOwnedInterfaceUIDCollision {
			if !constructorReturned || err == nil || second != nil || ctx.Err() != nil {
				t.Fatalf("different full UID was not rejected by actual constructor before DHCP: returned=%t instance=%t err=%v", constructorReturned, second != nil, err)
			}
			drained := make(chan struct{})
			go func() { workers.Wait(); close(drained) }()
			select {
			case <-drained:
			case <-time.After(2 * time.Second):
				t.Fatal("rejected full-UID collision left real DHCP workers active with caller live")
			}
			if fingerprint() != before || discoverRequests.Load() != discoversBefore || ackReplies.Load() != acksBefore || ctx.Err() != nil {
				t.Fatalf("full-UID collision mutated authentic attachment or reached DHCP: new DISCOVERs=%d ACKs=%d", discoverRequests.Load()-discoversBefore, ackReplies.Load()-acksBefore)
			}
			t.Log("different full Service UID sharing the interface-name prefix rejected before DHCP; authentic attachment unchanged and caller live")
			return
		}
		if err != nil || second == nil {
			t.Fatalf("fresh instance could not restart the same Service's retained owned DHCP interface: %v", err)
		}
		if ctx.Err() != nil || ackReplies.Load() < 2 || second.DHCPInterfaceIPv4 != leasedIP ||
			second.DHCPInterfaceHwaddr != first.DHCPInterfaceHwaddr || fingerprint() != before {
			t.Fatalf("owned restart did not preserve exact attachment identity and reacquire real lease with caller live: ACKs=%d", ackReplies.Load())
		}
		t.Log("fresh same-Service instance reused exact authentic DHCP attachment and acquired second wire lease; caller live")
		return
	}
	if scenario == serviceRejectUnownedInterface {
		foreignMAC, err := net.ParseMAC("02:00:00:00:01:02")
		must(err)
		service.Annotations[kubevip.HwAddrKey] = foreignMAC.String()
		foreign := &netlink.Macvlan{
			LinkAttrs: netlink.LinkAttrs{
				Name: "vip-12345678", ParentIndex: parent.Attrs().Index,
				HardwareAddr: foreignMAC, Alias: "external-owner/not-kube-vip",
			},
			Mode: netlink.MACVLAN_MODE_DEFAULT,
		}
		must(netlink.LinkAdd(foreign))
		must(netlink.LinkSetAlias(foreign, foreign.Attrs().Alias))
		must(netlink.LinkSetUp(foreign))
		existing, err := netlink.LinkByName(foreign.Attrs().Name)
		must(err)
		attrs := existing.Attrs()
		if existing.Type() != "macvlan" || attrs.ParentIndex != parent.Attrs().Index ||
			attrs.HardwareAddr.String() != foreignMAC.String() || attrs.Alias != foreign.Attrs().Alias || attrs.Flags&net.FlagUp == 0 {
			t.Fatalf("synthetic foreign macvlan did not match the required up/type/parent/MAC/foreign-owner control: type=%q parent=%d expected-parent=%d MAC=%s expected-MAC=%s alias=%q expected-alias=%q flags=%v",
				existing.Type(), attrs.ParentIndex, parent.Attrs().Index, attrs.HardwareAddr, foreignMAC, attrs.Alias, foreign.Attrs().Alias, attrs.Flags)
		}
		fingerprint := func() string {
			t.Helper()
			link, err := netlink.LinkByName(foreign.Attrs().Name)
			must(err)
			addresses, err := netlink.AddrList(link, netlink.FAMILY_V4)
			must(err)
			attrs := link.Attrs()
			return fmt.Sprintf("%s/%d/%d/%s/%d/%v/%s/%v", link.Type(), attrs.Index,
				attrs.ParentIndex, attrs.HardwareAddr, attrs.MTU, attrs.Flags, attrs.Alias, addresses)
		}
		before := fingerprint()
		t.Log("correct-shape pre-existing foreign-owned macvlan is up; parent and MAC match explicit Service inputs")
		ctx, cancel := context.WithCancel(context.Background())
		var workers sync.WaitGroup
		type constructorResult struct {
			inst *instance.Instance
			err  error
		}
		constructed := make(chan constructorResult, 1)
		var built *instance.Instance
		received := false
		t.Cleanup(func() {
			cancel()
			if !received {
				select {
				case result := <-constructed:
					built = result.inst
				case <-time.After(3 * time.Second):
					t.Error("real instance constructor ignored teardown cancellation")
				}
			}
			if built != nil {
				if err := built.CleanupLinkAttachments(); err != nil {
					t.Errorf("real instance fixture teardown: %v", err)
				}
			}
			stopped := make(chan struct{})
			go func() { workers.Wait(); close(stopped) }()
			select {
			case <-stopped:
			case <-time.After(3 * time.Second):
				t.Error("real unowned-interface DHCP workers ignored teardown cancellation")
			}
		})
		config := &kubevip.Config{
			Interface: parent.Attrs().Name, VIPSubnet: "32", DHCPMode: "ipv4", DNSMode: "ipv4",
			DHCPBackoffAttempts: 1, NodeName: nodeName,
		}
		go func() {
			built, err := instance.NewInstance(ctx, service, config, networkinterface.NewManager(),
				nil, nil, noop.NewManager(), &workers)
			constructed <- constructorResult{inst: built, err: err}
		}()
		select {
		case result := <-constructed:
			received, built = true, result.inst
			if result.err == nil || built != nil {
				t.Fatal("real instance constructor accepted an unowned existing DHCP interface")
			}
			if ctx.Err() != nil || discoverRequests.Load() != 0 || fingerprint() != before {
				t.Fatal("unowned DHCP interface rejection did not precede wire acquisition and link mutation with caller live")
			}
			t.Log("real instance constructor rejected unowned matching-name macvlan before DHCP and preserved foreign link with caller live")
		case <-ackSent:
			t.Fatalf("unowned existing DHCP interface reached real wire ACK before ownership validation; discovers=%d", discoverRequests.Load())
		case err := <-peerErrors:
			t.Fatalf("synthetic peer failed during unowned-interface control: %v", err)
		case <-time.After(2 * time.Second):
			t.Fatalf("real instance constructor did not reject unowned interface before acquisition; discovers=%d link-unchanged=%t", discoverRequests.Load(), fingerprint() == before)
		}
		return
	}
	endpoint := &v1.Endpoints{
		TypeMeta: metav1.TypeMeta{Kind: "Endpoints", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{
			Name: service.Name, Namespace: service.Namespace, ResourceVersion: "2",
		},
		Subsets: []v1.EndpointSubset{{Addresses: []v1.EndpointAddress{{IP: "192.0.2.100", NodeName: new(nodeName)}}}},
	}
	if dualStackWatch {
		endpoint.Subsets[0].Addresses = append(endpoint.Subsets[0].Addresses, v1.EndpointAddress{IP: "2001:db8:1::100", NodeName: new(nodeName)})
	} else if ipv6OnlyWatch {
		endpoint.Subsets[0].Addresses[0].IP = "2001:db8:1::100"
	}
	apiBlocked := make(chan struct{})
	replacementAPIBlocked := make(chan struct{})
	statusPublished := make(chan struct{})
	changedStatusPublished := make(chan struct{})
	allowFeedback := make(chan struct{})
	feedbackDelivered := make(chan struct{})
	serviceEvents := make(chan watch.Event, 32)
	deletionDelivered, endpointStopped := make(chan struct{}), make(chan struct{})
	var blockOnce, replacementBlockOnce, statusOnce, changedStatusOnce, feedbackOnce, deletionOnce, endpointOnce sync.Once
	var apiServiceDeleted bool
	var apiMu sync.Mutex
	apiService := service.DeepCopy()
	apiService.TypeMeta = metav1.TypeMeta{Kind: "Service", APIVersion: "v1"}
	apiVersion := 2
	apiService.ResourceVersion = strconv.Itoa(apiVersion)
	apiPeer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if stallAfterReplacement && replacementAcknowledged.Load() &&
			strings.HasPrefix(r.URL.Path, "/api/v1/namespaces/default/services/lease-proof") &&
			(r.Method == http.MethodGet || r.Method == http.MethodPut) {
			replacementBlockOnce.Do(func() { close(replacementAPIBlocked) })
			<-r.Context().Done()
			return
		}
		encode := func(value any) bool {
			if err := json.NewEncoder(w).Encode(value); err != nil {
				if r.Context().Err() == nil {
					t.Errorf("synthetic Kubernetes response: %v", err)
				}
				return false
			}
			return true
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/namespaces/default/endpoints" && r.URL.Query().Get("watch") == "true":
			if !encode(map[string]any{"type": "ADDED", "object": endpoint}) {
				return
			}
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			if watchDelete {
				endpointOnce.Do(func() { close(endpointStopped) })
			}
		case watchStatusFeedback && r.Method == http.MethodGet && r.URL.Path == "/api/v1/namespaces/default/services":
			apiMu.Lock()
			current, gone := apiService.DeepCopy(), apiServiceDeleted
			apiMu.Unlock()
			if r.URL.Query().Get("watch") != "true" {
				items := []v1.Service{*current}
				if gone {
					items = nil
				}
				encode(&v1.ServiceList{TypeMeta: metav1.TypeMeta{Kind: "ServiceList", APIVersion: "v1"}, ListMeta: metav1.ListMeta{ResourceVersion: current.ResourceVersion}, Items: items})
				return
			}
			if !gone {
				if !encode(map[string]any{"type": "ADDED", "object": current}) {
					return
				}
				w.(http.Flusher).Flush()
			}
			for {
				select {
				case event := <-serviceEvents:
					updated := event.Object.(*v1.Service)
					select {
					case <-allowFeedback:
					case <-r.Context().Done():
						return
					}
					if !encode(map[string]any{"type": event.Type, "object": updated}) {
						return
					}
					w.(http.Flusher).Flush()
					if event.Type == watch.Deleted {
						deletionOnce.Do(func() { close(deletionDelivered) })
					} else if len(updated.Status.LoadBalancer.Ingress) > 0 {
						feedbackOnce.Do(func() { close(feedbackDelivered) })
					}
				case <-r.Context().Done():
					return
				}
			}
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/namespaces/default/services/lease-proof":
			if !watchStatusFeedback {
				blockOnce.Do(func() { close(apiBlocked) })
				<-r.Context().Done() // No API response may release the in-flight Service lock.
				return
			}
			apiMu.Lock()
			if apiServiceDeleted {
				apiMu.Unlock()
				w.WriteHeader(http.StatusNotFound)
				encode(&metav1.Status{TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"}, Status: metav1.StatusFailure, Reason: metav1.StatusReasonNotFound, Code: http.StatusNotFound})
				return
			}
			current := apiService.DeepCopy()
			apiMu.Unlock()
			encode(current)
		case watchStatusFeedback && r.Method == http.MethodPut && (r.URL.Path == "/api/v1/namespaces/default/services/lease-proof" || r.URL.Path == "/api/v1/namespaces/default/services/lease-proof/status"):
			var updated v1.Service
			if err := json.NewDecoder(r.Body).Decode(&updated); err != nil || updated.UID != service.UID || updated.Name != service.Name || updated.Namespace != service.Namespace {
				t.Error("invalid synthetic Service update")
				http.Error(w, "invalid update", http.StatusBadRequest)
				return
			}
			apiMu.Lock()
			if apiServiceDeleted {
				apiMu.Unlock()
				w.WriteHeader(http.StatusNotFound)
				encode(&metav1.Status{TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"}, Status: metav1.StatusFailure, Reason: metav1.StatusReasonNotFound, Code: http.StatusNotFound})
				return
			}
			if strings.HasSuffix(r.URL.Path, "/status") {
				current := apiService.DeepCopy()
				current.Status = updated.Status
				updated = *current
			} else {
				updated.Status = apiService.DeepCopy().Status
			}
			apiVersion++
			updated.ResourceVersion = strconv.Itoa(apiVersion)
			updated.TypeMeta = metav1.TypeMeta{Kind: "Service", APIVersion: "v1"}
			apiService = updated.DeepCopy()
			// Keep external API mutation and watch-event order atomic, including deletion.
			select {
			case serviceEvents <- watch.Event{Type: watch.Modified, Object: updated.DeepCopy()}:
			default:
				t.Error("synthetic Service watch queue overflow")
			}
			apiMu.Unlock()
			if !encode(&updated) {
				return
			}
			for _, ingress := range updated.Status.LoadBalancer.Ingress {
				if ingress.IP == expectedIP {
					statusOnce.Do(func() { close(statusPublished) })
				}
				if ingress.IP == changedIP {
					changedStatusOnce.Do(func() { close(changedStatusPublished) })
				}
			}
		default:
			t.Errorf("unexpected synthetic Kubernetes request: %s %s", r.Method, r.URL.RequestURI())
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	t.Cleanup(apiPeer.Close)
	clientSet, err := kubernetes.NewForConfig(&rest.Config{
		Host: apiPeer.URL, ContentConfig: rest.ContentConfig{ContentType: "application/json"},
	})
	must(err)
	config := &kubevip.Config{
		Interface: parent.Attrs().Name, EnableServices: true, EnableEndpoints: true,
		NodeName: nodeName, VIPSubnet: "32", DHCPMode: "ipv4", DNSMode: "ipv4", DHCPBackoffAttempts: 1,
		DebounceTime: "0s",
	}
	if watchStatusFeedback {
		config.ServiceNamespace = service.Namespace
	}
	if dualStackWatch {
		config.VIPSubnet, config.DHCPMode, config.DNSMode = "32,128", "dual", "dual"
	} else if ipv6OnlyWatch {
		config.VIPSubnet, config.DHCPMode, config.DNSMode = "32,128", "ipv6", "ipv6"
	}
	processor, err := NewServicesProcessor(config, nil, clientSet, clientSet,
		networkinterface.NewManager(), nil, noop.NewManager(), nil, lease.NewManager(), nil)
	must(err)
	ctx, cancel := context.WithCancelCause(context.Background())
	var workers sync.WaitGroup
	var watcherStopped chan error
	t.Cleanup(func() {
		cancel(nil)
		processor.Stop()
		stopped := make(chan struct{})
		go func() { workers.Wait(); close(stopped) }()
		select {
		case <-stopped:
		case <-time.After(3 * time.Second):
			t.Error("real Service workers ignored teardown cancellation")
		}
		if watcherStopped != nil {
			select {
			case <-watcherStopped:
			case <-time.After(3 * time.Second):
				t.Error("real ServicesWatcher ignored teardown cancellation")
			}
		}
	})
	if watchStatusFeedback {
		watcherStopped = make(chan error, 1)
		go func() {
			watcherStopped <- processor.ServicesWatcher(ctx, processor.SyncServices, false)
			close(watcherStopped)
		}()
	} else {
		must(processor.Reconcile(ctx, watch.Event{Type: watch.Added, Object: service},
			processor.SyncServices, false, &workers, cancel))
	}

	var acknowledgedAt time.Time
	if ipv6OnlyWatch {
		v6.checkInitial(t)
		acknowledgedAt = v6.ackAt
	} else {
		select {
		case acknowledgedAt = <-ackSent:
		case err := <-peerErrors:
			t.Fatalf("synthetic DHCP peer failed before Service acquisition: %v", err)
		case err := <-watcherStopped:
			t.Fatalf("real ServicesWatcher stopped before DHCP acquisition: %v", err)
		case <-time.After(15 * time.Second):
			t.Fatal("synthetic peer sent no ACK; real Service acquisition boundary was not exercised")
		}
	}
	apiBoundary := apiBlocked
	if watchStatusFeedback {
		apiBoundary = statusPublished
	}
	select {
	case <-apiBoundary:
	case err := <-peerErrors:
		t.Fatalf("synthetic DHCP peer failed before Service activation: %v", err)
	case <-ctx.Done():
		t.Fatalf("real Service failed before API-stall boundary: %v", context.Cause(ctx))
	case <-time.After(time.Until(acknowledgedAt.Add(leaseLifetime))):
		t.Fatal("real Service never reached the status API stall within its lease lifetime")
	}
	vipPresent := func(ip string) bool {
		t.Helper()
		link, err := netlink.LinkByName("vip-12345678")
		if _, missing := err.(netlink.LinkNotFoundError); missing {
			return false
		}
		must(err)
		family := netlink.FAMILY_V4
		if v6Watch {
			family = netlink.FAMILY_ALL
		}
		addresses, err := netlink.AddrList(link, family)
		must(err)
		for _, address := range addresses {
			if address.IP.String() == ip {
				return true
			}
		}
		return false
	}
	if v6Watch {
		if !ipv6OnlyWatch {
			v6.checkInitial(t)
		}
		deadline := time.Now().Add(2 * time.Second)
		for {
			apiMu.Lock()
			v4Status, v6Status := false, false
			for _, ingress := range apiService.Status.LoadBalancer.Ingress {
				v4Status = v4Status || ingress.IP == expectedIP
				v6Status = v6Status || ingress.IP == v6.ip.String()
			}
			apiMu.Unlock()
			if vipPresent(expectedIP) && vipPresent(v6.ip.String()) && v4Status && v6Status {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("dual-stack initial kernel/status control missing: ipv4=%t ipv6=%t status4=%t status6=%t", vipPresent(expectedIP), vipPresent(v6.ip.String()), v4Status, v6Status)
			}
			select {
			case err := <-v6.errors:
				t.Fatalf("dual-stack IPv6 initial peer/observer failed: %v", err)
			case err := <-watcherStopped:
				t.Fatalf("dual-stack watcher ended before initial control: %v", err)
			case <-ctx.Done():
				t.Fatalf("dual-stack initial caller cancelled: %v", context.Cause(ctx))
			case <-time.After(10 * time.Millisecond):
			}
		}
		starts4, starts6 := dualStackServiceRunningStarts(t)
		expectedStarts4 := 1
		if ipv6OnlyWatch {
			expectedStarts4 = 0
		}
		if starts4 != expectedStarts4 || starts6 != 1 || v6.socketFree(t) || v6.fingerprint() != v6.before || ctx.Err() != nil || !time.Now().Before(acknowledgedAt.Add(4*time.Second)) || !time.Now().Before(v6.ackAt.Add(4*time.Second)) {
			t.Fatalf("dual-stack real Start/native socket/caller/pre-T1 control invalid: starts4=%d starts6=%d", starts4, starts6)
		}
		if ipv6OnlyWatch {
			t.Log("real IPv6-only ServicesWatcher acquired one8s lease and published its kernel VIP/status; independent raw/peer/DUID/server/IAID/transaction and actual single IPv6 Start/native socket controls live before T1 with no IPv4 client or completion wait")
		} else {
			t.Log("real dual-stack ServicesWatcher acquired both8s leases and published both kernel VIPs/statuses; independent IPv6 raw/peer/DUID/server/IAID/transaction controls and actual IPv4+IPv6 Start frames/native socket/caller live before T1")
		}
	}
	if !vipPresent(expectedIP) || !time.Now().Before(acknowledgedAt.Add(leaseLifetime)) {
		t.Fatal("real Reconcile/SyncServices did not install the leased VIP before expiry")
	}
	if watchStatusFeedback {
		t.Log("real ServicesWatcher acquired wire DHCP lease and installed kernel VIP; real API status published with Modified feedback held")
		close(allowFeedback)
		select {
		case <-feedbackDelivered:
		case err := <-watcherStopped:
			t.Fatalf("real ServicesWatcher stopped before status feedback: %v", err)
		case <-ctx.Done():
			t.Fatalf("real Service watch cancelled caller before status feedback: %v", context.Cause(ctx))
		case <-time.After(2 * time.Second):
			t.Fatal("synthetic API emitted no status Modified feedback; watch boundary not exercised")
		}
		t.Log("synthetic API delivered real Service status Modified feedback before lease expiry")
		if watchDelete {
			// Observe the real published instance without replacing any product behavior.
			processor.instancesMutex.RLock()
			var active *instance.Instance
			for _, candidate := range processor.ServiceInstances {
				if candidate != nil && candidate.UID() == service.UID {
					active = candidate
				}
			}
			processor.instancesMutex.RUnlock()
			if active == nil || (!ipv6OnlyWatch && active.DHCPv4Done() == nil) {
				t.Fatal("watched deletion control lacks real active DHCP instance/completion signal")
			}
			if ipv6OnlyWatch && (!active.IsDHCPv6 || active.DHCPv6Client == nil || active.IsDHCPv4 || active.DHCPv4Client != nil || active.DHCPv4Done() != nil || active.DHCPInterfaceIPv6 != expectedIP) {
				t.Fatal("IPv6-only watched deletion control unexpectedly has an IPv4 client/completion signal or lacks its real IPv6 lease")
			}
			clientDone := active.DHCPv4Done()
			select {
			case <-clientDone:
				t.Fatal("real DHCP Start completed before watched deletion control")
			default:
			}
			original, err := netlink.LinkByName(active.DHCPInterface)
			must(err)
			attrs := original.Attrs()
			if original.Type() != "macvlan" || attrs.Index <= 0 || attrs.ParentIndex != parent.Attrs().Index ||
				attrs.Alias != "kube-vip:dhcp:v1:"+string(service.UID)+":"+active.DHCPInterfaceHwaddr || attrs.Flags&net.FlagUp == 0 {
				t.Fatal("watched deletion control lacks original live indexed owned macvlan")
			}
			originalIndex, originalName, originalMAC := attrs.Index, attrs.Name, attrs.HardwareAddr.String()
			if !ipv6OnlyWatch {
				var initial [2]*dhcpv4.DHCPv4
				for i := range initial {
					select {
					case initial[i] = <-cleanupInitialRequests:
					case err := <-peerErrors:
						t.Fatalf("synthetic watched deletion raw control failed: %v", err)
					case <-time.After(2 * time.Second):
						t.Fatal("watched deletion raw observer missed real initial DORA")
					}
				}
				if initial[0].MessageType() != dhcpv4.MessageTypeDiscover || initial[1].MessageType() != dhcpv4.MessageTypeRequest ||
					initial[0].TransactionID != initial[1].TransactionID || initial[0].ClientHWAddr.String() != originalMAC ||
					initial[1].ClientHWAddr.String() != originalMAC || !initial[1].RequestedIPAddress().Equal(net.ParseIP(leasedIP)) ||
					!initial[1].ServerIdentifier().Equal(peerAddress.IP) {
					t.Fatal("watched deletion raw DORA lacks original transaction/MAC/server/requested lease")
				}
			}
			updates, subscriptionStop := make(chan netlink.LinkUpdate, 16), make(chan struct{})
			deleted, subscriptionDone := make(chan netlink.LinkUpdate, 1), make(chan struct{})
			must(netlink.LinkSubscribeWithOptions(updates, subscriptionStop, netlink.LinkSubscribeOptions{
				ErrorCallback: func(err error) {
					select {
					case <-subscriptionStop:
						return
					default:
						reportPeerError(fmt.Errorf("synthetic watched deletion link observer: %w", err))
					}
				},
			}))
			t.Cleanup(func() {
				close(subscriptionStop)
				select {
				case <-subscriptionDone:
				case <-time.After(2 * time.Second):
					t.Error("synthetic watched deletion link observer did not stop")
				}
			})
			go func() {
				defer close(subscriptionDone)
				for update := range updates {
					if update.Header.Type == syscall.RTM_DELLINK && update.Attrs().Index == originalIndex {
						select {
						case deleted <- update:
						default:
						}
					}
				}
			}()
			if time.Since(acknowledgedAt) >= leaseLifetime/4 || ctx.Err() != nil || releaseRequests.Load() != 0 || !vipPresent(expectedIP) {
				t.Fatal("watched deletion control lacks valid original VIP/lease/caller before T1")
			}
			if ipv6OnlyWatch {
				t.Log("real IPv6-only Service watcher installed active lease/VIP/status feedback; independent IPv6 raw/peer and original owned native identity verified with no IPv4 client/completion wait before external Deleted event")
			} else {
				t.Log("real Service watcher installed active lease/VIP/status feedback; raw DORA and original owned native identity verified before external Deleted event")
			}
			apiMu.Lock()
			apiServiceDeleted = true
			apiVersion++
			apiService.ResourceVersion = strconv.Itoa(apiVersion)
			select {
			case serviceEvents <- watch.Event{Type: watch.Deleted, Object: apiService.DeepCopy()}:
			default:
				apiMu.Unlock()
				t.Fatal("synthetic Deleted event queue overflow")
			}
			apiMu.Unlock()
			select {
			case <-deletionDelivered:
			case err := <-watcherStopped:
				t.Fatalf("real watcher stopped before Deleted event: %v", err)
			case <-time.After(2 * time.Second):
				t.Fatal("synthetic API did not deliver real Deleted event")
			}
			if !ipv6OnlyWatch {
				select {
				case <-clientDone:
				case <-time.After(2 * time.Second):
					t.Fatal("real watched deletion did not complete DHCP Start")
				}
			}
			select {
			case <-endpointStopped:
			case <-time.After(2 * time.Second):
				t.Fatal("real watched deletion did not cancel its endpoint HTTP watch")
			}
			select {
			case event := <-deleted:
				if event.Attrs().Name != originalName || event.Attrs().HardwareAddr.String() != originalMAC {
					t.Fatal("watched deletion kernel event did not identify original native attachment")
				}
			case err := <-peerErrors:
				t.Fatalf("synthetic watched deletion kernel observer failed: %v", err)
			case <-time.After(2 * time.Second):
				t.Fatal("watched deletion kernel observer missed original-index RTM_DELLINK")
			}
			_, lookupErr := netlink.LinkByIndex(originalIndex)
			var notFound netlink.LinkNotFoundError
			if !errors.As(lookupErr, &notFound) || vipPresent(expectedIP) || len(processor.ServiceSnapshots()) != 0 {
				t.Fatalf("watched deletion did not remove original VIP/attachment/instance: lookup=%v", lookupErr)
			}
			var released *dhcpv4.DHCPv4
			if !ipv6OnlyWatch {
				select {
				case released = <-releaseSeen:
				case err := <-peerErrors:
					t.Fatalf("synthetic watched deletion RELEASE observer failed: %v", err)
				case <-time.After(2 * time.Second):
				}
			}
			select {
			case err := <-watcherStopped:
				t.Fatalf("watched deletion stopped root watcher: %v", err)
			case err := <-peerErrors:
				t.Fatalf("synthetic watched deletion peer failed: %v", err)
			default:
			}
			if ctx.Err() != nil || time.Since(acknowledgedAt) >= leaseLifetime/2 || (!ipv6OnlyWatch && (discoverRequests.Load() != 1 || ackReplies.Load() != 1)) {
				t.Fatal("watched deletion required caller cancellation/expiry or unexpected reacquisition")
			}
			if !ipv6OnlyWatch {
				t.Logf("external Deleted delivered; original-index kernel deletion/absence and actual Start/endpoint completion verified with watcher/caller live before T1; raw RELEASEs=%d", releaseRequests.Load())
			}
			if !ipv6OnlyWatch {
				if released == nil {
					if releaseRequests.Load() != 0 {
						t.Fatal("watched deletion RELEASE count/channel disagreed")
					}
					t.Fatal("real watched Service deletion removed its original DHCP attachment without lease RELEASE; raw DORA/RTM_DELLINK/Start and endpoint completion verified with watcher and caller live")
				}
				if !released.ClientIPAddr.Equal(net.ParseIP(leasedIP)) || released.ClientHWAddr.String() != originalMAC ||
					!released.ServerIdentifier().Equal(peerAddress.IP) || releaseRequests.Load() != 1 {
					t.Fatal("watched deletion RELEASE did not match original lease/MAC/server")
				}
			} else if discoverRequests.Load() != 0 || ackReplies.Load() != 0 || releaseRequests.Load() != 0 || udpReleaseRequests.Load() != 0 || rawPeerPackets.Load() != 0 {
				t.Fatal("IPv6-only watched deletion unexpectedly used IPv4 DHCP wire traffic")
			}
			if v6Watch {
				var released6 *dhcpv6.Message
				limit := time.NewTimer(2 * time.Second)
				defer limit.Stop()
			v6ReleaseWait:
				for {
					select {
					case msg := <-v6.raw:
						if msg.MessageType == dhcpv6.MessageTypeRelease {
							released6 = msg
							break v6ReleaseWait
						}
					case err := <-v6.errors:
						t.Fatalf("dual-stack watched IPv6 wire/peer failed: %v", err)
					case <-limit.C:
						break v6ReleaseWait
					}
				}
				if released6 == nil || !v6.matchesRelease(released6) || v6.releases.Load() != 1 {
					t.Fatal("real dual-stack watched deletion lacks matching original IPv6 wire RELEASE despite calibrated raw/peer/lease/native controls")
				}
				if ipv6OnlyWatch {
					for _, kind := range []dhcpv6.MessageType{dhcpv6.MessageTypeSolicit, dhcpv6.MessageTypeRelease} {
						var received *dhcpv6.Message
						select {
						case received = <-v6.packets:
						case err := <-v6.errors:
							t.Fatalf("IPv6-only final peer correlation failed: %v", err)
						case <-time.After(time.Second):
							t.Fatal("IPv6-only peer did not receive expected terminal SOLICIT/RELEASE")
						}
						if received.MessageType != kind || received.Options.ClientID() == nil || !bytes.Equal(received.Options.ClientID().ToBytes(), v6.clientID.ToBytes()) || received.Options.OneIANA() == nil || received.Options.OneIANA().IaId != v6.iaid {
							t.Fatal("IPv6-only terminal peer type/DUID/IAID correlation failed")
						}
						if kind == dhcpv6.MessageTypeRelease && !bytes.Equal(received.ToBytes(), released6.ToBytes()) {
							t.Fatal("IPv6-only raw/peer RELEASE transaction/lease/server correlation failed")
						}
					}
				}
				completionLimit := time.Now().Add(time.Second)
				for {
					starts4, starts6 := dualStackServiceRunningStarts(t)
					if starts4 == 0 && starts6 == 0 && v6.socketFree(t) {
						break
					}
					if time.Now().After(completionLimit) {
						t.Fatalf("dual-stack actual Start workers/native IPv6 socket remained after Deleted event: starts4=%d starts6=%d", starts4, starts6)
					}
					select {
					case err := <-v6.errors:
						t.Fatalf("dual-stack completion observer peer failed: %v", err)
					case <-ctx.Done():
						t.Fatalf("dual-stack completion required caller cancellation: %v", context.Cause(ctx))
					case <-time.After(10 * time.Millisecond):
					}
				}
				if ctx.Err() != nil || vipPresent(expectedIP) || vipPresent(v6.ip.String()) || v6.fingerprint() != v6.before || !time.Now().Before(acknowledgedAt.Add(4*time.Second)) || !time.Now().Before(v6.ackAt.Add(4*time.Second)) {
					t.Fatal("dual-stack watched cleanup required expiry/caller cancellation or changed original parent/peer native control")
				}
				select {
				case err := <-watcherStopped:
					t.Fatalf("dual-stack root watcher stopped after deletion: %v", err)
				case err := <-v6.errors:
					t.Fatalf("dual-stack final wire/peer control failed: %v", err)
				default:
				}
				if ipv6OnlyWatch {
					t.Log("real external Deleted stopped the IPv6-only DHCP Start worker and released its native UDP6 socket; matching IPv6 wire/peer lease RELEASE, VIP/original owned attachment/tracked instance absent and endpoint watch completed with original parent/peer/root watcher/caller live before T1 and no IPv4 client/completion wait")
				} else {
					t.Log("real external Deleted stopped both dual-stack DHCP Start workers and released native IPv6 socket; matching IPv4+IPv6 lease RELEASEs, both VIPs/original owned attachment/tracked instance absent and endpoint watch completed with original parent/peer/root watcher/caller live before T1")
				}
			}
			t.Log("real Service watcher deletion delivered matching lease RELEASE and removed original VIP/attachment/instance; actual Start and endpoint completed before expiry with watcher and caller live")
			return
		}
		if changeAfterRejection {
			var changedAt time.Time
			select {
			case changedAt = <-changedAckSent:
			case err := <-peerErrors:
				t.Fatalf("synthetic DHCP peer failed before changed-address ACK: %v", err)
			case err := <-watcherStopped:
				t.Fatalf("real ServicesWatcher stopped before changed-address ACK: %v", err)
			case <-ctx.Done():
				t.Fatalf("caller cancelled before changed-address ACK: %v", context.Cause(ctx))
			case <-time.After(leaseLifetime):
				t.Fatal("synthetic peer observed no fresh replacement-address DORA ACK")
			}
			if rejectedOldRequests.Load() < 2 {
				t.Fatal("renewal and old-IP rebind rejection boundaries were not both exercised")
			}
			t.Logf("synthetic peer NAKed old-IP renewal/rebind requests and ACKed fresh requested replacement address; NAKs=%d", rejectedOldRequests.Load())
			select {
			case err := <-peerErrors:
				t.Fatalf("synthetic DHCP peer failed during changed-address reconciliation: %v", err)
			case err := <-watcherStopped:
				t.Fatalf("real ServicesWatcher stopped during changed-address reconciliation: %v", err)
			case <-ctx.Done():
				t.Fatalf("caller cancelled during changed-address reconciliation: %v", context.Cause(ctx))
			case <-time.After(2 * time.Second):
			}
			if !time.Now().Before(changedAt.Add(leaseLifetime)) {
				t.Fatal("replacement-address observation exceeded its new lease lifetime")
			}
			if stallAfterReplacement {
				apiRequestBlocked := false
				select {
				case <-replacementAPIBlocked:
					apiRequestBlocked = true
				default:
				}
				t.Logf("replacement DHCP ACK witnessed while external Service publication unavailable; API request blocked=%t", apiRequestBlocked)
				if vipPresent(expectedIP) {
					t.Fatal("old DHCP VIP remained installed after replacement lease while Service publication was unavailable")
				}
				t.Log("old kernel VIP withdrawn after replacement DHCP lease despite unavailable Service publication; new lease valid and caller live")
				return
			}
			statusChanged := false
			select {
			case <-changedStatusPublished:
				statusChanged = true
			default:
			}
			oldPresent, newPresent := vipPresent(expectedIP), vipPresent(changedIP)
			if !statusChanged || oldPresent || !newPresent {
				t.Fatalf("reacquired DHCP address did not replace real Service VIP through status/watch reconciliation: status=%t old=%t new=%t", statusChanged, oldPresent, newPresent)
			}
			t.Log("real ServicesWatcher reconciled fresh DHCP address into API status and new kernel VIP, old VIP absent, caller live")
			return
		}
		select {
		case err := <-watcherStopped:
			t.Fatalf("real ServicesWatcher stopped after its own status feedback: %v", err)
		case <-ctx.Done():
			t.Fatalf("real Service watch cancelled caller after status feedback: %v", context.Cause(ctx))
		case <-time.After(2 * time.Second):
		}
		if !time.Now().Before(acknowledgedAt.Add(leaseLifetime)) {
			t.Fatal("status feedback observation exceeded original lease lifetime")
		}
		if !vipPresent(expectedIP) {
			t.Fatal("current DHCP lease was withdrawn by its own real Service status/watch feedback before expiry")
		}
		t.Log("real Service status/watch feedback retained current DHCP VIP without caller cancellation or lease expiry")
		return
	}
	t.Log("real Reconcile/SyncServices installed the wire-ACKed VIP; status API is stalled and renewal peer is unavailable")
	if deleteBeforeExpiry {
		deleted := make(chan error, 1)
		go func() {
			deleted <- processor.Delete(watch.Event{Type: watch.Deleted, Object: service}, false)
		}()
		select {
		case err := <-deleted:
			must(err)
		case <-time.After(2 * time.Second):
			t.Fatal("public Service delete blocked on its stalled status API request")
		}
		if vipPresent(expectedIP) || len(processor.ServiceSnapshots()) != 0 {
			t.Fatal("public Service delete retained the real VIP or tracked instance")
		}
		if _, err := netlink.LinkByName("vip-12345678"); err == nil {
			t.Fatal("public Service delete retained its owned DHCP macvlan")
		} else if _, missing := err.(netlink.LinkNotFoundError); !missing {
			t.Fatal(err)
		}
		stopped := make(chan struct{})
		go func() { workers.Wait(); close(stopped) }()
		select {
		case <-stopped:
		case <-time.After(2 * time.Second):
			t.Fatal("public Service delete did not stop all real workers before caller cancellation")
		}
		if ctx.Err() != nil || !time.Now().Before(acknowledgedAt.Add(leaseLifetime)) {
			t.Fatal("public Service delete required caller cancellation or natural lease expiry")
		}
		t.Log("public Service delete cancelled stalled API, removed real VIP/macvlan/instance and stopped workers before lease expiry without caller cancellation")
		return
	}
	select {
	case <-time.After(time.Until(acknowledgedAt.Add(leaseLifetime + time.Second))):
	case err := <-peerErrors:
		t.Fatalf("synthetic DHCP peer failed during Service expiry: %v", err)
	case <-ctx.Done():
		t.Fatalf("Service lease expiry cancelled the caller/other Services: %v", context.Cause(ctx))
	}
	if vipPresent(expectedIP) {
		t.Fatal("expired DHCP Service VIP remained installed while its status API request was stalled")
	}
	t.Log("expired leased VIP withdrawn without an API response or caller cancellation")
}

type dhcpv6ServicePeerConn struct {
	net.PacketConn
	raw  *packet.Conn
	zone string
}

func (c *dhcpv6ServicePeerConn) ReadFrom(buf []byte) (int, net.Addr, error) {
	frame := make([]byte, 65536)
	for {
		n, _, err := c.raw.ReadFrom(frame)
		if err != nil {
			return 0, nil, err
		}
		msg, err := decodeDHCPv6ServiceClientWire(frame[:n])
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

func (c *dhcpv6ServicePeerConn) Close() error {
	return errors.Join(c.raw.Close(), c.PacketConn.Close())
}

func (c *dhcpv6ServicePeerConn) SetDeadline(deadline time.Time) error {
	return errors.Join(c.raw.SetDeadline(deadline), c.PacketConn.SetDeadline(deadline))
}

func (c *dhcpv6ServicePeerConn) SetReadDeadline(deadline time.Time) error {
	return c.raw.SetReadDeadline(deadline)
}

// Only the plain IPv6 UDP client traffic used by this isolated fixture is decoded.
func decodeDHCPv6ServiceClientWire(frame []byte) (*dhcpv6.Message, error) {
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

type dualStackServiceDHCPv6Reply struct {
	request *dhcpv6.Message
	at      time.Time
}

type dualStackServiceDHCPv6Peer struct {
	ip               net.IP
	clientID         *dhcpv6.DUIDEN
	serverID         *dhcpv6.DUIDEN
	raw              chan *dhcpv6.Message
	packets          chan *dhcpv6.Message
	ack              chan dualStackServiceDHCPv6Reply
	errors           chan error
	releases         atomic.Int32
	ackAt            time.Time
	iaid             [4]byte
	parentIndex      int
	fingerprint      func() string
	before           string
	terminalHold     atomic.Bool
	terminalSolicits atomic.Int32
	terminalEvents   chan dualStackServiceDHCPv6Reply
}

func newDualStackServiceDHCPv6Peer(t *testing.T, parent, peer netlink.Link) *dualStackServiceDHCPv6Peer {
	return newDualStackServiceDHCPv6PeerWithLifetime(t, parent, peer, 8*time.Second)
}

func newDualStackServiceDHCPv6PeerWithLifetime(t *testing.T, parent, peer netlink.Link, lifetime time.Duration) *dualStackServiceDHCPv6Peer {
	t.Helper()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	p := &dualStackServiceDHCPv6Peer{
		ip:       net.ParseIP("2001:db8:1::30"),
		clientID: &dhcpv6.DUIDEN{EnterpriseNumber: 1, EnterpriseIdentifier: []byte("public-dual-proof")},
		serverID: &dhcpv6.DUIDEN{EnterpriseNumber: 32473, EnterpriseIdentifier: []byte("synthetic-dual-peer")},
		raw:      make(chan *dhcpv6.Message, 16), packets: make(chan *dhcpv6.Message, 16),
		ack: make(chan dualStackServiceDHCPv6Reply, 1), errors: make(chan error, 8), parentIndex: parent.Attrs().Index,
		terminalEvents: make(chan dualStackServiceDHCPv6Reply, 2),
	}
	for i, link := range []netlink.Link{parent, peer} {
		addr, err := netlink.ParseAddr([]string{"fe80::2/64", "fe80::1/64"}[i])
		must(err)
		addr.Flags = syscall.IFA_F_NODAD
		must(netlink.AddrAdd(link, addr))
		addresses, err := netlink.AddrList(link, netlink.FAMILY_V6)
		must(err)
		ready := false
		for _, a := range addresses {
			if a.IP.Equal(addr.IP) && a.Flags&syscall.IFA_F_NODAD != 0 && a.Flags&(syscall.IFA_F_TENTATIVE|syscall.IFA_F_DADFAILED) == 0 {
				ready = true
			}
		}
		if !ready {
			t.Fatal("dual-stack peer lacks ready native manual link-local")
		}
	}
	selected, err := dhcpv6.GetLinkLocalAddr(parent.Attrs().Name)
	must(err)
	if !selected.Equal(net.ParseIP("fe80::2")) {
		t.Fatalf("dual-stack client parent selected wrong native link-local: %s", selected)
	}
	p.fingerprint = func() string {
		var items []string
		for _, expected := range []netlink.Link{parent, peer} {
			link, err := netlink.LinkByIndex(expected.Attrs().Index)
			if err != nil {
				return err.Error()
			}
			items = append(items, fmt.Sprintf("%s/%d/%s/%d/%d", link.Attrs().Name, link.Attrs().Index, link.Attrs().HardwareAddr, link.Attrs().MTU, link.Attrs().Flags))
			addresses, err := netlink.AddrList(link, netlink.FAMILY_V6)
			if err != nil {
				return err.Error()
			}
			for _, a := range addresses {
				items = append(items, a.IPNet.String())
			}
		}
		return strings.Join(items, "|")
	}
	p.before = p.fingerprint()
	report := func(err error) {
		select {
		case p.errors <- err:
		default:
		}
	}
	peerIface, err := net.InterfaceByName(peer.Attrs().Name)
	must(err)
	observer, err := packet.Listen(peerIface, packet.Datagram, syscall.ETH_P_IPV6, nil)
	must(err)
	stopped, rawDone, peerDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		defer close(rawDone)
		buf := make([]byte, 65536)
		for {
			n, _, err := observer.ReadFrom(buf)
			if err != nil {
				select {
				case <-stopped:
					return
				default:
					report(err)
					return
				}
			}
			msg, err := decodeDHCPv6ServiceClientWire(buf[:n])
			if err != nil {
				report(err)
				continue
			}
			if msg == nil {
				continue
			}
			if msg.MessageType == dhcpv6.MessageTypeRelease {
				p.releases.Add(1)
			}
			select {
			case p.raw <- msg:
			default:
				report(fmt.Errorf("dual-stack IPv6 raw queue overflow"))
			}
		}
	}()
	udp, err := server6.NewIPv6UDPConn(peerIface.Name, &net.UDPAddr{IP: net.IPv6unspecified, Port: 547})
	must(err)
	peerRaw, err := packet.Listen(peerIface, packet.Datagram, syscall.ETH_P_IPV6, nil)
	if err != nil {
		_ = udp.Close()
		must(err)
	}
	conn := &dhcpv6ServicePeerConn{PacketConn: udp, raw: peerRaw, zone: peerIface.Name}
	lease := dhcpv6.OptIAAddress{IPv6Addr: p.ip, PreferredLifetime: lifetime, ValidLifetime: lifetime}
	server, err := server6.NewServer(peerIface.Name, nil, func(out net.PacketConn, remote net.Addr, message dhcpv6.DHCPv6) {
		msg, ok := message.(*dhcpv6.Message)
		if !ok || msg.Options.ClientID() == nil || !bytes.Equal(msg.Options.ClientID().ToBytes(), p.clientID.ToBytes()) || msg.Options.OneIANA() == nil {
			report(fmt.Errorf("dual-stack IPv6 peer wrong DUID/IANA"))
			return
		}
		select {
		case p.packets <- msg:
		default:
			report(fmt.Errorf("dual-stack IPv6 peer queue overflow"))
			return
		}
		mods := []dhcpv6.Modifier{dhcpv6.WithServerID(p.serverID), dhcpv6.WithIAID(msg.Options.OneIANA().IaId)}
		var reply *dhcpv6.Message
		var replyErr error
		switch msg.MessageType {
		case dhcpv6.MessageTypeSolicit:
			if p.terminalHold.Load() {
				if count := p.terminalSolicits.Add(1); count <= 2 {
					p.terminalEvents <- dualStackServiceDHCPv6Reply{request: msg, at: time.Now()}
					if count == 1 {
						return // External peer withholds only the first terminal ADVERTISE.
					}
				} else {
					report(fmt.Errorf("unexpected third terminal SOLICIT before T1"))
					return
				}
			}
			reply, replyErr = dhcpv6.NewAdvertiseFromSolicit(msg, append(mods, dhcpv6.WithIANA(lease))...)
		case dhcpv6.MessageTypeRequest, dhcpv6.MessageTypeRenew, dhcpv6.MessageTypeRelease:
			if msg.Options.ServerID() == nil || !bytes.Equal(msg.Options.ServerID().ToBytes(), p.serverID.ToBytes()) {
				report(fmt.Errorf("dual-stack IPv6 selected server mismatch"))
				return
			}
			if msg.MessageType == dhcpv6.MessageTypeRenew || msg.MessageType == dhcpv6.MessageTypeRelease {
				addr := msg.Options.OneIANA().Options.OneAddress()
				if addr == nil || !addr.IPv6Addr.Equal(p.ip) {
					report(fmt.Errorf("dual-stack IPv6 renewal/release lease mismatch"))
					return
				}
			}
			if msg.MessageType == dhcpv6.MessageTypeRelease {
				mods = append(mods, dhcpv6.WithOption(&dhcpv6.OptStatusCode{StatusCode: iana.StatusSuccess}))
			} else {
				mods = append(mods, dhcpv6.WithIANA(lease))
			}
			reply, replyErr = dhcpv6.NewReplyFromMessage(msg, mods...)
		default:
			report(fmt.Errorf("dual-stack IPv6 unsupported peer message %s", msg.MessageType))
			return
		}
		if replyErr != nil {
			report(replyErr)
			return
		}
		if _, err := out.WriteTo(reply.ToBytes(), remote); err != nil {
			select {
			case <-stopped:
				return
			default:
				report(err)
				return
			}
		}
		if msg.MessageType == dhcpv6.MessageTypeRequest {
			select {
			case p.ack <- dualStackServiceDHCPv6Reply{request: msg, at: time.Now()}:
			default:
			}
		}
	}, server6.WithConn(conn))
	if err != nil {
		_ = conn.Close()
		must(err)
	}
	go func() { peerDone <- server.Serve() }()
	t.Cleanup(func() {
		close(stopped)
		_ = server.Close()
		_ = observer.Close()
		select {
		case <-rawDone:
		case <-time.After(2 * time.Second):
			t.Error("dual-stack IPv6 raw observer did not stop")
		}
		select {
		case <-peerDone:
		case <-time.After(2 * time.Second):
			t.Error("dual-stack IPv6 synthetic peer did not stop")
		}
	})
	return p
}

func (p *dualStackServiceDHCPv6Peer) checkInitial(t *testing.T) {
	t.Helper()
	var reply dualStackServiceDHCPv6Reply
	select {
	case reply = <-p.ack:
	case err := <-p.errors:
		t.Fatalf("dual-stack IPv6 initial peer failed: %v", err)
	case <-time.After(time.Second):
		t.Fatal("dual-stack IPv6 peer emitted no real initial REPLY")
	}
	request := reply.request
	p.ackAt = reply.at
	for _, kind := range []dhcpv6.MessageType{dhcpv6.MessageTypeSolicit, dhcpv6.MessageTypeRequest} {
		var wire, actualPeer *dhcpv6.Message
		select {
		case wire = <-p.raw:
		case err := <-p.errors:
			t.Fatalf("dual-stack IPv6 raw control failed: %v", err)
		case <-time.After(time.Second):
			t.Fatal("dual-stack IPv6 raw initial control missing")
		}
		select {
		case actualPeer = <-p.packets:
		case err := <-p.errors:
			t.Fatalf("dual-stack IPv6 peer control failed: %v", err)
		case <-time.After(time.Second):
			t.Fatal("dual-stack IPv6 peer initial control missing")
		}
		if wire.MessageType != kind || actualPeer.MessageType != kind || !bytes.Equal(wire.ToBytes(), actualPeer.ToBytes()) || wire.Options.OneIANA() == nil || wire.Options.ClientID() == nil || !bytes.Equal(wire.Options.ClientID().ToBytes(), p.clientID.ToBytes()) {
			t.Fatal("dual-stack IPv6 initial independent raw/peer/DUID/transaction/IANA controls disagree")
		}
		if kind == dhcpv6.MessageTypeRequest {
			if !bytes.Equal(actualPeer.ToBytes(), request.ToBytes()) {
				t.Fatal("dual-stack IPv6 real REPLY request transaction mismatch")
			}
			p.iaid = actualPeer.Options.OneIANA().IaId
		}
	}
	if p.ackAt.IsZero() || p.fingerprint() != p.before || p.releases.Load() != 0 {
		t.Fatal("dual-stack IPv6 initial native/lease/REPLY control invalid")
	}
}

func (p *dualStackServiceDHCPv6Peer) socketFree(t *testing.T) bool {
	t.Helper()
	fd, err := syscall.Socket(syscall.AF_INET6, syscall.SOCK_DGRAM|syscall.SOCK_CLOEXEC, syscall.IPPROTO_UDP)
	if err != nil {
		t.Fatal(err)
	}
	addr := &syscall.SockaddrInet6{Port: 546, ZoneId: uint32(p.parentIndex)}
	copy(addr.Addr[:], net.ParseIP("fe80::2").To16())
	err = syscall.Bind(fd, addr)
	closeErr := syscall.Close(fd)
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	if err == nil {
		return true
	}
	if errors.Is(err, syscall.EADDRINUSE) {
		return false
	}
	t.Fatalf("dual-stack native IPv6 port control failed: %v", err)
	return false
}

func (p *dualStackServiceDHCPv6Peer) matchesRelease(msg *dhcpv6.Message) bool {
	if msg.MessageType != dhcpv6.MessageTypeRelease || msg.Options.ClientID() == nil || !bytes.Equal(msg.Options.ClientID().ToBytes(), p.clientID.ToBytes()) || msg.Options.ServerID() == nil || !bytes.Equal(msg.Options.ServerID().ToBytes(), p.serverID.ToBytes()) {
		return false
	}
	addr := msg.Options.OneIANA()
	return addr != nil && addr.IaId == p.iaid && addr.Options.OneAddress() != nil && addr.Options.OneAddress().IPv6Addr.Equal(p.ip)
}

// Public runtime readback observes real Start frames, never a fixture completion channel.
func dualStackServiceRunningStarts(t *testing.T) (int, int) {
	t.Helper()
	var data bytes.Buffer
	if err := pprof.Lookup("goroutine").WriteTo(&data, 2); err != nil {
		t.Fatal(err)
	}
	return strings.Count(data.String(), "vip.(*DHCPv4Client).Start("), strings.Count(data.String(), "vip.(*DHCPv6Client).Start(")
}

func TestDHCPv6InstanceStopReleasesLeaseBeforeOwnedVLANRemoval(t *testing.T) {
	checkDHCPv6OwnedVLANRetirement(t, true)
}

func TestDHCPv6InstanceCleanupReleasesLeaseBeforeOwnedVLANRemoval(t *testing.T) {
	checkDHCPv6OwnedVLANRetirement(t, false)
}

func TestDHCPv6InstanceDelayedStopReleasesLeaseBeforeOwnedVLANRemoval(t *testing.T) {
	checkDHCPv6OwnedVLANRetirementCase(t, true, true)
}

func TestDHCPv6InstanceDelayedCleanupWaitsBeforeOwnedVLANRemoval(t *testing.T) {
	checkDHCPv6OwnedVLANRetirementCase(t, false, true)
}

func checkDHCPv6OwnedVLANRetirement(t *testing.T, stopFirst bool) {
	checkDHCPv6OwnedVLANRetirementCase(t, stopFirst, false)
}

func checkDHCPv6OwnedVLANRetirementCase(t *testing.T, stopFirst, delayed bool) {
	t.Helper()
	if os.Getenv("KUBE_VIP_ISOLATED_DHCP_TEST") != "1" {
		t.Skip("requires a dedicated network-none Linux test process")
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
		t.Fatal("owned-VLAN DHCPv6 test requires an initially loopback-only namespace")
	}
	must(netlink.LinkAdd(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: "kv6-vlan-c"}, PeerName: "kv6-vlan-s"}))
	base, err := netlink.LinkByName("kv6-vlan-c")
	must(err)
	t.Cleanup(func() { _ = netlink.LinkDel(base) })
	basePeer, err := netlink.LinkByName("kv6-vlan-s")
	must(err)
	must(netlink.LinkSetUp(base))
	must(netlink.LinkSetUp(basePeer))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	config := &kubevip.Config{
		Interface: base.Attrs().Name, VIPSubnet: "32,128", DHCPMode: "ipv6", DNSMode: "ipv6",
		DHCPBackoffAttempts: 2, NodeName: "offline-node",
	}
	service := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "owned-vlan-static", Namespace: "default", UID: "87654321-1234-1234-1234-123456789abc",
			Annotations: map[string]string{
				kubevip.LoadbalancerIPAnnotation: "2001:db8:1::90", kubevip.ServiceVlan: "kv6-vlan-c.123",
			},
		},
		Spec: v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer, IPFamilies: []v1.IPFamily{v1.IPv6Protocol}},
	}
	var staticWorkers sync.WaitGroup
	owner, err := instance.NewInstance(ctx, service, config, networkinterface.NewManager(), nil, nil, noop.NewManager(), &staticWorkers)
	must(err)
	ownerRetired := false
	t.Cleanup(func() {
		if !ownerRetired {
			if err := owner.CleanupLinkAttachments(); err != nil {
				t.Errorf("owned-VLAN static owner teardown: %v", err)
			}
		}
	})
	parent, err := netlink.LinkByName("kv6-vlan-c.123")
	must(err)
	vlan, ok := parent.(*netlink.Vlan)
	if !ok || vlan.VlanId != 123 || vlan.VlanProtocol != netlink.VLAN_PROTOCOL_8021Q || parent.Attrs().ParentIndex != base.Attrs().Index || parent.Attrs().Flags&net.FlagUp == 0 || !owner.IsVLAN || owner.VLANInterface != parent.Attrs().Name {
		t.Fatal("real static constructor did not create the expected native owned VLAN")
	}
	must(netlink.LinkAdd(&netlink.Vlan{
		LinkAttrs: netlink.LinkAttrs{Name: "kv6-vlan-s.123", ParentIndex: basePeer.Attrs().Index},
		VlanId:    123, VlanProtocol: netlink.VLAN_PROTOCOL_8021Q,
	}))
	peer, err := netlink.LinkByName("kv6-vlan-s.123")
	must(err)
	must(netlink.LinkSetUp(peer))
	lifetime := 8 * time.Second
	drainLimit := 2 * time.Second
	if delayed {
		lifetime, drainLimit = 16*time.Second, 7*time.Second
	}
	p := newDualStackServiceDHCPv6PeerWithLifetime(t, parent, peer, lifetime)
	stableNative := func() string {
		var values []string
		for _, expected := range []netlink.Link{base, basePeer, peer} {
			link, err := netlink.LinkByIndex(expected.Attrs().Index)
			if err != nil {
				return err.Error()
			}
			values = append(values, fmt.Sprintf("%s/%s/%d/%d/%s/%d/%d", link.Type(), link.Attrs().Name, link.Attrs().Index, link.Attrs().ParentIndex, link.Attrs().HardwareAddr, link.Attrs().MTU, link.Attrs().Flags))
		}
		return strings.Join(values, "|")
	}
	before := stableNative()
	service = service.DeepCopy()
	service.Name, service.UID = "owned-vlan-dhcp", "12345678-1234-1234-1234-123456789abc"
	service.Annotations[kubevip.LoadbalancerIPAnnotation] = "::"
	service.Annotations[kubevip.LoadbalancerHostname] = "public-dual-proof"
	type constructorResult struct {
		inst *instance.Instance
		err  error
	}
	constructed := make(chan constructorResult, 1)
	var workers sync.WaitGroup
	var client *instance.Instance
	received, cleanupInvoked := false, false
	t.Cleanup(func() {
		cancel()
		if !received {
			select {
			case result := <-constructed:
				client = result.inst
			case <-time.After(2 * time.Second):
				t.Error("owned-VLAN real constructor ignored teardown cancellation")
				return
			}
		}
		if client != nil && !cleanupInvoked {
			if err := client.CleanupLinkAttachments(); err != nil {
				t.Errorf("owned-VLAN DHCP instance teardown: %v", err)
			}
		}
		drained := make(chan struct{})
		go func() { workers.Wait(); close(drained) }()
		select {
		case <-drained:
		case <-time.After(drainLimit):
			t.Error("owned-VLAN actual DHCP Start worker ignored teardown cancellation")
		}
	})
	go func() {
		built, err := instance.NewInstance(ctx, service, config, networkinterface.NewManager(), nil, nil, noop.NewManager(), &workers)
		constructed <- constructorResult{inst: built, err: err}
	}()
	select {
	case result := <-constructed:
		received, client = true, result.inst
		must(result.err)
	case err := <-p.errors:
		t.Fatalf("owned-VLAN acquisition peer failed: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("real owned-VLAN DHCP constructor did not acquire its lease")
	}
	p.checkInitial(t)
	v4, v6 := dualStackServiceRunningStarts(t)
	if client == nil || !client.IsVLAN || client.VLANInterface != parent.Attrs().Name || !client.IsDHCPv6 || client.IsDHCPv4 || client.DHCPv6Client == nil || client.DHCPv4Done() != nil || client.DHCPInterfaceIPv6 != p.ip.String() || len(client.VIPConfigs) != 1 || client.VIPConfigs[0].VIP != p.ip.String() || v4 != 0 || v6 != 1 || p.socketFree(t) || ctx.Err() != nil || time.Since(p.ackAt) >= 3*time.Second {
		t.Fatal("owned-VLAN actual constructor/lease/Start/native socket/caller/pre-T1 control invalid")
	}
	dhcpLink, err := netlink.LinkByName(client.DHCPInterface)
	must(err)
	mac, ok := dhcpLink.(*netlink.Macvlan)
	if !ok || mac.Attrs().ParentIndex != parent.Attrs().Index || mac.Attrs().HardwareAddr.String() != client.DHCPInterfaceHwaddr || mac.Attrs().Alias != "kube-vip:dhcp:v1:"+string(service.UID)+":"+client.DHCPInterfaceHwaddr || mac.Attrs().Flags&net.FlagUp == 0 {
		t.Fatal("owned-VLAN real DHCP macvlan lacks expected native type/parent/MAC/full UID owner")
	}
	must(owner.CleanupLinkAttachments(client))
	ownerRetired = true
	if p.fingerprint() != p.before || stableNative() != before || ctx.Err() != nil {
		t.Fatal("public static-to-DHCP VLAN ownership handoff changed its live native transport")
	}
	workerDone := make(chan struct{})
	go func() { workers.Wait(); close(workerDone) }()
	select {
	case <-workerDone:
		t.Fatal("actual owned-VLAN DHCP Start worker ended before retirement")
	default:
	}
	updates, subscribeDone := make(chan netlink.LinkUpdate, 16), make(chan struct{})
	must(netlink.LinkSubscribe(updates, subscribeDone))
	t.Cleanup(func() { close(subscribeDone) })
	if delayed {
		deadline := time.NewTimer(7 * time.Second)
		defer deadline.Stop()
		readPair := func(kind dhcpv6.MessageType) *dhcpv6.Message {
			t.Helper()
			var wire, actual *dhcpv6.Message
			select {
			case wire = <-p.raw:
			case err := <-p.errors:
				t.Fatalf("delayed VLAN raw/peer fixture failed: %v", err)
			case <-deadline.C:
				t.Fatal("delayed VLAN raw frame missing before 16s lease T1")
			}
			select {
			case actual = <-p.packets:
			case err := <-p.errors:
				t.Fatalf("delayed VLAN peer fixture failed: %v", err)
			case <-deadline.C:
				t.Fatal("delayed VLAN peer receive missing before T1")
			}
			if wire.MessageType != kind || actual.MessageType != kind || !bytes.Equal(wire.ToBytes(), actual.ToBytes()) || wire.Options.ClientID() == nil || !bytes.Equal(wire.Options.ClientID().ToBytes(), p.clientID.ToBytes()) || wire.Options.OneIANA() == nil || wire.Options.OneIANA().IaId != p.iaid {
				t.Fatal("delayed VLAN independent raw/peer kind/DUID/IAID/transaction disagree")
			}
			return wire
		}
		p.terminalHold.Store(true)
		stopped := make(chan struct{})
		go func() { client.DHCPv6Client.Stop(); close(stopped) }()
		select {
		case <-stopped:
		case <-time.After(time.Second):
			t.Fatal("delayed VLAN public Stop failed to return")
		}
		var first dualStackServiceDHCPv6Reply
		select {
		case first = <-p.terminalEvents:
		case err := <-p.errors:
			t.Fatalf("delayed VLAN initial terminal peer failed: %v", err)
		case <-time.After(time.Second):
			t.Fatal("delayed VLAN peer received no terminal SOLICIT to hold")
		}
		initial := readPair(dhcpv6.MessageTypeSolicit)
		v4, v6 := dualStackServiceRunningStarts(t)
		if !bytes.Equal(initial.ToBytes(), first.request.ToBytes()) || p.fingerprint() != p.before || stableNative() != before || p.socketFree(t) || ctx.Err() != nil || v4 != 0 || v6 != 1 || time.Since(first.at) >= time.Second {
			t.Fatal("delayed VLAN held terminal SOLICIT/native/actual Start/socket/caller control invalid")
		}
		t.Log("16s real lease/public constructors/native ownership handoff verified; public Stop returned; first terminal SOLICIT independently received by raw observer and external peer and its ADVERTISE held; actual Start/native546/original VLAN/base/peer/caller still live")
		readNaturalRelease := func() {
			t.Helper()
			var retry dualStackServiceDHCPv6Reply
			select {
			case retry = <-p.terminalEvents:
			case err := <-p.errors:
				t.Fatalf("delayed VLAN natural retry peer failed: %v", err)
			case <-deadline.C:
				t.Fatal("delayed VLAN unchanged natural library retry not received before T1")
			}
			elapsed := retry.at.Sub(first.at)
			if elapsed < 4800*time.Millisecond || elapsed >= 7*time.Second || !bytes.Equal(retry.request.ToBytes(), first.request.ToBytes()) {
				t.Fatalf("delayed VLAN actual same-peer natural 5s retry/transaction control invalid: %s", elapsed)
			}
			if !bytes.Equal(readPair(dhcpv6.MessageTypeSolicit).ToBytes(), retry.request.ToBytes()) || !p.matchesRelease(readPair(dhcpv6.MessageTypeRelease)) || p.releases.Load() != 1 {
				t.Fatal("delayed VLAN natural retry/original-lease RELEASE wire control invalid")
			}
			t.Logf("external peer observed actual unchanged natural retry after %s; matching raw/peer retry and original lease DUID/server/IAID RELEASE received", elapsed)
		}
		if stopFirst {
			select {
			case <-workerDone:
			case err := <-p.errors:
				t.Fatalf("delayed VLAN normal Stop fixture failed: %v", err)
			case <-deadline.C:
				t.Fatal("delayed VLAN normal Stop actual Start did not drain before T1")
			}
			readNaturalRelease()
			if p.fingerprint() != p.before || !p.socketFree(t) || stableNative() != before || ctx.Err() != nil {
				t.Fatal("delayed VLAN normal Stop retained-parent/actual worker/socket/caller control invalid")
			}
			t.Log("delayed normal Stop positive control completed actual Start/one matching RELEASE/native port free with original transport parent and caller live")
		}
		cleanupInvoked = true
		finished := make(chan error, 1)
		go func() { finished <- client.CleanupLinkAttachments() }()
		select {
		case err := <-finished:
			must(err)
		case err := <-p.errors:
			t.Fatalf("delayed VLAN cleanup fixture failed: %v", err)
		case <-deadline.C:
			t.Fatal("delayed VLAN actual Cleanup did not return before T1")
		}
		deleted := make(map[int]bool)
		for !deleted[parent.Attrs().Index] || !deleted[dhcpLink.Attrs().Index] {
			select {
			case update, ok := <-updates:
				if !ok {
					t.Fatal("delayed VLAN native deletion observer closed")
				}
				if update.Header.Type == syscall.RTM_DELLINK && update.Link != nil {
					deleted[update.Attrs().Index] = true
				}
			case <-deadline.C:
				t.Fatal("delayed VLAN native deletion events missing")
			}
		}
		for _, index := range []int{parent.Attrs().Index, dhcpLink.Attrs().Index} {
			_, err := netlink.LinkByIndex(index)
			var absent netlink.LinkNotFoundError
			if !errors.As(err, &absent) {
				t.Fatalf("delayed VLAN captured index not typed-absent: %d %v", index, err)
			}
		}
		retiredWhileRunning := false
		select {
		case <-workerDone:
		default:
			v4, v6 := dualStackServiceRunningStarts(t)
			if v4 != 0 || v6 != 1 {
				t.Fatal("delayed VLAN worker-open/actual Start calibration disagrees")
			}
			retiredWhileRunning = true
		}
		select {
		case <-workerDone:
		case err := <-p.errors:
			t.Fatalf("delayed VLAN post-retirement fixture failed: %v", err)
		case <-deadline.C:
			t.Fatal("delayed VLAN actual Start failed to drain naturally before T1")
		}
		v4, v6 = dualStackServiceRunningStarts(t)
		if v4 != 0 || v6 != 0 || stableNative() != before || ctx.Err() != nil || time.Since(p.ackAt) >= 7*time.Second {
			t.Fatal("delayed VLAN final actual worker/native base-peer/caller/pre-T1 control invalid")
		}
		select {
		case err := <-p.errors:
			t.Fatalf("delayed VLAN final peer/observer fault: %v", err)
		default:
		}
		t.Logf("actual Cleanup/native DELLINK/typed-index absence observed; actual constructor Start worker naturally drained with base/peer/caller live before T1; retired-while-Start-active=%t raw-RELEASEs=%d", retiredWhileRunning, p.releases.Load())
		if retiredWhileRunning {
			t.Fatal("real owned-VLAN Cleanup retired its captured native transport parent while actual DHCPv6 Start was still completing held terminal release; independently received original terminal SOLICIT, real Start calibration and later natural worker drain/base-peer/caller controls verified")
		}
		if !stopFirst {
			readNaturalRelease()
		}
		if p.releases.Load() != 1 {
			t.Fatal("delayed VLAN retirement lacks exactly one matching lease RELEASE")
		}
		return
	}
	t.Log("real public constructors and ownership handoff established an 8s DHCPv6 lease on the actual owned native VLAN transport parent; independent raw/peer/DUID/server/IAID/transaction, full UID/MAC macvlan, actual single Start/native socket and base/peer/caller controls live before T1")
	deadline := time.NewTimer(2500 * time.Millisecond)
	defer deadline.Stop()
	var release *dhcpv6.Message
	readRelease := func(limit <-chan time.Time) bool {
		t.Helper()
		for _, kind := range []dhcpv6.MessageType{dhcpv6.MessageTypeSolicit, dhcpv6.MessageTypeRelease} {
			var wire, actualPeer *dhcpv6.Message
			select {
			case wire = <-p.raw:
			case err := <-p.errors:
				t.Fatalf("owned-VLAN release observer/peer failed: %v", err)
			case <-limit:
				return false
			}
			select {
			case actualPeer = <-p.packets:
			case err := <-p.errors:
				t.Fatalf("owned-VLAN release peer failed: %v", err)
			case <-limit:
				t.Fatal("owned-VLAN release peer receive control missing")
			}
			if wire.MessageType != kind || actualPeer.MessageType != kind || !bytes.Equal(wire.ToBytes(), actualPeer.ToBytes()) || wire.Options.ClientID() == nil || !bytes.Equal(wire.Options.ClientID().ToBytes(), p.clientID.ToBytes()) || wire.Options.OneIANA() == nil || wire.Options.OneIANA().IaId != p.iaid {
				t.Fatalf("owned-VLAN independent raw/peer %s/DUID/IAID/transaction controls disagree", kind)
			}
			if kind == dhcpv6.MessageTypeRelease {
				release = wire
				if !p.matchesRelease(release) {
					t.Fatal("owned-VLAN raw RELEASE does not match the original lease/DUID/server/IAID")
				}
			}
		}
		return true
	}
	if stopFirst {
		stopped := make(chan struct{})
		go func() { client.DHCPv6Client.Stop(); close(stopped) }()
		select {
		case <-stopped:
		case <-deadline.C:
			t.Fatal("owned-VLAN normal Stop failed to return before T1")
		}
		select {
		case <-workerDone:
		case err := <-p.errors:
			t.Fatalf("owned-VLAN normal Stop peer failed: %v", err)
		case <-deadline.C:
			t.Fatal("owned-VLAN normal Stop did not complete actual Start before T1")
		}
		if !readRelease(deadline.C) {
			t.Fatal("owned-VLAN release control did not arrive before T1")
		}
		if p.releases.Load() != 1 || p.fingerprint() != p.before || !p.socketFree(t) || ctx.Err() != nil {
			t.Fatal("owned-VLAN normal Stop/RELEASE/actual Start/native socket control invalid")
		}
		t.Log("real owned-VLAN normal Stop completed actual DHCPv6 Start, freed its native UDP6 socket and delivered one matching raw lease RELEASE with original VLAN/base/peer/caller live")
	}
	cleanupDone := make(chan error, 1)
	cleanupInvoked = true
	go func() { cleanupDone <- client.CleanupLinkAttachments() }()
	select {
	case err := <-cleanupDone:
		must(err)
	case <-deadline.C:
		t.Fatal("owned-VLAN real Cleanup failed to return before T1")
	}
	select {
	case <-workerDone:
	case err := <-p.errors:
		t.Fatalf("owned-VLAN cleanup peer failed: %v", err)
	case <-deadline.C:
		t.Fatal("owned-VLAN cleanup did not complete actual DHCPv6 Start before T1")
	}
	deleted := make(map[int]bool)
	for !deleted[parent.Attrs().Index] || !deleted[dhcpLink.Attrs().Index] {
		select {
		case update, ok := <-updates:
			if !ok {
				t.Fatal("owned-VLAN native deletion observer closed early")
			}
			if update.Header.Type == syscall.RTM_DELLINK && update.Link != nil {
				deleted[update.Attrs().Index] = true
			}
		case <-deadline.C:
			t.Fatal("owned-VLAN native deletion events missing before T1")
		}
	}
	for _, index := range []int{parent.Attrs().Index, dhcpLink.Attrs().Index} {
		_, err := netlink.LinkByIndex(index)
		var absent netlink.LinkNotFoundError
		if !errors.As(err, &absent) {
			t.Fatalf("owned-VLAN captured index not typed-absent after cleanup: %d %v", index, err)
		}
	}
	v4, v6 = dualStackServiceRunningStarts(t)
	if v4 != 0 || v6 != 0 || stableNative() != before || ctx.Err() != nil || time.Since(p.ackAt) >= 3*time.Second {
		t.Fatal("owned-VLAN cleanup actual Start/base/peer/caller/pre-T1 controls invalid")
	}
	select {
	case err := <-p.errors:
		t.Fatalf("owned-VLAN final peer/observer failed: %v", err)
	default:
	}
	t.Logf("real owned-VLAN Cleanup returned, actual DHCPv6 Start completed and captured VLAN/macvlan native deletions/typed absence verified with original base/peer/caller live before T1; raw RELEASEs=%d", p.releases.Load())
	if !stopFirst && !readRelease(time.After(250*time.Millisecond)) {
		t.Fatal("real owned-VLAN cleanup completed actual DHCPv6 Start and deleted its native transport parent without matching raw RELEASE; native initial wire/lease/ownership-handoff/base-peer/caller controls verified")
	}
	if ctx.Err() != nil || time.Since(p.ackAt) >= 3*time.Second || !p.matchesRelease(release) || p.releases.Load() != 1 {
		t.Fatal("owned-VLAN cleanup did not preserve exactly one matching original lease raw RELEASE")
	}
	t.Log("real owned-VLAN public retirement delivered one matching active-lease RELEASE and completed actual Start while removing only its captured VLAN/macvlan with original base/peer/caller live before T1")
}
