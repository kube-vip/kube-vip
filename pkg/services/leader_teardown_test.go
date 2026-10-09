package services

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/arp"
	"github.com/kube-vip/kube-vip/pkg/cluster"
	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/node/noop"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	"github.com/kube-vip/kube-vip/pkg/vip"
	"github.com/vishvananda/netlink"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// apiUnavailable mirrors the error the labeler produces when the Kubernetes
// API is unreachable, e.g. during an etcd/apiserver stall:
//
//	error removing label from node: node patching failed with patch [...]:
//	Patch "https://10.43.0.1:443/api/v1/nodes/marble": dial tcp 10.43.0.1:443:
//	connect: connection refused
const apiUnavailable = "node patching failed with patch " +
	"[{\"op\":\"remove\",\"path\":\"/metadata/labels/" +
	"service-provided.kube-vip.io~1traefik.kube-system\"}]: " +
	"Patch \"https://10.43.0.1:443/api/v1/nodes/marble\": " +
	"dial tcp 10.43.0.1:443: connect: connection refused"

// unavailableLabeler records label removal attempts and fails them the way a
// node whose API endpoint is down does.
type unavailableLabeler struct {
	mu       sync.Mutex
	attempts []map[string]string
}

func (l *unavailableLabeler) AddLabel(map[string]string) error { return nil }

func (l *unavailableLabeler) RemoveLabel(labels map[string]string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.attempts = append(l.attempts, labels)
	return fmt.Errorf("%s", apiUnavailable)
}

func (l *unavailableLabeler) removalAttempts() []map[string]string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]map[string]string, len(l.attempts))
	copy(out, l.attempts)
	return out
}

// recordingNetwork stands in for the netlink layer below vip.Network, which
// unit tests must not touch. It counts DeleteIP calls.
type recordingNetwork struct {
	mu        sync.Mutex
	deleteIPs int

	ip      string
	arpName string
}

func (n *recordingNetwork) deleteIPCalls() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.deleteIPs
}

func (n *recordingNetwork) resetDeleteIPs() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.deleteIPs = 0
}

func (n *recordingNetwork) AddIP(bool, bool, ...int) (bool, error) { return false, nil }
func (n *recordingNetwork) AddRoute(bool) (bool, error)            { return false, nil }
func (n *recordingNetwork) ReplaceRoute() error                    { return nil }
func (n *recordingNetwork) DeleteIP() (bool, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.deleteIPs++
	return true, nil
}
func (n *recordingNetwork) DeleteRoute() error            { return nil }
func (n *recordingNetwork) UpdateRoutes() (bool, error)   { return false, nil }
func (n *recordingNetwork) IsSet() (*netlink.Addr, error) { return nil, nil }
func (n *recordingNetwork) IP() string                    { return n.ip }
func (n *recordingNetwork) CIDR() string                  { return n.ip + "/32" }
func (n *recordingNetwork) IPisLinkLocal() bool           { return false }
func (n *recordingNetwork) PrepareRoute() *netlink.Route  { return nil }
func (n *recordingNetwork) RouteHash() string             { return "" }
func (n *recordingNetwork) SetIP(string) error            { return nil }
func (n *recordingNetwork) SetServicePorts(*v1.Service)   {}
func (n *recordingNetwork) Interface() string             { return "lo" }
func (n *recordingNetwork) IsDADFAIL() bool               { return false }
func (n *recordingNetwork) IsDNS() bool                   { return false }
func (n *recordingNetwork) IsDDNS() bool                  { return false }
func (n *recordingNetwork) DDNSHostName() string          { return "" }
func (n *recordingNetwork) DNSName() string               { return "" }
func (n *recordingNetwork) SetMask(string) error          { return nil }
func (n *recordingNetwork) SetHasEndpoints(bool)          {}
func (n *recordingNetwork) HasEndpoints() bool            { return false }
func (n *recordingNetwork) ARPName() string               { return n.arpName }
func (n *recordingNetwork) GetPossibleSubnets() string    { return "" }
func (n *recordingNetwork) DHCPFamily() string            { return "" }
func (n *recordingNetwork) IPVSMark() uint32              { return 0 }

var _ vip.Network = (*recordingNetwork)(nil)

func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond * 10)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, msg)
}

