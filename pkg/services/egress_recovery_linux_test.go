//go:build linux

package services

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	kernelnft "github.com/google/nftables"
	"github.com/google/nftables/expr"
	"github.com/kube-vip/kube-vip/pkg/election"
	"github.com/kube-vip/kube-vip/pkg/endpoints"
	"github.com/kube-vip/kube-vip/pkg/endpoints/providers"
	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/networkinterface"
	"github.com/kube-vip/kube-vip/pkg/nftables"
	"github.com/kube-vip/kube-vip/pkg/route"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	"github.com/kube-vip/kube-vip/pkg/services/serviceelection"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
)

// Exercise real client-go lease elections, Service activation/cleanup, routes
// and nftables in separate node namespaces. Only the API server and node labels
// are substituted; this does not exercise a CNI or send application traffic.
func TestRecoverEgressAfterEndpointLoss(t *testing.T) {
	if runIsolatedEgressTest(t) {
		return
	}
	for _, test := range []struct {
		name            string
		nextLeader      string
		serviceElection bool
	}{
		{name: "retained instance", nextLeader: "node-a"},
		{name: "same leader", nextLeader: "node-a", serviceElection: true},
		{name: "different leader", nextLeader: "node-b", serviceElection: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name: "egress", Namespace: "default", UID: "egress-recovery",
					Annotations: map[string]string{kubevip.Egress: "true", kubevip.ActiveEndpoint: "10.0.0.1"},
				},
				Spec: v1.ServiceSpec{
					Type: v1.ServiceTypeLoadBalancer, LoadBalancerIP: "192.0.2.10",
					ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyLocal,
				},
			}
			client := newEgressRecoveryClient(t, service)
			first := newEgressRecoveryNode(t, "node-a", client, service, test.serviceElection)
			second := newEgressRecoveryNode(t, "node-b", client, service, test.serviceElection)
			first.reconcile(t, service, true)
			if test.serviceElection {
				first.start(t, service)
			} else {
				if err := inEgressRecoveryNamespace(first.namespace, func() error {
					return first.processor.SyncServices(first.ctx, service, &first.wg)
				}); err != nil {
					t.Fatal(err)
				}
				first.reconcile(t, service, true)
			}
			first.waitForDatapath(t, service, true)
			originalInstance := first.processor.findServiceInstance(service)
			if test.serviceElection {
				assertRecoveryLeaseHolder(t, client, service, "node-a")
			}

			first.reconcile(t, service, false)
			first.waitForDatapath(t, service, false)
			current, err := client.CoreV1().Services(service.Namespace).Get(context.Background(), service.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if got := current.Annotations[kubevip.ActiveEndpoint]; got != "" {
				t.Errorf("zero endpoints left a stale active endpoint %q", got)
			}

			next := first
			if test.nextLeader == "node-b" {
				next = second
			}
			next.reconcile(t, current, true)
			if next == second {
				// Model the Service watch delivering the persisted endpoint before
				// the new node joins its first campaign.
				current, err = client.CoreV1().Services(service.Namespace).Get(context.Background(), service.Name, metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				next.start(t, current)
			}
			next.waitForDatapath(t, service, true)
			if !test.serviceElection && first.processor.findServiceInstance(service) != originalInstance {
				t.Fatal("recovery replaced the instance instead of reconciling its egress state")
			}
			restored, err := client.CoreV1().Services(service.Namespace).Get(context.Background(), service.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if restored.Annotations[kubevip.ActiveEndpoint] != "10.0.0.1" ||
				restored.Annotations[kubevip.EgressNftablesTable] != "kube_vip_recovery" {
				t.Fatalf("recovery left incorrect Service annotations: %v", restored.Annotations)
			}
			if test.serviceElection {
				assertRecoveryLeaseHolder(t, client, service, test.nextLeader)
			}
			if next == second {
				first.waitForDatapath(t, service, false)
			}
			t.Logf("%s recovered the VIP route and endpoint-specific SNAT rule without restarting either processor", test.nextLeader)
		})
	}
}

