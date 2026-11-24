package services

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func TestServiceSnapshotChangedBGPForwardingNode(t *testing.T) {
	original := &v1.Service{Spec: v1.ServiceSpec{LoadBalancerIP: "192.0.2.10"}}
	updated := original.DeepCopy()
	updated.Annotations = map[string]string{kubevip.BGPForwardingNode: "true"}
	if !serviceSnapshotChanged(original, updated) {
		t.Fatal("enabling forwarding-node mode must restart the endpoint watcher")
	}
	if !serviceSnapshotChanged(updated, original) {
		t.Fatal("disabling forwarding-node mode must restart the endpoint watcher")
	}
	if serviceSnapshotChanged(updated, updated.DeepCopy()) {
		t.Fatal("an unchanged forwarding-node Service must not restart")
	}
}

func TestServiceSnapshotChangedEndpointlessOptIn(t *testing.T) {
	original := &v1.Service{Spec: v1.ServiceSpec{ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeCluster}}
	optedIn := original.DeepCopy()
	optedIn.Annotations = map[string]string{kubevip.AllowReconcileWithoutEndpoints: "true"}
	if !serviceSnapshotChanged(original, optedIn) {
		t.Fatal("enabling endpointless reconciliation must restart the watcher")
	}
	if !serviceSnapshotChanged(optedIn, original) {
		t.Fatal("disabling endpointless reconciliation must restart the watcher")
	}
	stillOptedIn := optedIn.DeepCopy()
	stillOptedIn.Annotations[kubevip.AllowReconcileWithoutEndpoints] = "TRUE"
	if serviceSnapshotChanged(optedIn, stillOptedIn) {
		t.Fatal("unchanged endpointless opt-in must not restart the watcher")
	}
}

func TestServiceInterfaceChanged(t *testing.T) {
	for _, test := range []struct {
		name                       string
		oldInterface, newInterface string
		oldVLAN, newVLAN           string
		config                     kubevip.Config
		changed                    bool
	}{
		{name: "interface added", newInterface: "eth1", changed: true},
		{name: "interface removed", oldInterface: "eth1", changed: true},
		{name: "interface unchanged", oldInterface: "eth1", newInterface: "eth1"},
		{name: "explicit Service default added", newInterface: "eth1", config: kubevip.Config{Interface: "eth0", ServicesInterface: "eth1"}},
		{name: "explicit primary default removed", oldInterface: "eth0", config: kubevip.Config{Interface: "eth0"}},
		{name: "override differs from Service default", newInterface: "eth0", config: kubevip.Config{Interface: "eth0", ServicesInterface: "eth1"}, changed: true},
		{name: "VLAN changed", oldVLAN: "eth0.42", newVLAN: "eth0.43", changed: true},
		{name: "VLAN removed", oldVLAN: "eth0.42", newInterface: "eth1", changed: true},
		{name: "VLAN takes precedence", oldInterface: "eth1", newInterface: "eth2", oldVLAN: "eth0.42", newVLAN: "eth0.42"},
	} {
		t.Run(test.name, func(t *testing.T) {
			original := &v1.Service{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				kubevip.ServiceInterface: test.oldInterface,
				kubevip.ServiceVlan:      test.oldVLAN,
			}}}
			updated := original.DeepCopy()
			updated.Annotations[kubevip.ServiceInterface] = test.newInterface
			updated.Annotations[kubevip.ServiceVlan] = test.newVLAN
			processor := &Processor{config: &test.config}
			if got := processor.serviceInterfaceChanged(original, updated); got != test.changed {
				t.Fatalf("serviceInterfaceChanged() = %t, want %t", got, test.changed)
			}
		})
	}
}