// syncWriter buffers log records written from several goroutines so the test
// handler stays race-detector clean.
type syncWriter struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// TestReleaseAbortsVIPDeletionWhenAPIUnavailable is the regression test for
// issue #1775: losing a service lease while the Kubernetes API is unreachable
// must still release the local VIP and stop the ARP broadcaster.
//
// Before the fix, deleteService performed the API-dependent node-label removal
// first; when that patch failed with "connection refused" it returned early and
// never stopped the cluster, so:
//   - the gratuitous-ARP broadcaster kept running,
//   - the VIP stayed bound on the interface,
//   - the service instance stayed registered in p.ServiceInstances.
//
// The node then keeps answering ARP for a VIP it no longer owns and flaps with
// the new leader. Local teardown has to run first and unconditionally; API
// bookkeeping is attempted afterwards best-effort and surfaced (warning +
// returned error) rather than aborting the release.
func TestReleaseAbortsVIPDeletionWhenAPIUnavailable(t *testing.T) {
	const vipAddress = "192.0.2.10"
	const arpName = vipAddress + "/32-lo"
	const labelKey = "service-provided.kube-vip.io/traefik.kube-system"

	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "traefik",
			Namespace: "kube-system",
			UID:       types.UID("uid-traefik"),
			Annotations: map[string]string{
				kubevip.LoadbalancerIPAnnotation: vipAddress,
			},
		},
		Spec: v1.ServiceSpec{
			Type:           v1.ServiceTypeLoadBalancer,
			LoadBalancerIP: vipAddress,
		},
	}

	cfg := &kubevip.Config{
		NodeName:         "marble",
		Interface:        "lo",
		EnableARP:        true,
		ArpBroadcastRate: 3000,
		VIP:              vipAddress,
	}

	// The ARP manager is real: the point of the test is that the ARP instance
	// and the VIP go away, so only the netlink layer below is faked.
	arpMgr := arp.NewManager(cfg)
	network := &recordingNetwork{ip: vipAddress, arpName: arpName}

	cl, err := cluster.InitCluster(cfg, true, nil, arpMgr, nil, noop.NewManager())
	if err != nil {
		t.Fatalf("init cluster: %v", err)
	}
	cl.Network = []vip.Network{network}

	labeler := &unavailableLabeler{}
	inst := &instance.Instance{
		ServiceUID:      svc.UID,
		ServiceSnapshot: svc,
		LabelAdded:      true,
		Clusters:        []*cluster.Cluster{cl},
	}

	p := &Processor{
		config:           cfg,
		leaseMgr:         lease.NewManager(),
		nodeLabelManager: labeler,
		ServiceInstances: []*instance.Instance{inst},
	}

	svcCtx := servicecontext.New(context.Background())
	p.svcMap.Store(svc.UID, svcCtx)
	// Unblock the load-balancer goroutines no matter where the assertions land.
	t.Cleanup(func() { svcCtx.Cancel() })

	leaseNamespace, serviceLease := lease.ServiceName(svc)
	svcLease := p.leaseMgr.Add(context.Background(), lease.NewID(cfg.LeaderElectionType, leaseNamespace, serviceLease))

	// Become the leader the way SyncServices does: bind the VIP, run the ARP
	// broadcaster, and wait until the broadcaster has registered itself.
	var wg sync.WaitGroup
	if err := cl.StartLoadBalancerService(svcCtx.Ctx, cfg, nil, lease.ServiceNamespacedName(svc), &wg); err != nil {
		t.Fatalf("start load balancer service: %v", err)
	}
	waitFor(t, time.Second*10, func() bool { return arpMgr.Count(arpName) == 1 }, "ARP instance registration")

	// The startup pre-clean calls DeleteIP once; only count releases after this.
	network.resetDeleteIPs()

	logs := &syncWriter{}
	prevLogger := log.Default()
	log.SetDefault(log.New(log.NewTextHandler(logs, &log.HandlerOptions{Level: log.LevelDebug})))
	t.Cleanup(func() { log.SetDefault(prevLogger) })

	// The API goes down mid-release, so the node-label patch fails.
	err = p.onStoppedLeading(svcCtx, svcLease, svc)

	// API bookkeeping is still attempted and its failure still surfaces...
	got := labeler.removalAttempts()
	if len(got) != 1 {
		t.Fatalf("label removal attempts = %d, want 1", len(got))
	}
	if err == nil {
		t.Fatal("expected the failed label removal to surface an error from onStoppedLeading")
	}
	if !strings.Contains(err.Error(), "removing label from node") {
		t.Fatalf("error = %v, want it to report the failed label removal", err)
	}

	// ...but it must not abort the local release.
	if got := len(p.ServiceInstances); got != 0 {
		t.Fatalf("service instance was not deregistered after leadership loss: %d instances left", got)
	}
	if got := labeler.removalAttempts()[0][labelKey]; got != vipAddress {
		t.Fatalf("label removal targeted %q = %q, want %q", labelKey, got, vipAddress)
	}
	waitFor(t, time.Second*5, func() bool { return network.deleteIPCalls() > 0 }, "VIP DeleteIP after leadership loss")
	waitFor(t, time.Second*5, func() bool { return arpMgr.Count(arpName) == 0 }, "ARP instance removal after leadership loss")

	// The failed bookkeeping is a warning, not an error-level abort.
	if strings.Contains(logs.String(), `level=ERROR msg="service deletion"`) {
		t.Fatalf("label failure during release surfaced as ERROR:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), `level=WARN msg="service deletion"`) {
		t.Fatalf("no WARN-level \"service deletion\" record in:\n%s", logs.String())
	}

	// The load balancer teardown (ARP removal + VIP delete + stop marker) runs
	// to completion; it used to be skipped entirely.
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second * 5):
		t.Fatal("load balancer teardown did not complete after leadership loss")
	}
}
