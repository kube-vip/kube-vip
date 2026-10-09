package services

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/networkinterface"
	"github.com/kube-vip/kube-vip/pkg/node/labeler"
	coordinationv1 "k8s.io/api/coordination/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	staleNode       = "marble"
	stalePeer       = "galena"
	staleVIPAddr    = "192.0.2.10"
	staleLabelKey   = "service-provided.kube-vip.io/traefik.kube-system"
	staleLabelPath  = "/metadata/labels/service-provided.kube-vip.io~1traefik.kube-system"
	staleOtherLabel = "service-provided.kube-vip.io/netbird.kube-system"
	staleOtherVIP   = "192.0.2.11"
)

// fakeAPI serves the handful of endpoints the stale-state reconcile reads:
// the node object (and its label patches), the service list/gets, and the
// service leases.
type fakeAPI struct {
	t               *testing.T
	mu              sync.Mutex
	server          *httptest.Server
	nodeLabels      map[string]string
	services        []v1.Service
	leases          map[string]*coordinationv1.Lease // key "ns/name"
	patches         []string
	failPatches     bool
	nodeGets        int
	leaseGets       int
	serviceListGets int
}

func newFakeAPI(t *testing.T) *fakeAPI {
	f := &fakeAPI{t: t, leases: map[string]*coordinationv1.Lease{}}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeAPI) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")

	node := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: staleNode, Labels: f.nodeLabels}}

	switch {
	case strings.HasPrefix(r.URL.Path, "/api/v1/nodes/"):
		switch r.Method {
		case http.MethodGet:
			f.nodeGets++
			writeJSON(f.t, w, node)
		case http.MethodPatch:
			body, _ := io.ReadAll(r.Body)
			if f.failPatches {
				f.patches = append(f.patches, string(body))
				w.WriteHeader(http.StatusInternalServerError)
				writeJSON(f.t, w, &metav1.Status{
					TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
					Status:   "Failure",
					Reason:   "InternalError",
					Message:  "fake api failure", Code: http.StatusInternalServerError,
				})
				return
			}
			f.patches = append(f.patches, string(body))
			writeJSON(f.t, w, node)
		default:
			writeNotFound(f.t, w)
		}
	case strings.HasPrefix(r.URL.Path, "/apis/coordination.k8s.io/v1/namespaces/"):
		f.leaseGets++
		key := leaseKeyFromPath(r.URL.Path)
		if l, ok := f.leases[key]; ok {
			writeJSON(f.t, w, l)
			return
		}
		writeNotFound(f.t, w)
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/services":
		f.serviceListGets++
		writeJSON(f.t, w, &v1.ServiceList{Items: f.services})
	case strings.HasPrefix(r.URL.Path, "/api/v1/namespaces/"):
		for i := range f.services {
			if ("/api/v1/namespaces/" + f.services[i].Namespace + "/services/" + f.services[i].Name) == r.URL.Path {
				writeJSON(f.t, w, &f.services[i])
				return
			}
		}
		writeNotFound(f.t, w)
	default:
		writeNotFound(f.t, w)
	}
}

func leaseKeyFromPath(path string) string {
	trimmed := strings.TrimPrefix(path, "/apis/coordination.k8s.io/v1/namespaces/")
	parts := strings.Split(trimmed, "/")
	if len(parts) == 3 && parts[1] == "leases" {
		return parts[0] + "/" + parts[2]
	}
	return trimmed
}

func writeJSON(t *testing.T, w http.ResponseWriter, obj any) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(obj); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

func writeNotFound(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	w.WriteHeader(http.StatusNotFound)
	writeJSON(t, w, &metav1.Status{
		TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
		Status:   "Failure",
		Reason:   metav1.StatusReasonNotFound, Message: "not found",
		Code: http.StatusNotFound,
	})
}

func (f *fakeAPI) snapshot() (patches []string, nodeGets, leaseGets, listGets int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.patches))
	copy(out, f.patches)
	return out, f.nodeGets, f.leaseGets, f.serviceListGets
}

func staleConfig() *kubevip.Config {
	return &kubevip.Config{
		NodeName:               staleNode,
		Interface:              "lo",
		EnableARP:              true,
		EnableServicesElection: true,
		EnableNodeLabeling:     true,
	}
}