func TestReconcileServiceInterfaceUpdate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = http.NewResponseController(w).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	client, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatalf("create Kubernetes client: %v", err)
	}

	for _, test := range []struct {
		name              string
		instancePresent   bool
		snapshotRefreshed bool
		teardownFails     bool
		wireGuard         bool
		vlan              string
	}{
		{name: "active Service", instancePresent: true},
		{name: "active Service with refreshed snapshot", instancePresent: true, snapshotRefreshed: true},
		{name: "retries after teardown failure", instancePresent: true, teardownFails: true},
		{name: "watching on standby"},
		{name: "WireGuard ignores interface edit", instancePresent: true, wireGuard: true},
		{name: "WireGuard ignores VLAN edit", instancePresent: true, wireGuard: true, vlan: "eth1.42"},
	} {
		t.Run(test.name, func(t *testing.T) {
			original := &v1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "example", Namespace: "default", UID: "service-uid", Annotations: map[string]string{
					kubevip.ServiceInterface: "eth0",
				}},
				Spec: v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer, LoadBalancerIP: "192.0.2.10"},
			}
			if test.snapshotRefreshed {
				delete(original.Annotations, kubevip.ServiceInterface)
			}
			updated := original.DeepCopy()
			if test.vlan != "" {
				updated.Annotations[kubevip.ServiceVlan] = test.vlan
			} else {
				updated.Annotations[kubevip.ServiceInterface] = "eth1"
			}

			labeler := &testLabeler{}
			processor := &Processor{
				config:           &kubevip.Config{EnableWireguard: test.wireGuard, DebounceTime: "0s"},
				serviceLock:      newTestServiceLocks(),
				lbClassFilter:    lbClassFilter,
				rwClientSet:      client,
				nodeLabelManager: labeler,
				instanceFactory: serviceInstanceFactoryFunc(func(_ context.Context, svc *v1.Service, _ *sync.WaitGroup) (*instance.Instance, error) {
					return &instance.Instance{ServiceUID: svc.UID, ServiceSnapshot: svc.DeepCopy()}, nil
				}),
			}
			initializeTestElectionCoordinators(processor)
			oldInstance := &instance.Instance{ServiceUID: original.UID, ServiceSnapshot: original}
			if test.snapshotRefreshed {
				oldInstance.ServiceSnapshot = updated.DeepCopy()
			}
			if test.instancePresent {
				processor.ServiceInstances = []*instance.Instance{oldInstance}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			var wg sync.WaitGroup
			defer func() {
				cancel()
				wg.Wait()
			}()
			oldContext := servicecontext.New(ctx)
			oldContext.StartWatching()
			oldContext.SetWatchedService(original)
			processor.svcMap.Store(original.UID, oldContext)
			wg.Go(func() {
				<-oldContext.Ctx.Done()
				oldContext.StopWatching()
			})

			callback := func(*servicecontext.Context, *v1.Service, *sync.WaitGroup) error { return nil }
			cancelWatcher := func(err error) { t.Errorf("endpoint watcher failed: %v", err) }
			event := watch.Event{Type: watch.Modified, Object: updated}
			if test.teardownFails {
				removeErr := errors.New("node label update failed")
				labeler.removeErr = removeErr
				oldInstance.LabelAdded = true
				if err := processor.Reconcile(ctx, event, callback, false, &wg, cancelWatcher); !errors.Is(err, removeErr) {
					t.Fatalf("Reconcile() error = %v, want %v", err, removeErr)
				}
				current, err := processor.getServiceContext(updated.UID)
				if err != nil || current != oldContext || processor.findServiceInstance(updated) != oldInstance {
					t.Fatal("failed teardown replaced the original Service context or instance")
				}
				labeler.removeErr = nil
			}
			if err := processor.Reconcile(ctx, event, callback, false, &wg, cancelWatcher); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			if test.teardownFails && oldInstance.LabelAdded {
				t.Fatal("successful label removal left the original instance marked as labeled")
			}
			current, err := processor.getServiceContext(updated.UID)
			if err != nil || current == nil || current.Ctx.Err() != nil {
				t.Fatalf("expected a live Service context, got %v, error %v", current, err)
			}
			currentInstance := processor.findServiceInstance(updated)
			if test.wireGuard {
				if current != oldContext || currentInstance != oldInstance {
					t.Fatal("interface annotation edit replaced the WireGuard Service instance or watcher")
				}
				return
			}
			if oldContext.Ctx.Err() == nil || current == oldContext {
				t.Fatal("Service interface edit left the original watcher running")
			}
			if currentInstance == nil || currentInstance == oldInstance || currentInstance.ServiceSnapshot.Annotations[kubevip.ServiceInterface] != "eth1" {
				t.Fatal("expected a replacement Service instance with interface eth1")
			}
		})
	}
}

func TestValidateBGPForwardingNode(t *testing.T) {
	for _, test := range []struct {
		name   string
		config kubevip.Config
		force  bool
		valid  bool
	}{
		{name: "BGP on all nodes", config: kubevip.Config{EnableBGP: true}, valid: true},
		{name: "BGP disabled"},
		{name: "ARP", config: kubevip.Config{EnableBGP: true, EnableARP: true}},
		{name: "routing table", config: kubevip.Config{EnableBGP: true, EnableRoutingTable: true}},
		{name: "WireGuard", config: kubevip.Config{EnableBGP: true, EnableWireguard: true}},
		{name: "global election", config: kubevip.Config{EnableBGP: true, KubernetesLeaderElection: kubevip.KubernetesLeaderElection{EnableLeaderElection: true}}},
		{name: "Service election", config: kubevip.Config{EnableBGP: true, EnableServicesElection: true}},
		{name: "forced election", config: kubevip.Config{EnableBGP: true, PerServiceElectionOnDemand: true}, force: true},
		{name: "global next-hop rewrite", config: kubevip.Config{EnableBGP: true, BGPConfig: kubevip.BGPConfig{MpbgpNexthop: "fixed"}}},
		{name: "peer next-hop rewrite", config: kubevip.Config{EnableBGP: true, BGPConfig: kubevip.BGPConfig{Peers: []kubevip.BGPPeer{{MpbgpNexthop: "fixed"}}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := &v1.Service{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{kubevip.BGPForwardingNode: "true"}}}
			if test.force {
				service.Annotations[kubevip.ForcePerServiceElection] = "true"
			}
			if err := validateBGPForwardingNode(&test.config, service); (err == nil) != test.valid {
				t.Fatalf("validation error = %v, valid = %t", err, test.valid)
			}
			delete(service.Annotations, kubevip.BGPForwardingNode)
			if err := validateBGPForwardingNode(&test.config, service); err != nil {
				t.Fatalf("existing Services must not be affected: %v", err)
			}
		})
	}
}

