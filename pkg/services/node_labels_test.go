package services

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/node/labeler"
	coordinationv1 "k8s.io/api/coordination/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func TestRemoveStaleNodeLabels(t *testing.T) {
	const prefix = kubevip.ServiceProvided + "/"
	ourClass := "kube-vip"
	otherClass := "other"
	newLB := func(name string, class *string) *v1.Service {
		return &v1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID(name)},
			Spec:       v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer, LoadBalancerClass: class},
		}
	}
	active := newLB("active", &ourClass)
	stopped := newLB("stopped", &ourClass)
	follower := newLB("follower", &ourClass)
	foreign := newLB("foreign", &otherClass)
	demoted := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "demoted", Namespace: "default", UID: "demoted"},
		Spec:       v1.ServiceSpec{Type: v1.ServiceTypeClusterIP},
	}
	services := map[string]*v1.Service{
		"active": active, "stopped": stopped, "follower": follower, "foreign": foreign, "demoted": demoted,
	}
	node := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", Labels: map[string]string{
		prefix + "active.default":        "192.0.2.1",
		prefix + "stopped.default":       "192.0.2.2",
		prefix + "follower.default":      "192.0.2.3",
		prefix + "deleted.default":       "192.0.2.4",
		prefix + "demoted.default":       "192.0.2.5",
		prefix + "foreign.default":       "192.0.2.6",
		prefix + "elsewhere.kube-system": "192.0.2.7",
		kubevip.HasIP:                    "192.0.2.8",
	}}}

	var mu sync.Mutex
	var removed []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		var body any
		switch name, isService := strings.CutPrefix(request.URL.Path, "/api/v1/namespaces/default/services/"); {
		case request.URL.Path == "/api/v1/nodes/node-a" && request.Method == http.MethodPatch:
			mu.Lock()
			removed = append(removed, removedNodeLabels(t, request)...)
			mu.Unlock()
			body = node
		case request.URL.Path == "/api/v1/nodes/node-a":
			body = node
		case isService && services[name] != nil:
			body = services[name]
		case isService:
			writer.WriteHeader(http.StatusNotFound)
			body = &metav1.Status{TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
				Status: metav1.StatusFailure, Reason: metav1.StatusReasonNotFound, Code: http.StatusNotFound}
		default:
			t.Errorf("unexpected request %s %s", request.Method, request.URL.Path)
			writer.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(writer).Encode(body)
	}))
	defer server.Close()

	clientSet, err := kubernetes.NewForConfig(&rest.Config{
		Host: server.URL, ContentConfig: rest.ContentConfig{ContentType: "application/json"},
	})
	if err != nil {
		t.Fatalf("create Kubernetes client: %v", err)
	}

	labelManager := labeler.NewManager("node-a", clientSet)
	processor := &Processor{
		config: &kubevip.Config{
			EnableNodeLabeling: true, ServiceNamespace: "default", LoadBalancerClassName: ourClass,
		},
		lbClassFilter: lbClassFilter,
		serviceLock:   newTestServiceLocks(),
		clientSet:     clientSet,
		ServiceInstances: []*instance.Instance{
			{ServiceUID: active.UID, ServiceSnapshot: active, AddCalled: true, LabelAdded: true},
			{ServiceUID: stopped.UID, ServiceSnapshot: stopped, LabelAdded: true},
		},
		nodeLabelManager: labelManager,
	}
	processor.claimNodeLabels(active.UID, generateLabelsFromService(active))

	if err := processor.RemoveStaleNodeLabels(context.Background(), labelManager); err != nil {
		t.Fatalf("RemoveStaleNodeLabels() error = %v", err)
	}
	slices.Sort(removed)
	want := []string{
		kubevip.ServiceProvided + "/deleted.default",
		kubevip.ServiceProvided + "/demoted.default",
		kubevip.ServiceProvided + "/follower.default",
		kubevip.ServiceProvided + "/stopped.default",
	}
	if !slices.Equal(removed, want) {
		t.Fatalf("removed labels = %v, want %v", removed, want)
	}
}