func staleService() *v1.Service {
	return &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "traefik",
			Namespace: "kube-system",
			UID:       types.UID("uid-traefik"),
			Annotations: map[string]string{
				kubevip.LoadbalancerIPAnnotation: staleVIPAddr,
			},
		},
		Spec: v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
	}
}

func staleOtherService() *v1.Service {
	return &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "netbird",
			Namespace: "kube-system",
			UID:       types.UID("uid-netbird"),
			Annotations: map[string]string{
				kubevip.LoadbalancerIPAnnotation: staleOtherVIP,
			},
		},
		Spec: v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
	}
}

func holderLease(name, holder string) *coordinationv1.Lease {
	identity := holder
	return &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kube-system"},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: &identity},
	}
}

type gcRecorder struct {
	mu    sync.Mutex
	calls []string
}

func (g *gcRecorder) gc(adapter, address string, _ *networkinterface.Manager) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, adapter+"/"+address)
	return true, nil
}

func (g *gcRecorder) snapshot() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]string, len(g.calls))
	copy(out, g.calls)
	sort.Strings(out)
	return out
}

func staleProcessor(t *testing.T, f *fakeAPI, cfg *kubevip.Config) (*Processor, *gcRecorder) {
	t.Helper()
	cs, err := kubernetes.NewForConfig(&rest.Config{Host: f.server.URL})
	if err != nil {
		t.Fatalf("create clientset: %v", err)
	}
	rec := &gcRecorder{}
	p := &Processor{
		config:           cfg,
		clientSet:        cs,
		rwClientSet:      cs,
		leaseMgr:         lease.NewManager(),
		nodeLabelManager: labeler.NewManager(cfg.NodeName, cs),
		lbClassFilter:    lbClassFilter,
		gcStaleAddress:   rec.gc,
	}
	return p, rec
}

func assertReleased(t *testing.T, rec *gcRecorder, f *fakeAPI, wantGC []string, wantPatches int) {
	t.Helper()
	got := rec.snapshot()
	want := make([]string, len(wantGC))
	copy(want, wantGC)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("VIP releases = %v, want %v", got, want)
	}
	patches, _, _, _ := f.snapshot()
	if len(patches) != wantPatches {
		t.Fatalf("node label patches = %d (%v), want %d", len(patches), patches, wantPatches)
	}
	if wantPatches > 0 {
		for _, body := range patches {
			if !strings.Contains(body, `"op":"remove"`) {
				t.Fatalf("patch is not a label removal: %s", body)
			}
		}
	}
}

