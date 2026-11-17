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
			var patch []struct{ Op, Path string }
			if err := json.NewDecoder(request.Body).Decode(&patch); err != nil {
				t.Errorf("decode node patch: %v", err)
			}
			mu.Lock()
			for _, op := range patch {
				if op.Op == "remove" {
					removed = append(removed, strings.TrimPrefix(op.Path, "/metadata/labels/"))
				}
			}
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
		"service-provided.kube-vip.io~1deleted.default",
		"service-provided.kube-vip.io~1demoted.default",
		"service-provided.kube-vip.io~1follower.default",
		"service-provided.kube-vip.io~1stopped.default",
	}
	if !slices.Equal(removed, want) {
		t.Fatalf("removed labels = %v, want %v", removed, want)
	}
}