func TestRecoverNodeLabels(t *testing.T) {
	const nodeName = "node-a"
	const prefix = kubevip.ServiceProvided + "/"
	ourClass := "kube-vip"
	otherClass := "other"
	newLB := func(name string, class *string) *v1.Service {
		return &v1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID(name)},
			Spec:       v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer, LoadBalancerClass: class},
		}
	}
	services := map[string]*v1.Service{
		"mine": newLB("mine", &ourClass), "theirs": newLB("theirs", &ourClass), "foreign": newLB("foreign", &otherClass),
	}
	leaseHolders := map[string]string{
		"default/kubevip-mine":      nodeName,
		"default/kubevip-theirs":    "node-b",
		"kube-system/plndr-cp-lock": "node-b",
	}
	node := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName, Labels: map[string]string{
		prefix + "mine.default":        "192.0.2.1",
		prefix + "theirs.default":      "192.0.2.2",
		prefix + "deleted.default":     "192.0.2.3",
		prefix + "foreign.default":     "192.0.2.4",
		prefix + "outside.kube-system": "192.0.2.5",
		kubevip.HasIP:                  "192.0.2.6",
		"kubernetes.io/hostname":       nodeName,
	}}}

	var mu sync.Mutex
	var patches [][]string
	notFound := &metav1.Status{TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
		Status: metav1.StatusFailure, Reason: metav1.StatusReasonNotFound, Code: http.StatusNotFound}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		var body any = notFound
		serviceName, isService := strings.CutPrefix(request.URL.Path, "/api/v1/namespaces/default/services/")
		leaseKey, isLease := strings.CutPrefix(request.URL.Path, "/apis/coordination.k8s.io/v1/namespaces/")
		leaseKey = strings.Replace(leaseKey, "/leases/", "/", 1)
		switch {
		case request.URL.Path == "/api/v1/nodes/"+nodeName && request.Method == http.MethodPatch:
			removed := removedNodeLabels(t, request)
			mu.Lock()
			patches = append(patches, removed)
			mu.Unlock()
			body = node
		case request.URL.Path == "/api/v1/nodes/"+nodeName:
			body = node
		case isService && services[serviceName] != nil:
			body = services[serviceName]
		case isLease && leaseHolders[leaseKey] != "":
			holder := leaseHolders[leaseKey]
			namespace, name, _ := strings.Cut(leaseKey, "/")
			body = &coordinationv1.Lease{
				TypeMeta:   metav1.TypeMeta{Kind: "Lease", APIVersion: "coordination.k8s.io/v1"},
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Spec:       coordinationv1.LeaseSpec{HolderIdentity: &holder},
			}
		case isService || isLease:
			writer.WriteHeader(http.StatusNotFound)
		default:
			t.Errorf("unexpected request %s %s", request.Method, request.URL.Path)
			writer.WriteHeader(http.StatusInternalServerError)
			return
		}
		if err := json.NewEncoder(writer).Encode(body); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer server.Close()

	clientSet, err := kubernetes.NewForConfig(&rest.Config{
		Host: server.URL, ContentConfig: rest.ContentConfig{ContentType: "application/json"},
	})
	if err != nil {
		t.Fatalf("create Kubernetes client: %v", err)
	}
	processor := &Processor{
		config: &kubevip.Config{
			NodeName: nodeName, EnableNodeLabeling: true, EnableServices: true, EnableServicesElection: true,
			EnableControlPlane: true, EnableLeaderElection: true, LeaseName: "kube-system/plndr-cp-lock",
			ServiceNamespace: "default", LoadBalancerClassName: ourClass,
		},
		lbClassFilter: lbClassFilter,
		serviceLock:   newTestServiceLocks(),
		clientSet:     clientSet,
	}

	if err := processor.RecoverNodeLabels(context.Background(), labeler.NewManager(nodeName, clientSet)); err != nil {
		t.Fatalf("RecoverNodeLabels() error = %v", err)
	}
	if len(patches) != 1 {
		t.Fatalf("node patches = %d, want a single batched patch", len(patches))
	}
	removed := patches[0]
	slices.Sort(removed)
	want := []string{
		kubevip.HasIP,
		kubevip.ServiceProvided + "/deleted.default",
		kubevip.ServiceProvided + "/theirs.default",
	}
	if !slices.Equal(removed, want) {
		t.Fatalf("removed labels = %v, want %v", removed, want)
	}
}

