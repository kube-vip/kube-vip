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
