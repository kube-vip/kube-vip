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

const testServiceLabelPrefix = kubevip.ServiceProvided + "/"

// nodeLabelTestAPI serves node "node-a" with labels, the Services of the
// "default" namespace as a list, and Leases by name. Any other request,
// including a per-Service GET, fails the test.
type nodeLabelTestAPI struct {
	labels   map[string]string
	services []*v1.Service
	leases   map[string]string // "<namespace>/<name>" to holder
	onList   func()
	onPatch  func()

	mu      sync.Mutex
	patches [][]string
}

func (api *nodeLabelTestAPI) serve(t *testing.T) *kubernetes.Clientset {
	t.Helper()
	node := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", Labels: api.labels}}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		var body any
		leaseKey, isLease := strings.CutPrefix(request.URL.Path, "/apis/coordination.k8s.io/v1/namespaces/")
		switch {
		case request.URL.Path == "/api/v1/nodes/node-a" && request.Method == http.MethodPatch:
			removed := removedNodeLabels(t, request)
			if api.onPatch != nil {
				api.onPatch()
			}
			api.mu.Lock()
			api.patches = append(api.patches, removed)
			api.mu.Unlock()
			body = node
		case request.URL.Path == "/api/v1/nodes/node-a":
			body = node
		case request.URL.Path == "/api/v1/namespaces/default/services":
			if api.onList != nil {
				api.onList()
			}
			list := &v1.ServiceList{}
			for _, service := range api.services {
				list.Items = append(list.Items, *service)
			}
			body = list
		case isLease:
			key := strings.Replace(leaseKey, "/leases/", "/", 1)
			holder, found := api.leases[key]
			if !found {
				writer.WriteHeader(http.StatusNotFound)
				body = &metav1.Status{TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
					Status: metav1.StatusFailure, Reason: metav1.StatusReasonNotFound, Code: http.StatusNotFound}
				break
			}
			namespace, name, _ := strings.Cut(key, "/")
			body = &coordinationv1.Lease{
				TypeMeta:   metav1.TypeMeta{Kind: "Lease", APIVersion: "coordination.k8s.io/v1"},
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Spec:       coordinationv1.LeaseSpec{HolderIdentity: &holder},
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
	return clientSet
}

func (api *nodeLabelTestAPI) removalPatches() [][]string {
	api.mu.Lock()
	defer api.mu.Unlock()
	patches := make([][]string, 0, len(api.patches))
	for _, patch := range api.patches {
		patch = slices.Clone(patch)
		slices.Sort(patch)
		patches = append(patches, patch)
	}
	return patches
}

// newNodeLabelTestProcessor returns a processor for the Services of the
// "default" namespace with load balancer class "kube-vip", served by api.
func newNodeLabelTestProcessor(t *testing.T, api *nodeLabelTestAPI) (*Processor, *labeler.Manager) {
	t.Helper()
	clientSet := api.serve(t)
	labelManager := labeler.NewManager("node-a", clientSet)
	return &Processor{
		config: &kubevip.Config{
			NodeName: "node-a", EnableNodeLabeling: true, ServiceNamespace: "default", LoadBalancerClassName: "kube-vip",
		},
		lbClassFilter:    lbClassFilter,
		serviceLock:      newTestServiceLocks(),
		clientSet:        clientSet,
		nodeLabelManager: labelManager,
	}, labelManager
}

func testLoadBalancer(name, class string) *v1.Service {
	return &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID(name)},
		Spec:       v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer, LoadBalancerClass: &class},
	}
}