// removedNodeLabels decodes a node label merge patch and returns the keys it
// removes.
func removedNodeLabels(t *testing.T, request *http.Request) []string {
	t.Helper()
	var patch struct {
		Metadata struct {
			Labels map[string]*string `json:"labels"`
		} `json:"metadata"`
	}
	if err := json.NewDecoder(request.Body).Decode(&patch); err != nil {
		t.Errorf("decode node patch: %v", err)
	}
	var removed []string
	for key, value := range patch.Metadata.Labels {
		if value == nil {
			removed = append(removed, key)
		}
	}
	return removed
}

// claimTestAPI serves the node and its Services for the stale label sweep and
// lets a test act while a Service is looked up or the removal patch is sent.
type claimTestAPI struct {
	onLookup func(name string)
	onPatch  func()

	mu      sync.Mutex
	patches [][]string
}

func (api *claimTestAPI) removalPatches() [][]string {
	api.mu.Lock()
	defer api.mu.Unlock()
	return slices.Clone(api.patches)
}

func newClaimTestProcessor(t *testing.T, api *claimTestAPI, services map[string]*v1.Service,
	labels map[string]string) (*Processor, *labeler.Manager) {
	t.Helper()
	node := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", Labels: labels}}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		var body any = node
		name, isService := strings.CutPrefix(request.URL.Path, "/api/v1/namespaces/default/services/")
		switch {
		case request.URL.Path == "/api/v1/nodes/node-a" && request.Method == http.MethodPatch:
			removed := removedNodeLabels(t, request)
			if api.onPatch != nil {
				api.onPatch()
			}
			api.mu.Lock()
			api.patches = append(api.patches, removed)
			api.mu.Unlock()
		case request.URL.Path == "/api/v1/nodes/node-a":
		case isService:
			if api.onLookup != nil {
				api.onLookup(name)
			}
			if services[name] == nil {
				writer.WriteHeader(http.StatusNotFound)
				body = &metav1.Status{TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
					Status: metav1.StatusFailure, Reason: metav1.StatusReasonNotFound, Code: http.StatusNotFound}
			} else {
				body = services[name]
			}
		default:
			t.Errorf("unexpected request %s %s", request.Method, request.URL.Path)
			writer.WriteHeader(http.StatusInternalServerError)
			return
		}
		if err := json.NewEncoder(writer).Encode(body); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	clientSet, err := kubernetes.NewForConfig(&rest.Config{
		Host: server.URL, ContentConfig: rest.ContentConfig{ContentType: "application/json"},
	})
	if err != nil {
		t.Fatalf("create Kubernetes client: %v", err)
	}
	labelManager := labeler.NewManager("node-a", clientSet)
	return &Processor{
		config: &kubevip.Config{
			EnableNodeLabeling: true, ServiceNamespace: "default", LoadBalancerClassName: "kube-vip",
		},
		lbClassFilter:    lbClassFilter,
		serviceLock:      newTestServiceLocks(),
		clientSet:        clientSet,
		nodeLabelManager: labelManager,
	}, labelManager
}