func TestRecoverEgressAfterSNATFailure(t *testing.T) {
	for _, test := range []struct {
		name   string
		legacy bool
		update bool
		stop   string
	}{
		{name: "EndpointSlices"},
		{name: "legacy Endpoints", legacy: true},
		{name: "endpoint reconfiguration", update: true},
		{name: "newer Service endpoint", stop: "endpoint"},
		{name: "endpoint not yet persisted", stop: "local"},
		{name: "endpoint reverts", update: true, stop: "revert"},
		{name: "leadership loss", stop: "leader"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if runIsolatedEgressTest(t) {
				return
			}
			service := &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name: "egress", Namespace: "default", UID: "egress-recovery",
					Annotations: map[string]string{
						kubevip.Egress: "true", kubevip.ActiveEndpoint: "10.0.0.1", kubevip.EgressDestinationPorts: "tcp:5060",
					},
				},
				Spec: v1.ServiceSpec{
					Type: v1.ServiceTypeLoadBalancer, LoadBalancerIP: "192.0.2.10",
					ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyLocal,
				},
			}
			client := newEgressRecoveryClient(t, service)
			namespace, err := netns.Get()
			if err != nil {
				t.Fatal(err)
			}
			node := newEgressRecoveryNodeInNamespace(t, "node-a", client, service, true, namespace)
			if test.legacy {
				config := node.processor.config
				config.EnableEndpoints = true
				node.endpoints = endpoints.NewEndpointProcessor(config, providers.NewEndpoints(), nil,
					node.processor.findServiceInstance, nil, node.processor.routeMgr, node.processor.serviceLock)
			}
			node.reconcile(t, service, true)
			if test.update {
				node.start(t, service)
				node.waitForDatapath(t, service, true)
				if err := nftables.DeleteSNATFromAllTables(string(service.UID)); err != nil {
					t.Fatal(err)
				}
			}
			unblockSNAT := blockEgressSNATChain(t, service)
			desired := service.DeepCopy()
			if test.update {
				desired.Annotations[kubevip.ActiveEndpoint] = "10.0.0.2"
				node.reconcileEndpoint(t, service, "10.0.0.2")
			} else {
				node.start(t, service)
			}
			node.waitForDatapathState(t, desired, true, false)
			assertRecoveryLeaseHolder(t, client, service, "node-a")

			switch test.stop {
			case "endpoint":
				// Change the API object before the endpoint watcher reports it.
				api := client.CoreV1().Services(service.Namespace)
				latest, err := api.Get(t.Context(), service.Name, metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				latest.Annotations[kubevip.ActiveEndpoint] = "10.0.0.2"
				if _, err := api.Update(t.Context(), latest, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
				unblockSNAT()
				node.waitForEgressRetryToStop(t, service)
				node.waitForDatapathState(t, service, true, false)
				node.reconcileEndpoint(t, service, "10.0.0.2")
				node.waitForDatapath(t, latest, true)
			case "local":
				// Model a watcher that selected a different endpoint while
				// writing the annotation to the API was temporarily failing.
				node.ctx.SetEgressEndpoint("10.0.0.2")
				unblockSNAT()
				node.waitForEgressRetryToStop(t, service)
				node.waitForDatapathState(t, service, true, false)
			case "revert":
				unblockSNAT()
				node.reconcile(t, service, true)
				node.waitForDatapath(t, service, true)
				node.waitForDatapathState(t, desired, true, false)
			case "leader":
				// Force a new live lease holder while the endpoint stays ready.
				leases := client.CoordinationV1().Leases(service.Namespace)
				active, err := leases.Get(t.Context(), "kubevip-"+service.Name, metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				newHolder, duration := "node-b", int32(60)
				active.Spec.HolderIdentity = &newHolder
				active.Spec.LeaseDurationSeconds = &duration
				active.Spec.RenewTime = &metav1.MicroTime{Time: time.Now()}
				if _, err := leases.Update(t.Context(), active, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
				node.waitForDatapath(t, service, false)
				unblockSNAT()
				node.waitForEgressRetryToStop(t, service)
				node.waitForDatapath(t, service, false)
				assertRecoveryLeaseHolder(t, client, service, newHolder)
			default:
				unblockSNAT()
				// No Service or endpoint changes occur after removing the fault.
				node.waitForDatapath(t, desired, true)
				assertRecoveryLeaseHolder(t, client, service, "node-a")
				assertRecoveredSNATTraffic(t, desired)
				restored, err := client.CoreV1().Services(service.Namespace).Get(t.Context(), service.Name, metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if restored.Annotations[kubevip.EgressNftablesTable] != "kube_vip_recovery" {
					t.Fatal("recovery did not publish the nftables table annotation")
				}
			}
		})
	}
}

func TestRecoverEgressFailedFamilyOnly(t *testing.T) {
	if runIsolatedEgressTest(t) {
		return
	}
	svc := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "egress", Namespace: "default", UID: "egress-recovery",
		Annotations: map[string]string{
			kubevip.ActiveEndpoint:           "10.0.0.1",
			kubevip.ActiveEndpointIPv6:       "fd00:10::1",
			kubevip.LoadbalancerIPAnnotation: "192.0.2.10,2001:db8::10",
		},
	}}
	p := &Processor{config: &kubevip.Config{
		InstanceName: "recovery", EgressWithNftables: true,
		EgressPodCidr: "10.0.0.0/16,fd00:10::/64", EgressServiceCidr: "10.96.0.0/12,fd00:20::/64",
	}}
	unblockSNAT := blockEgressSNATChain(t, svc)
	failed, configured, err := p.programServiceEgress(t.Context(), svc, egressAllFamilies)
	if err == nil || failed != egressIPv4 || len(configured) == 0 {
		t.Fatalf("initial programming = families %d, configured %v, error %v", failed, configured, err)
	}
	conn := &kernelnft.Conn{}
	chain := nftables.GetSNatChainForTable(true, string(svc.UID), "kube_vip_recovery")
	ipv6Rules, err := conn.GetRules(chain.Table, chain)
	if err != nil || len(ipv6Rules) != 1 {
		t.Fatalf("healthy family has %d rules, error %v", len(ipv6Rules), err)
	}

	unblockSNAT()
	failed, configured, err = p.programServiceEgress(t.Context(), svc, failed)
	if err != nil || failed != 0 || len(configured) == 0 {
		t.Fatalf("retry = families %d, configured %v, error %v", failed, configured, err)
	}
	after, err := conn.GetRules(chain.Table, chain)
	if err != nil || len(after) != 1 || after[0].Handle != ipv6Rules[0].Handle {
		t.Fatalf("retry changed the healthy family: %v (%v)", after, err)
	}
}