func TestRemoveStaleNodeLabels(t *testing.T) {
	active := testLoadBalancer("active", "kube-vip")
	demoted := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "demoted", Namespace: "default", UID: "demoted"},
		Spec:       v1.ServiceSpec{Type: v1.ServiceTypeClusterIP},
	}
	services := []*v1.Service{
		active, testLoadBalancer("stopped", "kube-vip"), testLoadBalancer("follower", "kube-vip"),
		testLoadBalancer("foreign", "other"), demoted,
	}
	labels := map[string]string{
		testServiceLabelPrefix + "active.default":        "192.0.2.1",
		testServiceLabelPrefix + "stopped.default":       "192.0.2.2",
		testServiceLabelPrefix + "follower.default":      "192.0.2.3",
		testServiceLabelPrefix + "deleted.default":       "192.0.2.4",
		testServiceLabelPrefix + "demoted.default":       "192.0.2.5",
		testServiceLabelPrefix + "foreign.default":       "192.0.2.6",
		testServiceLabelPrefix + "elsewhere.kube-system": "192.0.2.7",
		kubevip.HasIP: "192.0.2.8",
	}
	wantRemoved := [][]string{{
		testServiceLabelPrefix + "deleted.default",
		testServiceLabelPrefix + "demoted.default",
		testServiceLabelPrefix + "follower.default",
		testServiceLabelPrefix + "stopped.default",
	}}
	api := &nodeLabelTestAPI{labels: labels, services: services}
	processor, labelManager := newNodeLabelTestProcessor(t, api)
	processor.claimNodeLabels(active.UID, generateLabelsFromService(active))

	if err := processor.RemoveStaleNodeLabels(context.Background(), labelManager); err != nil {
		t.Fatalf("RemoveStaleNodeLabels() error = %v", err)
	}
	if got := api.removalPatches(); !slices.EqualFunc(got, wantRemoved, slices.Equal[[]string]) {
		t.Fatalf("removal patches = %v, want %v", got, wantRemoved)
	}
}

func TestRecoverNodeLabels(t *testing.T) {
	api := &nodeLabelTestAPI{
		labels: map[string]string{
			testServiceLabelPrefix + "mine.default":        "192.0.2.1",
			testServiceLabelPrefix + "theirs.default":      "192.0.2.2",
			testServiceLabelPrefix + "deleted.default":     "192.0.2.3",
			testServiceLabelPrefix + "foreign.default":     "192.0.2.4",
			testServiceLabelPrefix + "outside.kube-system": "192.0.2.5",
			kubevip.HasIP:            "192.0.2.6",
			"kubernetes.io/hostname": "node-a",
		},
		services: []*v1.Service{
			testLoadBalancer("mine", "kube-vip"), testLoadBalancer("theirs", "kube-vip"),
			testLoadBalancer("foreign", "other"),
		},
		leases: map[string]string{
			"default/kubevip-mine":      "node-a",
			"default/kubevip-theirs":    "node-b",
			"kube-system/plndr-cp-lock": "node-b",
		},
	}
	processor, labelManager := newNodeLabelTestProcessor(t, api)
	processor.config.EnableServices = true
	processor.config.EnableServicesElection = true
	processor.config.EnableControlPlane = true
	processor.config.EnableLeaderElection = true
	processor.config.LeaseName = "kube-system/plndr-cp-lock"

	if err := processor.RecoverNodeLabels(context.Background(), labelManager); err != nil {
		t.Fatalf("RecoverNodeLabels() error = %v", err)
	}
	want := [][]string{{
		kubevip.HasIP,
		testServiceLabelPrefix + "deleted.default",
		testServiceLabelPrefix + "theirs.default",
	}}
	if got := api.removalPatches(); !slices.EqualFunc(got, want, slices.Equal[[]string]) {
		t.Fatalf("removal patches = %v, want %v", got, want)
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

func TestStaleNodeLabelSweepRespectsClaims(t *testing.T) {
	activating := testLoadBalancer("activating", "kube-vip")
	newAPI := func() *nodeLabelTestAPI {
		return &nodeLabelTestAPI{labels: map[string]string{
			testServiceLabelPrefix + "activating.default": "192.0.2.1",
			testServiceLabelPrefix + "deleted.default":    "192.0.2.2",
		}, services: []*v1.Service{activating}}
	}

	t.Run("label claimed after the stale check is kept", func(t *testing.T) {
		api := newAPI()
		processor, labelManager := newNodeLabelTestProcessor(t, api)
		api.onList = func() {
			processor.claimNodeLabels(activating.UID, generateLabelsFromService(activating))
		}

		if err := processor.RemoveStaleNodeLabels(context.Background(), labelManager); err != nil {
			t.Fatalf("RemoveStaleNodeLabels() error = %v", err)
		}
		want := [][]string{{testServiceLabelPrefix + "deleted.default"}}
		if got := api.removalPatches(); !slices.EqualFunc(got, want, slices.Equal[[]string]) {
			t.Fatalf("removal patches = %v, want %v", got, want)
		}
	})

	t.Run("claim waits for an in-flight removal", func(t *testing.T) {
		api := newAPI()
		processor, labelManager := newNodeLabelTestProcessor(t, api)
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