func TestStaleNodeLabelSweepRespectsClaims(t *testing.T) {
	const prefix = kubevip.ServiceProvided + "/"
	class := "kube-vip"
	activating := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "activating", Namespace: "default", UID: "activating"},
		Spec:       v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer, LoadBalancerClass: &class},
	}
	services := map[string]*v1.Service{"activating": activating}
	labels := map[string]string{
		prefix + "activating.default": "192.0.2.1",
		prefix + "deleted.default":    "192.0.2.2",
	}

	t.Run("label claimed after the stale check is kept", func(t *testing.T) {
		api := &claimTestAPI{}
		processor, labelManager := newClaimTestProcessor(t, api, services, labels)
		api.onLookup = func(name string) {
			if name == activating.Name {
				processor.claimNodeLabels(activating.UID, generateLabelsFromService(activating))
			}
		}

		if err := processor.RemoveStaleNodeLabels(context.Background(), labelManager); err != nil {
			t.Fatalf("RemoveStaleNodeLabels() error = %v", err)
		}
		want := [][]string{{prefix + "deleted.default"}}
		if got := api.removalPatches(); !slices.EqualFunc(got, want, slices.Equal[[]string]) {
			t.Fatalf("removal patches = %v, want %v", got, want)
		}
	})

	t.Run("claim waits for an in-flight removal", func(t *testing.T) {
		api := &claimTestAPI{}
		processor, labelManager := newClaimTestProcessor(t, api, services, labels)
		patching := make(chan struct{})
		release := make(chan struct{})
		var releaseOnce sync.Once
		// Registered after the server, so it runs before the server waits for
		// the blocked patch handler.
		t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
		api.onPatch = func() {
			close(patching)
			<-release
		}

		swept := make(chan error, 1)
		go func() { swept <- processor.RemoveStaleNodeLabels(context.Background(), labelManager) }()
		select {
		case <-patching:
		case <-time.After(time.Second):
			t.Fatal("stale label sweep did not send its removal patch")
		}

		claimed := make(chan struct{})
		go func() {
			processor.claimNodeLabels(activating.UID, generateLabelsFromService(activating))
			close(claimed)
		}()
		select {
		case <-claimed:
			t.Fatal("label claim did not wait for the in-flight removal patch")
		case <-time.After(50 * time.Millisecond):
		}

		releaseOnce.Do(func() { close(release) })
		select {
		case <-claimed:
		case <-time.After(time.Second):
			t.Fatal("label claim stayed blocked after the removal patch completed")
		}
		if err := <-swept; err != nil {
			t.Fatalf("RemoveStaleNodeLabels() error = %v", err)
		}
	})
}

func nodeLabelClaimed(p *Processor, uid types.UID) bool {
	p.nodeLabelMu.Lock()
	defer p.nodeLabelMu.Unlock()
	_, claimed := p.nodeLabelClaims[uid]
	return claimed
}

func TestNodeLabelClaimFollowsServiceLifecycle(t *testing.T) {
	addClaimedService := func(t *testing.T) (*Processor, *v1.Service) {
		t.Helper()
		service := admissionTestService("claimed", "192.0.2.10")
		processor := &Processor{
			serviceLock:      newTestServiceLocks(),
			config:           &kubevip.Config{DisableServiceUpdates: true, EnableServicesElection: true},
			ServiceInstances: []*instance.Instance{{ServiceUID: service.UID, ServiceSnapshot: service}},
			nodeLabelManager: &testLabeler{},
		}
		initializeTestElectionCoordinators(processor)
		svcCtx := publishTestServiceContext(processor, service)
		if err := processor.addService(context.Background(), svcCtx, service, &sync.WaitGroup{}); err != nil {
			t.Fatalf("addService() error = %v", err)
		}
		if !nodeLabelClaimed(processor, service.UID) {
			t.Fatal("configured Service did not claim its node label")
		}
		return processor, service
	}

	t.Run("deletion releases the claim", func(t *testing.T) {
		processor, service := addClaimedService(t)
		if err := processor.deleteService(context.Background(), service.UID, nil); err != nil {
			t.Fatalf("deleteService() error = %v", err)
		}
		if nodeLabelClaimed(processor, service.UID) {
			t.Fatal("deleted Service kept its node label claim")
		}
	})

	t.Run("stopping Services releases the claim", func(t *testing.T) {
		processor, service := addClaimedService(t)
		processor.Stop()
		if nodeLabelClaimed(processor, service.UID) {
			t.Fatal("stopped Service kept its node label claim")
		}
	})
}