func TestRecoverEgressWhileActiveEndpointAnnotationFails(t *testing.T) {
	for _, provider := range []string{"EndpointSlices", "Endpoints"} {
		t.Run(provider, func(t *testing.T) {
			t.Parallel()
			if runIsolatedEgressTest(t) {
				return
			}
			service := &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name: "egress", Namespace: "default", UID: "egress-recovery",
					Annotations: map[string]string{kubevip.Egress: "true", kubevip.EgressDestinationPorts: "tcp:5060"},
				},
				Spec: v1.ServiceSpec{
					Type: v1.ServiceTypeLoadBalancer, LoadBalancerIP: "192.0.2.10",
					ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyLocal,
				},
			}
			var failActiveEndpointWrites atomic.Bool
			var rejectUntil atomic.Pointer[time.Time]
			var rejected atomic.Int32
			client := newEgressRecoveryClientWithServiceUpdate(t, service, func(updated *v1.Service) bool {
				if !failActiveEndpointWrites.Load() || updated.Annotations[kubevip.ActiveEndpoint] == "" {
					return false
				}
				now := time.Now()
				if rejectUntil.Load() == nil {
					until := now.Add(30 * time.Second)
					rejectUntil.CompareAndSwap(nil, &until)
				}
				if now.Before(*rejectUntil.Load()) {
					rejected.Add(1)
					return true
				}
				return false
			})
			namespace, err := netns.Get()
			if err != nil {
				t.Fatal(err)
			}
			node := newEgressRecoveryNodeInNamespace(t, "node-a", client, service, true, namespace)
			node.datapath.readCurrentService = true
			if provider == "Endpoints" {
				node.processor.config.EnableEndpoints = true
				node.endpoints = endpoints.NewEndpointProcessor(node.processor.config, providers.NewEndpoints(), nil,
					node.processor.findServiceInstance, nil, node.processor.routeMgr, node.processor.serviceLock)
			}
			node.reconcile(t, service, true)
			node.start(t, service)
			want := serviceWithEgressEndpoint(service, kubevip.ActiveEndpoint, "10.0.0.1")
			node.waitForDatapath(t, want, true)
			assertRecoveryLeaseHolder(t, client, service, "node-a")

			setRecoveryLeaseHolder(t, client, service, "node-b")
			node.waitForDatapath(t, want, false)
			api := client.CoreV1().Services(service.Namespace)
			current, err := api.Get(t.Context(), service.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			delete(current.Annotations, kubevip.ActiveEndpoint)
			if _, err := api.Update(t.Context(), current, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			// Allow lease reads and renewals throughout the outage. The selected
			// endpoint remains Ready and the watcher receives no further events.
			failActiveEndpointWrites.Store(true)
			setRecoveryLeaseHolder(t, client, service, "")
			node.waitForDatapath(t, want, true)
			assertRecoveryLeaseHolder(t, client, service, "node-a")
			assertRecoveredSNATTraffic(t, want)
			current, err = api.Get(t.Context(), service.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			recoverAt := rejectUntil.Load()
			if recoverAt == nil || !time.Now().Before(*recoverAt) || current.Annotations[kubevip.ActiveEndpoint] != "" || rejected.Load() == 0 {
				t.Fatalf("expected working SNAT while active-endpoint writes failed, annotation=%q, failed writes=%d",
					current.Annotations[kubevip.ActiveEndpoint], rejected.Load())
			}
			ctx, cancel := context.WithDeadline(t.Context(), recoverAt.Add(30*time.Second))
			defer cancel()
			if err := wait.PollUntilContextCancel(ctx, 50*time.Millisecond, true, func(ctx context.Context) (bool, error) {
				latest, err := api.Get(ctx, service.Name, metav1.GetOptions{})
				return err == nil && latest.Annotations[kubevip.ActiveEndpoint] == want.Annotations[kubevip.ActiveEndpoint], err
			}); err != nil {
				t.Fatalf("active-endpoint was not republished after the API recovered: %v", err)
			}
			node.waitForEgressRetryToStop(t, service)
			node.waitForDatapath(t, want, true)
		})
	}
}

func TestRecoverEgressAfterRuleRemoval(t *testing.T) {
	if runIsolatedEgressTest(t) {
		return
	}
	service := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "egress", Namespace: "default", UID: "egress-recovery",
			Annotations: map[string]string{
				kubevip.Egress: "true", kubevip.ActiveEndpoint: "10.0.0.1",
				kubevip.EgressDestinationPorts: "tcp:5060",
				kubevip.EgressAllowedNetworks:  "198.51.100.0/24,203.0.113.0/24",
			},
		},
		Spec: v1.ServiceSpec{
			Type: v1.ServiceTypeLoadBalancer, LoadBalancerIP: "192.0.2.10",
			ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyLocal,
		},
	}
	client := newEgressRecoveryClient(t, service)
	namespace, err := netns.Get()
	if err != nil {
		t.Fatal(err)
	}
	node := newEgressRecoveryNodeInNamespace(t, "node-a", client, service, true, namespace)
	const checkIntervalSeconds = 1
	node.processor.config.EgressVerificationInterval = checkIntervalSeconds
	node.reconcile(t, service, true)
	node.start(t, service)
	node.waitForDatapath(t, service, true)
	assertRecoveryLeaseHolder(t, client, service, "node-a")

	conn := &kernelnft.Conn{}
	want := nftables.GetSNatChainForTable(false, string(service.UID), "kube_vip_recovery")
	before, err := conn.GetRules(want.Table, want)
	if err != nil || len(before) != 2 {
		t.Fatalf("expected two installed SNAT rules, got %d (error %v)", len(before), err)
	}
	time.Sleep(3 * checkIntervalSeconds * time.Second)
	intact, err := conn.GetRules(want.Table, want)
	if err != nil || len(intact) != len(before) || intact[0].Handle != before[0].Handle || intact[1].Handle != before[1].Handle {
		t.Fatalf("healthy SNAT rules changed during verification (error %v)", err)
	}

	if err := conn.DelRule(intact[0]); err != nil {
		t.Fatal(err)
	}
	if err := conn.Flush(); err != nil {
		t.Fatal(err)
	}
	expected := map[string]int{"192.0.2.10": 2}
	if err := wait.PollUntilContextTimeout(t.Context(), 20*time.Millisecond, 5*time.Second, true,
		func(context.Context) (bool, error) {
			// Read the rules between reconciliations, as the verifier itself
			// does. Programming a family can temporarily remove its chain.
			p := node.processor
			p.serviceLock.Lock(service.UID)
			ok, err := nftables.HasSNATRules(false, string(service.UID), "kube_vip_recovery", expected)
			return ok, errors.Join(err, p.serviceLock.Unlock(service.UID))
		}); err != nil {
		t.Fatalf("wait for SNAT rule to be restored: %v", err)
	}
	node.waitForEgressRetryToStop(t, service)
	assertRecoveredSNATTraffic(t, service)

	// Keep a second repair pending while the leader loses ownership.
	if err := nftables.DeleteSNATFromTable(false, string(service.UID), "kube_vip_recovery"); err != nil {
		t.Fatal(err)
	}
	unblockSNAT := blockEgressSNATChain(t, service)
	if err := wait.PollUntilContextTimeout(t.Context(), 10*time.Millisecond, 3*time.Second, true, func(context.Context) (bool, error) {
		p := node.processor
		p.serviceLock.Lock(service.UID)
		value, ok := p.serviceEgress.Load(service.UID)
		pending := ok && value.(*serviceEgressState).retry != nil
		return pending, p.serviceLock.Unlock(service.UID)
	}); err != nil {
		t.Fatalf("wait for the next SNAT repair to be queued: %v", err)
	}
	setRecoveryLeaseHolder(t, client, service, "node-b")
	node.waitForDatapath(t, service, false)
	unblockSNAT()
	node.waitForEgressRetryToStop(t, service)
	time.Sleep(2 * time.Second)
	node.waitForDatapath(t, service, false)
	assertRecoveryLeaseHolder(t, client, service, "node-b")
}

