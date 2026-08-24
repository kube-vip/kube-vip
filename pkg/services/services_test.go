package services

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
)

func TestConfigureServiceDoesNotOverwriteActiveEndpoint(t *testing.T) {
	const selectedEndpoint = "172.30.2.40"

	staleService := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-service", Namespace: "default", UID: "test-uid",
			Annotations: map[string]string{kubevip.Egress: "true", kubevip.ActiveEndpoint: ""},
		},
		Spec: v1.ServiceSpec{LoadBalancerIP: "10.114.44.149"},
	}
	currentService := staleService.DeepCopy()
	currentService.Annotations[kubevip.ActiveEndpoint] = selectedEndpoint
	currentService.ResourceVersion = "2"

	var mutex sync.Mutex
	updateRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mutex.Lock()
		defer mutex.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		switch request.Method {
		case http.MethodGet:
			if err := json.NewEncoder(writer).Encode(currentService); err != nil {
				t.Errorf("encode Service response: %v", err)
			}
		case http.MethodPut:
			updatedService := &v1.Service{}
			if err := json.NewDecoder(request.Body).Decode(updatedService); err != nil {
				http.Error(writer, err.Error(), http.StatusBadRequest)
				return
			}
			updateRequests++
			currentService = updatedService
			if err := json.NewEncoder(writer).Encode(currentService); err != nil {
				t.Errorf("encode updated Service response: %v", err)
			}
		default:
			http.Error(writer, "unexpected request", http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()

	clientSet, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatalf("create Kubernetes client: %v", err)
	}
	processor := &Processor{
		config: &kubevip.Config{
			DisableServiceUpdates:  true,
			EnableServicesElection: true,
		},
		clientSet: clientSet,
	}
	serviceInstance := &instance.Instance{ServiceSnapshot: staleService}
	if err := processor.configureService(context.Background(), serviceInstance, staleService, &sync.WaitGroup{}); err != nil {
		t.Fatalf("configureService returned error: %v", err)
	}

	mutex.Lock()
	defer mutex.Unlock()
	if updateRequests != 0 {
		t.Fatalf("configureService sent %d stale Service updates, want none", updateRequests)
	}
	if got := currentService.Annotations[kubevip.ActiveEndpoint]; got != selectedEndpoint {
		t.Fatalf("active endpoint = %q, want %q", got, selectedEndpoint)
	}
}

// Test_upnpLeaseDurationForService tests whether the default lease duration is used, and whether the annotation
// overrides it correctly.
//
// For simplicity, this table driven test does not cover all edge cases (passing in nil instance, nil service snapshot,
// nil annotations, etc).
func Test_upnpLeaseDurationForService(t *testing.T) {
	const annotation = "kube-vip.io/upnp-lease-duration" // Validating value of kubevip.UpnpLeaseDuration.
	tcs := []struct {
		name        string
		annotations map[string]string
		want        int // in seconds
	}{
		{
			name:        "No annotation uses default",
			annotations: map[string]string{},
			want:        3600,
		},
		{
			name: "Valid annotation overrides default",
			annotations: map[string]string{
				annotation: "2h",
			},
			want: 7200,
		},
		{
			name: "Valid short annotation overrides default",
			annotations: map[string]string{
				annotation: "30m",
			},
			want: 1800,
		},
		{
			name: "Invalid annotation uses default",
			annotations: map[string]string{
				annotation: "invalid-duration",
			},
			want: 3600,
		},
	}

	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			i := &instance.Instance{
				ServiceSnapshot: &v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Annotations: tc.annotations,
					},
				},
			}
			gotDuration := upnpLeaseDurationForService(i)
			got := int(gotDuration.Seconds())
			if got != tc.want {
				t.Errorf("upnpLeaseDurationForService(%+v) = %v, want %v", tc.annotations, got, tc.want)
			}
		})
	}
}