// TestChangedSinceWatched is a regression test for a node that has lost (or never won) the
// service election. Its instance has been removed from the manager, but its watchers still
// run with the Service they were started with. A traffic policy change must still be detected
// against that copy, otherwise the node keeps treating a Local service as Cluster and keeps
// campaigning for the lease without a local endpoint.
func TestChangedSinceWatched(t *testing.T) {
	processor := &Processor{config: &kubevip.Config{}}
	policy := v1.IPFamilyPolicySingleStack
	cluster := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "example", Namespace: "default", UID: "service-uid"},
		Spec: v1.ServiceSpec{
			Type:                  v1.ServiceTypeLoadBalancer,
			LoadBalancerIP:        "192.0.2.10",
			ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeCluster,
			IPFamilyPolicy:        &policy,
		},
	}
	local := cluster.DeepCopy()
	local.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyTypeLocal

	watchedCtx := func(svc *v1.Service) *servicecontext.Context {
		svcCtx := servicecontext.New(context.Background())
		svcCtx.SetWatchedService(svc)
		return svcCtx
	}

	tests := []struct {
		name     string
		instance *instance.Instance
		svcCtx   *servicecontext.Context
		svc      *v1.Service
		want     bool
	}{
		{
			name:     "instance present, policy changed",
			instance: &instance.Instance{ServiceSnapshot: cluster},
			svcCtx:   watchedCtx(cluster),
			svc:      local,
			want:     true,
		},
		{
			name:     "instance present, unchanged",
			instance: &instance.Instance{ServiceSnapshot: local},
			svcCtx:   watchedCtx(cluster),
			svc:      local,
			want:     false,
		},
		{
			name:   "instance removed, watcher started with old policy",
			svcCtx: watchedCtx(cluster),
			svc:    local,
			want:   true,
		},
		{
			name:   "instance removed, watcher already on current policy",
			svcCtx: watchedCtx(local),
			svc:    local,
			want:   false,
		},
		{
			name:   "no watcher started yet",
			svcCtx: servicecontext.New(context.Background()),
			svc:    local,
			want:   false,
		},
		{
			name: "no instance and no context",
			svc:  local,
			want: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := processor.changedSinceWatched(test.instance, test.svcCtx, test.svc); got != test.want {
				t.Fatalf("changedSinceWatched() = %v, want %v", got, test.want)
			}
		})
	}
}

// TestChangedSinceWatchedForcePerServiceElection covers a Service moving between
// the regular and the on-demand election watcher. The watcher the Service now
// matches finds the instance the other one started, so the change must be
// detected or the election never starts (or never stops) until kube-vip restarts.
func TestChangedSinceWatchedForcePerServiceElection(t *testing.T) {
	processor := &Processor{config: &kubevip.Config{}}
	regular := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "example", Namespace: "default", UID: "service-uid"},
		Spec: v1.ServiceSpec{
			Type:                  v1.ServiceTypeLoadBalancer,
			LoadBalancerIP:        "192.0.2.10",
			ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeLocal,
		},
	}
	forced := regular.DeepCopy()
	forced.Annotations = map[string]string{kubevip.ForcePerServiceElection: "true"}
	notForced := regular.DeepCopy()
	notForced.Annotations = map[string]string{kubevip.ForcePerServiceElection: "false"}

	tests := []struct {
		name         string
		watched, svc *v1.Service
		want         bool
	}{
		{name: "annotation added", watched: regular, svc: forced, want: true},
		{name: "annotation removed", watched: forced, svc: regular, want: true},
		{name: "annotation set to false", watched: forced, svc: notForced, want: true},
		{name: "false is the same as absent", watched: regular, svc: notForced, want: false},
		{name: "still forced", watched: forced, svc: forced.DeepCopy(), want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := processor.changedSinceWatched(&instance.Instance{ServiceSnapshot: test.watched}, nil, test.svc); got != test.want {
				t.Fatalf("changedSinceWatched() with instance = %v, want %v", got, test.want)
			}
			svcCtx := servicecontext.New(context.Background())
			svcCtx.SetWatchedService(test.watched)
			if got := processor.changedSinceWatched(nil, svcCtx, test.svc); got != test.want {
				t.Fatalf("changedSinceWatched() with watched Service = %v, want %v", got, test.want)
			}
		})
	}
}