// Use a fresh TCP connection so conntrack cannot reuse an existing translation.
func assertRecoveredSNATTraffic(t *testing.T, service *v1.Service) {
	t.Helper()
	endpoint, vip := service.Annotations[kubevip.ActiveEndpoint], service.Spec.LoadBalancerIP
	const peer = "198.51.100.10"
	loopback, err := netlink.LinkByName("lo")
	if err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkSetUp(loopback); err != nil {
		t.Fatal(err)
	}
	for _, ip := range []string{endpoint, peer, vip} {
		addr, err := netlink.ParseAddr(ip + "/32")
		if err != nil {
			t.Fatal(err)
		}
		if err := netlink.AddrAdd(loopback, addr); err != nil && !errors.Is(err, syscall.EEXIST) {
			t.Fatal(err)
		}
	}
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.ParseIP(peer), Port: 5060})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := listener.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	dialer := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(endpoint)}, Timeout: 3 * time.Second}
	client, err := dialer.DialContext(t.Context(), "tcp4", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	conn, err := listener.AcceptTCP()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	got, _, err := net.SplitHostPort(conn.RemoteAddr().String())
	if err != nil || got != vip {
		t.Fatalf("TCP peer saw source %q, want %q (error %v)", got, vip, err)
	}
}

func (n *egressRecoveryNode) waitForEgressRetryToStop(t *testing.T, service *v1.Service) {
	t.Helper()
	if err := wait.PollUntilContextTimeout(t.Context(), 10*time.Millisecond, 5*time.Second, true, func(context.Context) (bool, error) {
		p := n.processor
		p.serviceLock.Lock(service.UID)
		value, ok := p.serviceEgress.Load(service.UID)
		stopped := !ok || value.(*serviceEgressState).retry == nil
		return stopped, p.serviceLock.Unlock(service.UID)
	}); err != nil {
		t.Fatalf("wait for pending egress retry: %v", err)
	}
}