func TestReconcileStaleServiceStateFullPass(t *testing.T) {
	t.Run("releases VIP and label of a lease held by another node", func(t *testing.T) {
		f := newFakeAPI(t)
		f.nodeLabels = map[string]string{staleLabelKey: staleVIPAddr}
		f.services = []v1.Service{*staleService()}
		f.leases["kube-system/kubevip-traefik"] = holderLease("kubevip-traefik", stalePeer)

		p, rec := staleProcessor(t, f, staleConfig())
		if err := p.reconcileStaleServiceState(context.Background(), true); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		assertReleased(t, rec, f, []string{"lo/" + staleVIPAddr}, 1)
		patches, _, _, _ := f.snapshot()
		if !strings.Contains(patches[0], staleLabelPath) {
			t.Fatalf("label patch does not remove the stale label key: %s", patches[0])
		}
	})

	t.Run("keeps state when this node holds the lease", func(t *testing.T) {
		f := newFakeAPI(t)
		f.nodeLabels = map[string]string{staleLabelKey: staleVIPAddr}
		f.services = []v1.Service{*staleService()}
		f.leases["kube-system/kubevip-traefik"] = holderLease("kubevip-traefik", staleNode)

		p, rec := staleProcessor(t, f, staleConfig())
		if err := p.reconcileStaleServiceState(context.Background(), true); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		assertReleased(t, rec, f, nil, 0)
	})

	t.Run("keeps everything when the API is unreachable", func(t *testing.T) {
		f := newFakeAPI(t)
		f.nodeLabels = map[string]string{staleLabelKey: staleVIPAddr}
		f.services = []v1.Service{*staleService()}
		f.leases["kube-system/kubevip-traefik"] = holderLease("kubevip-traefik", stalePeer)

		p, rec := staleProcessor(t, f, staleConfig())
		f.server.Close() // API down before the reconcile gets to look
		if err := p.reconcileStaleServiceState(context.Background(), true); err == nil {
			t.Fatal("expected the reconcile to report the failed node read")
		}
		assertReleased(t, rec, f, nil, 0)
	})

	t.Run("treats a missing lease object as unowned", func(t *testing.T) {
		f := newFakeAPI(t)
		f.nodeLabels = map[string]string{staleLabelKey: staleVIPAddr}
		f.services = []v1.Service{*staleService()}
		// no lease entry: 404

		p, rec := staleProcessor(t, f, staleConfig())
		if err := p.reconcileStaleServiceState(context.Background(), true); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		assertReleased(t, rec, f, []string{"lo/" + staleVIPAddr}, 1)
	})

	t.Run("releases bindings in deployments without node labels", func(t *testing.T) {
		f := newFakeAPI(t)
		f.services = []v1.Service{*staleService()}
		f.leases["kube-system/kubevip-traefik"] = holderLease("kubevip-traefik", stalePeer)

		cfg := staleConfig()
		cfg.EnableNodeLabeling = false // enable_node_labeling=false deployments
		p, rec := staleProcessor(t, f, cfg)
		if err := p.reconcileStaleServiceState(context.Background(), true); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		assertReleased(t, rec, f, []string{"lo/" + staleVIPAddr}, 0)
	})

	t.Run("skips services outside the watcher filter", func(t *testing.T) {
		cases := []struct {
			name   string
			mutate func(svc *v1.Service)
		}{
			{
				name:   "not a load balancer",
				mutate: func(svc *v1.Service) { svc.Spec.Type = v1.ServiceTypeClusterIP },
			},
			{
				name:   "ignored",
				mutate: func(svc *v1.Service) { svc.Annotations[kubevip.LoadbalancerIgnore] = "true" },
			},
			{
				name:   "foreign load balancer class",
				mutate: func(svc *v1.Service) { class := "some-other-vendor"; svc.Spec.LoadBalancerClass = &class },
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				f := newFakeAPI(t)
				svc := staleService()
				tc.mutate(svc)
				f.services = []v1.Service{*svc}
				f.leases["kube-system/kubevip-traefik"] = holderLease("kubevip-traefik", stalePeer)

				p, rec := staleProcessor(t, f, staleConfig())
				if err := p.reconcileStaleServiceState(context.Background(), true); err != nil {
					t.Fatalf("reconcile: %v", err)
				}
				assertReleased(t, rec, f, nil, 0)
				_, _, leaseGets, _ := f.snapshot()
				if leaseGets != 0 {
					t.Fatalf("filtered service still reached the lease check: %d lease gets", leaseGets)
				}
			})
		}
	})

	t.Run("never releases what this process leads", func(t *testing.T) {
		t.Run("provided instance", func(t *testing.T) {
			f := newFakeAPI(t)
			f.nodeLabels = map[string]string{staleLabelKey: staleVIPAddr}
			f.services = []v1.Service{*staleService()}
			f.leases["kube-system/kubevip-traefik"] = holderLease("kubevip-traefik", stalePeer)

			p, rec := staleProcessor(t, f, staleConfig())
			svc := staleService()
			p.ServiceInstances = []*instance.Instance{
				{ServiceUID: svc.UID, ServiceSnapshot: svc, AddCalled: true},
			}
			if err := p.reconcileStaleServiceState(context.Background(), true); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			assertReleased(t, rec, f, nil, 0)
		})

		t.Run("elected lease", func(t *testing.T) {
			f := newFakeAPI(t)
			f.nodeLabels = map[string]string{staleLabelKey: staleVIPAddr}
			f.services = []v1.Service{*staleService()}
			f.leases["kube-system/kubevip-traefik"] = holderLease("kubevip-traefik", stalePeer)

			p, rec := staleProcessor(t, f, staleConfig())
			ns, name := lease.ServiceName(staleService())
			svcLease := p.leaseMgr.Add(context.Background(), lease.NewID(p.config.LeaderElectionType, ns, name))
			svcLease.Elected.Store(true)

			if err := p.reconcileStaleServiceState(context.Background(), true); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			assertReleased(t, rec, f, nil, 0)
			_, _, leaseGets, _ := f.snapshot()
			if leaseGets != 0 {
				t.Fatalf("locally elected lease still reached the API: %d lease gets", leaseGets)
			}
		})
	})

	t.Run("follower-registered instance does not block healing", func(t *testing.T) {
		// The watcher registers an instance on every node, leader or not.
		// Treating that registration as ownership would disable the reconcile
		// everywhere (#1775's healing exists precisely for non-leader nodes).
		f := newFakeAPI(t)
		f.nodeLabels = map[string]string{staleLabelKey: staleVIPAddr}
		f.services = []v1.Service{*staleService()}
		f.leases["kube-system/kubevip-traefik"] = holderLease("kubevip-traefik", stalePeer)

		p, rec := staleProcessor(t, f, staleConfig())
		svc := staleService()
		p.ServiceInstances = []*instance.Instance{
			{ServiceUID: svc.UID, ServiceSnapshot: svc}, // never AddCalled/LabelAdded
		}
		if err := p.reconcileStaleServiceState(context.Background(), true); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		assertReleased(t, rec, f, []string{"lo/" + staleVIPAddr}, 1)
	})

	t.Run("keeps VIP shared with a locally provided service", func(t *testing.T) {
		// netbird provides the same address here; releasing it because
		// traefik's lease went stale would black-hole netbird, the exact
		// failure mode this PR removes. deleteService guards shared VIPs the
		// same way.
		f := newFakeAPI(t)
		f.nodeLabels = map[string]string{staleLabelKey: staleVIPAddr}
		shared := staleOtherService()
		shared.Annotations[kubevip.LoadbalancerIPAnnotation] = staleVIPAddr
		f.services = []v1.Service{*staleService(), *shared}
		f.leases["kube-system/kubevip-traefik"] = holderLease("kubevip-traefik", stalePeer)
		f.leases["kube-system/kubevip-netbird"] = holderLease("kubevip-netbird", staleNode)

		p, rec := staleProcessor(t, f, staleConfig())
		p.ServiceInstances = []*instance.Instance{
			{ServiceUID: shared.UID, ServiceSnapshot: shared, AddCalled: true, LabelAdded: true},
		}
		if err := p.reconcileStaleServiceState(context.Background(), true); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		// The shared address stays; traefik's stale label still gets removed.
		assertReleased(t, rec, f, nil, 1)
	})
}