// A jump to the Service's chain makes nftables reject deleting that chain with
// EBUSY. Removing the jump allows the ordinary rule setup to succeed.
func blockEgressSNATChain(t *testing.T, service *v1.Service) func() {
	t.Helper()
	want := nftables.GetSNatChainForTable(false, string(service.UID), "kube_vip_recovery")
	conn := &kernelnft.Conn{}
	table := conn.AddTable(want.Table)
	conn.AddChain(&kernelnft.Chain{Name: want.Name, Table: table})
	blocker := conn.AddChain(&kernelnft.Chain{Name: "test_block_snat_cleanup", Table: table})
	conn.AddRule(&kernelnft.Rule{Table: table, Chain: blocker,
		Exprs: []expr.Any{&expr.Verdict{Kind: expr.VerdictJump, Chain: want.Name}}})
	if err := conn.Flush(); err != nil {
		t.Fatal(err)
	}
	return func() {
		t.Helper()
		conn.DelChain(blocker)
		if err := conn.Flush(); err != nil {
			t.Fatal(err)
		}
	}
}

// Run each test that exercises background netlink operations in its own process
// namespace. Keeping the test name exact also isolates the state of each subtest.
func runIsolatedEgressTest(t *testing.T) bool {
	t.Helper()
	if os.Getenv("KUBE_VIP_EGRESS_RECOVERY_CHILD") != "" {
		return false
	}
	pattern := strings.Split(t.Name(), "/")
	for i := range pattern {
		pattern[i] = "^" + regexp.QuoteMeta(pattern[i]) + "$"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer cancel()
	//nolint:gosec // Re-executes this test binary using only its own test name.
	command := exec.CommandContext(ctx, os.Args[0], "-test.run="+strings.Join(pattern, "/"), "-test.v", "-test.timeout=70s")
	command.Env = append(os.Environ(), "KUBE_VIP_EGRESS_RECOVERY_CHILD=1")
	command.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWNET}
	output, err := command.CombinedOutput()
	if err != nil {
		if !requireNetworkNamespaces && (errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES)) {
			t.Skipf("network namespaces unavailable: %v", err)
		}
		t.Fatalf("isolated recovery test: %v\n%s", err, output)
	}
	t.Logf("%s", output)
	return true
}

type egressRecoveryNode struct {
	processor *Processor
	endpoints *endpoints.Processor
	datapath  *egressRecoveryDatapath
	ctx       *servicecontext.Context
	namespace netns.NsHandle
	lastKnown string
	wg        sync.WaitGroup
}

func newEgressRecoveryNode(t *testing.T, name string, client *kubernetes.Clientset, service *v1.Service, serviceElection bool) *egressRecoveryNode {
	t.Helper()
	runtime.LockOSThread()
	original, err := netns.Get()
	if err != nil {
		runtime.UnlockOSThread()
		t.Fatal(err)
	}
	namespace, createErr := netns.New()
	restoreErr := netns.Set(original)
	_ = original.Close()
	runtime.UnlockOSThread()
	if restoreErr != nil {
		t.Fatalf("restore network namespace: %v", restoreErr)
	}
	if createErr != nil {
		if requireNetworkNamespaces {
			t.Fatal(createErr)
		}
		t.Skipf("network namespaces unavailable: %v", createErr)
	}
	return newEgressRecoveryNodeInNamespace(t, name, client, service, serviceElection, namespace)
}

func newEgressRecoveryNodeInNamespace(t *testing.T, name string, client *kubernetes.Clientset, service *v1.Service,
	serviceElection bool, namespace netns.NsHandle) *egressRecoveryNode {
	t.Helper()
	t.Cleanup(func() { _ = namespace.Close() })
	if err := inEgressRecoveryNamespace(namespace, func() error {
		link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "kvrecover0"}}
		if err := netlink.LinkAdd(link); err != nil {
			return err
		}
		return netlink.LinkSetUp(link)
	}); err != nil {
		t.Fatal(err)
	}
	config := &kubevip.Config{
		NodeName: name, InstanceName: "recovery", Interface: "kvrecover0", VIPSubnet: "32",
		EnableServicesElection: serviceElection, EnableRoutingTable: true, LeaderElectionType: "kubernetes",
		RoutingTableID: 200, RoutingTableType: syscall.RTN_LOCAL, RoutingProtocol: recoveryProtocol,
		DisableServiceUpdates: true, EgressWithNftables: true,
		EgressPodCidr: "10.0.0.0/16", EgressServiceCidr: "10.96.0.0/12",
		KubernetesLeaderElection: kubevip.KubernetesLeaderElection{LeaseDuration: 4, RenewDeadline: 3, RetryPeriod: 1},
	}
	electionManager, err := election.NewManager(config, client, client)
	if err != nil {
		t.Fatal(err)
	}
	processor, err := NewServicesProcessor(config, nil, client, client, networkinterface.NewManager(), nil,
		&testLabeler{}, electionManager, lease.NewManager(), route.NewManager())
	if err != nil {
		t.Fatal(err)
	}
	adapter := &electionAdapter{processor: processor}
	datapath := &egressRecoveryDatapath{electionAdapter: adapter, namespace: namespace}
	processor.electionCoordinators, err = serviceelection.NewManager(&serviceelection.Dependencies{
		Config: config, Leases: processor.leaseMgr, ElectionManager: electionManager,
		State: adapter, Datapath: datapath, Runner: adapter, Scheduler: adapter,
	})
	if err != nil {
		t.Fatal(err)
	}
	node := &egressRecoveryNode{
		processor: processor, ctx: publishTestServiceContext(processor, service), namespace: namespace, datapath: datapath,
	}
	node.endpoints = endpoints.NewEndpointProcessor(config, providers.NewEndpointslices(), nil,
		processor.findServiceInstance, nil, processor.routeMgr, processor.serviceLock)
	t.Cleanup(func() {
		if !serviceElection {
			if err := inEgressRecoveryNamespace(namespace, func() error {
				return processor.deleteServiceForContext(context.Background(), service.UID, node.ctx)
			}); err != nil {
				t.Errorf("clean service: %v", err)
			}
		}
		node.ctx.Cancel()
		node.wg.Wait()
		processor.electionCoordinators.Wait()
	})
	return node
}

func (n *egressRecoveryNode) start(t *testing.T, service *v1.Service) {
	t.Helper()
	n.wg.Go(func() {
		if err := n.processor.StartServicesLeaderElection(n.ctx, service, &n.wg); err != nil {
			t.Errorf("service election: %v", err)
		}
	})
}

func (n *egressRecoveryNode) reconcile(t *testing.T, service *v1.Service, present bool) {
	t.Helper()
	endpoint := ""
	if present {
		endpoint = "10.0.0.1"
	}
	n.reconcileEndpoint(t, service, endpoint)
}