func TestReconcileStaleServiceStateLabelPass(t *testing.T) {
	t.Run("vanished service drops label and recorded VIP", func(t *testing.T) {
		f := newFakeAPI(t)
		f.nodeLabels = map[string]string{staleLabelKey: staleVIPAddr}
		// no services: the Get 404s

		p, rec := staleProcessor(t, f, staleConfig())
		if err := p.reconcileStaleServiceState(context.Background(), false); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		assertReleased(t, rec, f, []string{"lo/" + staleVIPAddr}, 1)
	})

	t.Run("label for a lease held elsewhere is dropped", func(t *testing.T) {
		f := newFakeAPI(t)
		f.nodeLabels = map[string]string{staleLabelKey: staleVIPAddr}
		f.services = []v1.Service{*staleService()}
		f.leases["kube-system/kubevip-traefik"] = holderLease("kubevip-traefik", stalePeer)

		p, rec := staleProcessor(t, f, staleConfig())
		if err := p.reconcileStaleServiceState(context.Background(), false); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		assertReleased(t, rec, f, []string{"lo/" + staleVIPAddr}, 1)
	})

	t.Run("label for own lease is kept", func(t *testing.T) {
		f := newFakeAPI(t)
		f.nodeLabels = map[string]string{staleLabelKey: staleVIPAddr}
		f.services = []v1.Service{*staleService()}
		f.leases["kube-system/kubevip-traefik"] = holderLease("kubevip-traefik", staleNode)

		p, rec := staleProcessor(t, f, staleConfig())
		if err := p.reconcileStaleServiceState(context.Background(), false); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		assertReleased(t, rec, f, nil, 0)
	})

	t.Run("malformed label key is tolerated", func(t *testing.T) {
		f := newFakeAPI(t)
		f.nodeLabels = map[string]string{"service-provided.kube-vip.io/not-a-name": "x"}

		p, rec := staleProcessor(t, f, staleConfig())
		if err := p.reconcileStaleServiceState(context.Background(), false); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		assertReleased(t, rec, f, nil, 0)
	})

	t.Run("a failing label patch does not stop other releases", func(t *testing.T) {
		f := newFakeAPI(t)
		f.nodeLabels = map[string]string{
			staleLabelKey:   staleVIPAddr,
			staleOtherLabel: staleOtherVIP,
		}
		f.services = []v1.Service{*staleService(), *staleOtherService()}
		f.leases["kube-system/kubevip-traefik"] = holderLease("kubevip-traefik", stalePeer)
		f.leases["kube-system/kubevip-netbird"] = holderLease("kubevip-netbird", stalePeer)
		f.failPatches = true // every node patch errors; the VIP release must still happen

		p, rec := staleProcessor(t, f, staleConfig())
		if err := p.reconcileStaleServiceState(context.Background(), false); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		got := rec.snapshot()
		want := []string{"lo/" + staleOtherVIP, "lo/" + staleVIPAddr}
		sort.Strings(want)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("VIP releases = %v, want %v", got, want)
		}
		patches, _, _, _ := f.snapshot()
		if len(patches) < 2 {
			t.Fatalf("expected both stale labels to be attempted, got %d patches", len(patches))
		}
	})
}