func (n *egressRecoveryNode) reconcileEndpoint(t *testing.T, service *v1.Service, endpoint string) {
	t.Helper()
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Name: "egress-slice", Namespace: service.Namespace},
		AddressType: discoveryv1.AddressTypeIPv4,
	}
	if endpoint != "" {
		slice.Endpoints = []discoveryv1.Endpoint{{Addresses: []string{endpoint}, NodeName: &n.processor.config.NodeName}}
	}
	event := watch.Event{Type: watch.Modified, Object: slice}
	if n.processor.config.EnableEndpoints {
		//nolint:staticcheck // Deliberately exercise the legacy Endpoints provider.
		legacy := &v1.Endpoints{ObjectMeta: metav1.ObjectMeta{Name: service.Name, Namespace: service.Namespace}}
		if endpoint != "" {
			//nolint:staticcheck // Deliberately exercise the legacy Endpoints provider.
			legacy.Subsets = []v1.EndpointSubset{{Addresses: []v1.EndpointAddress{{IP: endpoint, NodeName: &n.processor.config.NodeName}}}}
		}
		event.Object = legacy
	}
	err := inEgressRecoveryNamespace(n.namespace, func() error {
		_, err := n.endpoints.Reconcile(n.ctx, event, &n.lastKnown,
			service, n.processor.config.NodeName, &n.wg, n.processor.clientSet,
			func(ctx context.Context, updated *v1.Service, inst *instance.Instance) error {
				return n.processor.updateEgressConfiguration(ctx, n.ctx, updated, inst)
			})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (n *egressRecoveryNode) waitForDatapath(t *testing.T, service *v1.Service, present bool) {
	t.Helper()
	n.waitForDatapathState(t, service, present, present)
}

func (n *egressRecoveryNode) waitForDatapathState(t *testing.T, service *v1.Service, wantRoute, wantSNAT bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var routeFound, snatFound, instanceFound bool
	for {
		n.processor.serviceLock.Lock(service.UID)
		instanceFound = n.processor.findServiceInstance(service) != nil
		err := inEgressRecoveryNamespace(n.namespace, func() error {
			routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: 200}, netlink.RT_FILTER_TABLE)
			if err != nil {
				return err
			}
			routeFound = false
			for _, r := range routes {
				if r.Dst != nil && r.Dst.String() == "192.0.2.10/32" && r.Protocol == recoveryProtocol {
					routeFound = true
				}
			}
			conn := &kernelnft.Conn{}
			chains, err := conn.ListChains()
			if err != nil {
				return err
			}
			want := nftables.GetSNatChainForTable(false, string(service.UID), "kube_vip_recovery")
			snatFound = false
			for _, chain := range chains {
				if chain.Table.Name != want.Table.Name || chain.Name != want.Name {
					continue
				}
				rules, err := conn.GetRules(chain.Table, chain)
				if err != nil {
					return err
				}
				for _, rule := range rules {
					var source, target, snat bool
					for _, expression := range rule.Exprs {
						switch e := expression.(type) {
						case *expr.Lookup:
							set, err := conn.GetSetByName(chain.Table, e.SetName)
							if err != nil {
								return err
							}
							elements, err := conn.GetSetElements(set)
							if err != nil {
								return err
							}
							for _, element := range elements {
								source = source || bytes.Equal(element.Key, net.ParseIP(service.Annotations[kubevip.ActiveEndpoint]).To4())
							}
						case *expr.Immediate:
							target = target || bytes.Equal(e.Data, net.ParseIP("192.0.2.10").To4())
						case *expr.NAT:
							snat = e.Type == expr.NATTypeSourceNAT
						}
					}
					snatFound = snatFound || (source && target && snat)
				}
			}
			return nil
		})
		unlockErr := n.processor.serviceLock.Unlock(service.UID)
		if err != nil || unlockErr != nil {
			t.Fatal(errors.Join(err, unlockErr))
		}
		wantInstance := wantRoute || !n.processor.config.EnableServicesElection
		if routeFound == wantRoute && snatFound == wantSNAT && instanceFound == wantInstance {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: route=%t SNAT=%t instance=%t, want route=%t SNAT=%t instance=%t", n.processor.config.NodeName,
				routeFound, snatFound, instanceFound, wantRoute, wantSNAT, wantInstance)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type egressRecoveryDatapath struct {
	*electionAdapter
	namespace          netns.NsHandle
	readCurrentService bool
}

func (d *egressRecoveryDatapath) Activate(ctx context.Context, service *v1.Service, svcCtx *servicecontext.Context, wg *sync.WaitGroup) error {
	if d.readCurrentService {
		// Simulate the updated Service snapshot without an endpoint event.
		current, err := d.processor.clientSet.CoreV1().Services(service.Namespace).Get(ctx, service.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		service = current
	}
	return inEgressRecoveryNamespace(d.namespace, func() error { return d.electionAdapter.Activate(ctx, service, svcCtx, wg) })
}

func (d *egressRecoveryDatapath) Cleanup(ctx context.Context, service *v1.Service, svcCtx *servicecontext.Context, current func() bool) error {
	return inEgressRecoveryNamespace(d.namespace, func() error { return d.electionAdapter.Cleanup(ctx, service, svcCtx, current) })
}

func inEgressRecoveryNamespace(namespace netns.NsHandle, run func() error) (err error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	original, err := netns.Get()
	if err != nil {
		return err
	}
	defer original.Close()
	defer func() { err = errors.Join(err, netns.Set(original)) }()
	if err := netns.Set(namespace); err != nil {
		return err
	}
	return run()
}

func assertRecoveryLeaseHolder(t *testing.T, client *kubernetes.Clientset, service *v1.Service, holder string) {
	t.Helper()
	namespace, name := lease.ServiceName(service)
	resource, err := client.CoordinationV1().Leases(namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if resource.Spec.HolderIdentity == nil || *resource.Spec.HolderIdentity != holder {
		t.Fatalf("lease holder = %v, want %s", resource.Spec.HolderIdentity, holder)
	}
}

func setRecoveryLeaseHolder(t *testing.T, client *kubernetes.Clientset, service *v1.Service, holder string) {
	t.Helper()
	namespace, name := lease.ServiceName(service)
	api := client.CoordinationV1().Leases(namespace)
	old, err := api.Get(t.Context(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	duration := int32(60)
	old.Spec.HolderIdentity = &holder
	old.Spec.LeaseDurationSeconds = &duration
	old.Spec.RenewTime = &metav1.MicroTime{Time: time.Now()}
	if _, err := api.Update(t.Context(), old, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

// A small JSON REST transport shared by both real client-go electors. Writes
// use resourceVersion checks, including the lock's out-of-band annotation update.
func newEgressRecoveryClient(t *testing.T, service *v1.Service) *kubernetes.Clientset {
	t.Helper()
	return newEgressRecoveryClientWithServiceUpdate(t, service, nil)
}

func newEgressRecoveryClientWithServiceUpdate(t *testing.T, service *v1.Service, reject func(*v1.Service) bool) *kubernetes.Clientset {
	t.Helper()
	var mu sync.Mutex
	version := 1
	service = service.DeepCopy()
	service.ResourceVersion = "1"
	objects := map[string]k8sruntime.Object{"/api/v1/namespaces/default/services/egress": service}
	client, err := kubernetes.NewForConfig(&rest.Config{
		Host: "https://egress-recovery.test", ContentConfig: rest.ContentConfig{ContentType: "application/json"},
		Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			mu.Lock()
			defer mu.Unlock()
			path := request.URL.Path
			object := objects[path]
			status := http.StatusOK
			if request.Method == http.MethodPost || request.Method == http.MethodPut {
				body, err := io.ReadAll(request.Body)
				if err != nil {
					return nil, err
				}
				updated, _, err := scheme.Codecs.UniversalDeserializer().Decode(body, nil, nil)
				if err != nil {
					return nil, err
				}
				metadata, err := meta.Accessor(updated)
				if err != nil {
					return nil, err
				}
				if request.Method == http.MethodPost {
					path += "/" + metadata.GetName()
					object = objects[path]
				}
				if object != nil {
					current, err := meta.Accessor(object)
					if err != nil {
						return nil, err
					}
					if request.Method == http.MethodPost || metadata.GetResourceVersion() != current.GetResourceVersion() {
						status = http.StatusConflict
						object = &metav1.Status{Status: "Failure", Reason: metav1.StatusReasonConflict, Code: int32(status)}
					}
				}
				if status == http.StatusOK {
					if update, ok := updated.(*v1.Service); ok && reject != nil && reject(update) {
						status = http.StatusInternalServerError
						object = &metav1.Status{Status: "Failure", Reason: metav1.StatusReasonInternalError, Code: int32(status)}
					} else {
						version++
						metadata.SetResourceVersion(strconv.Itoa(version))
						objects[path], object = updated.DeepCopyObject(), updated
					}
				}
			}
			if object == nil {
				status = http.StatusNotFound
				object = &metav1.Status{Status: "Failure", Reason: metav1.StatusReasonNotFound, Code: int32(status)}
			}
			body, err := json.Marshal(object)
			if err != nil {
				return nil, fmt.Errorf("encode API response: %w", err)
			}
			return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}},
				Body: io.NopCloser(bytes.NewReader(body)), Request: request}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}