func TestStartStaleStateReconcileGuards(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(cfg *kubevip.Config)
	}{
		{
			name:   "no services election",
			mutate: func(cfg *kubevip.Config) { cfg.EnableServicesElection = false },
		},
		{
			name:   "unknown node name",
			mutate: func(cfg *kubevip.Config) { cfg.NodeName = "" },
		},
		{
			name:   "etcd-backed leases have no holder to check",
			mutate: func(cfg *kubevip.Config) { cfg.LeaderElectionType = "etcd" },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeAPI(t)
			cfg := staleConfig()
			tc.mutate(cfg)
			p, _ := staleProcessor(t, f, cfg)
			p.startStaleStateReconcile(context.Background())
			time.Sleep(50 * time.Millisecond)
			_, nodeGets, _, listGets := f.snapshot()
			if nodeGets != 0 || listGets != 0 {
				t.Fatalf("guarded reconcile touched the API: %d node gets, %d service lists", nodeGets, listGets)
			}
		})
	}
}

func TestStartStaleStateReconcileRunsPeriodically(t *testing.T) {
	old := staleStateReconcileInterval
	staleStateReconcileInterval = 20 * time.Millisecond
	t.Cleanup(func() { staleStateReconcileInterval = old })

	f := newFakeAPI(t)
	f.nodeLabels = map[string]string{staleLabelKey: staleVIPAddr}
	f.services = []v1.Service{*staleService()}
	// Lease held by this node: every pass must run but change nothing.
	f.leases["kube-system/kubevip-traefik"] = holderLease("kubevip-traefik", staleNode)

	p, rec := staleProcessor(t, f, staleConfig())
	ctx, cancel := context.WithCancel(context.Background())

	p.startStaleStateReconcile(ctx)

	// The startup full pass runs asynchronously so lease lookups cannot delay
	// the watcher.
	waitFor(t, time.Second*10, func() bool {
		_, _, _, lists := f.snapshot()
		return lists >= 1
	}, "startup full pass to enumerate services")
	_, nodeGets, _, _ := f.snapshot()
	waitFor(t, time.Second*10, func() bool {
		_, gets, _, _ := f.snapshot()
		return gets >= nodeGets+3
	}, "periodic reconcile passes")

	cancel()
	time.Sleep(50 * time.Millisecond)
	_, before, _, _ := f.snapshot()
	time.Sleep(150 * time.Millisecond)
	_, after, _, _ := f.snapshot()
	if after-before > 1 {
		t.Fatalf("periodic reconcile kept running after the context was cancelled (%d extra passes)", after-before)
	}
	assertReleased(t, rec, f, nil, 0)
}

func TestSplitServiceProvidedLabelKey(t *testing.T) {
	cases := []struct {
		key      string
		wantName string
		wantNS   string
		wantOK   bool
	}{
		{"service-provided.kube-vip.io/traefik.kube-system", "traefik", "kube-system", true},
		{"service-provided.kube-vip.io/notthere", "", "", false},
		{"service-provided.kube-vip.io/.kube-system", "", "", false},
		{"service-provided.kube-vip.io/traefik.", "", "", false},
		{"other.kube-vip.io/traefik.kube-system", "", "", false}, // foreign prefix rejected
	}
	for _, tc := range cases {
		name, ns, ok := splitServiceProvidedLabelKey(tc.key)
		if name != tc.wantName || ns != tc.wantNS || ok != tc.wantOK {
			t.Errorf("splitServiceProvidedLabelKey(%q) = (%q, %q, %t), want (%q, %q, %t)",
				tc.key, name, ns, ok, tc.wantName, tc.wantNS, tc.wantOK)
		}
	}
}
