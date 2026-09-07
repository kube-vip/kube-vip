package services

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/kube-vip/kube-vip/pkg/node/noop"
	"github.com/kube-vip/kube-vip/pkg/vip"
)

type testDHCPClient struct {
	ips    chan string
	errors chan error
}

func newTestDHCPClient() *testDHCPClient {
	return &testDHCPClient{ips: make(chan string, 1), errors: make(chan error)}
}

func (c *testDHCPClient) ErrorChannel() chan error { return c.errors }
func (c *testDHCPClient) IPChannel() chan string   { return c.ips }
func (c *testDHCPClient) Start(context.Context) error {
	return nil
}
func (c *testDHCPClient) Stop() {}
func (c *testDHCPClient) WithHostName(string) vip.DHCPClient {
	return c
}

func TestAddServiceDoesNotOverwriteActiveEndpoint(t *testing.T) {
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
			updateRequests++
			updatedService := &v1.Service{}
			if err := json.NewDecoder(request.Body).Decode(updatedService); err != nil {
				http.Error(writer, err.Error(), http.StatusBadRequest)
				return
			}
			currentService = updatedService
			if err := json.NewEncoder(writer).Encode(currentService); err != nil {
				t.Errorf("encode updated Service response: %v", err)
			}
		default:
			http.Error(writer, "unexpected request", http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()

	clientSet, err := kubernetes.NewForConfig(&rest.Config{
		Host: server.URL,
		ContentConfig: rest.ContentConfig{
			ContentType: "application/json",
		},
	})
	if err != nil {
		t.Fatalf("create Kubernetes client: %v", err)
	}
	processor := &Processor{
		config: &kubevip.Config{
			EnableServicesElection: true,
			EnableARP:              true,
			NodeName:               "test-node",
		},
		clientSet:        clientSet,
		nodeLabelManager: noop.NewManager(),
	}
	serviceInstance := &instance.Instance{ServiceUID: staleService.UID, ServiceSnapshot: staleService}
	processor.ServiceInstances = []*instance.Instance{serviceInstance}
	if err := processor.addService(context.Background(), staleService, &sync.WaitGroup{}); err != nil {
		t.Fatalf("addService returned error: %v", err)
	}

	mutex.Lock()
	defer mutex.Unlock()
	if updateRequests != 1 {
		t.Fatalf("addService sent %d Service updates, want 1", updateRequests)
	}
	if got := currentService.Annotations[kubevip.ActiveEndpoint]; got != selectedEndpoint {
		t.Fatalf("active endpoint = %q, want %q", got, selectedEndpoint)
	}
}

func TestConfigureServiceRejectsCancelledContext(t *testing.T) {
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "cancelled", Namespace: "default", UID: "cancelled",
	}}
	serviceInstance := &instance.Instance{ServiceUID: service.UID, ServiceSnapshot: service}
	processor := &Processor{
		config:           &kubevip.Config{EnableServicesElection: true},
		ServiceInstances: []*instance.Instance{serviceInstance},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := processor.configureService(ctx, serviceInstance, service, &sync.WaitGroup{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("configureService() error = %v, want context cancellation", err)
	}
}

func TestUpdateEgressConfigurationRejectsRecreatedService(t *testing.T) {
	trackedService := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-service", Namespace: "default", UID: "original-uid",
			Annotations: map[string]string{kubevip.ActiveEndpoint: "10.0.0.1"},
		},
	}
	updatedService := trackedService.DeepCopy()
	updatedService.Annotations[kubevip.ActiveEndpoint] = "10.0.0.2"
	recreatedService := updatedService.DeepCopy()
	recreatedService.UID = "replacement-uid"

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(writer).Encode(recreatedService); err != nil {
			t.Errorf("encode Service response: %v", err)
		}
	}))
	defer server.Close()

	clientSet, err := kubernetes.NewForConfig(&rest.Config{
		Host: server.URL,
		ContentConfig: rest.ContentConfig{
			ContentType: "application/json",
		},
	})
	if err != nil {
		t.Fatalf("create Kubernetes client: %v", err)
	}

	snapshot := trackedService.DeepCopy()
	serviceInstance := &instance.Instance{ServiceUID: trackedService.UID, ServiceSnapshot: snapshot}
	processor := &Processor{
		config:           &kubevip.Config{},
		clientSet:        clientSet,
		ServiceInstances: []*instance.Instance{serviceInstance},
	}

	if err := processor.updateEgressConfiguration(context.Background(), updatedService); err != nil {
		t.Fatalf("updateEgressConfiguration() error = %v", err)
	}
	if serviceInstance.ServiceSnapshot != snapshot {
		t.Fatal("recreated Service replaced the tracked instance snapshot")
	}
}

func TestConfigureServiceWatchesBothDHCPFamilies(t *testing.T) {
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "test-service", Namespace: "default", UID: "service-uid",
	}}
	dhcpv4 := newTestDHCPClient()
	dhcpv6 := newTestDHCPClient()
	serviceInstance := &instance.Instance{
		ServiceUID:      service.UID,
		ServiceSnapshot: service,
		VIPConfigs: []*kubevip.Config{
			{VIP: "0.0.0.0"},
			{VIP: "::"},
		},
		IsDHCPv4:     true,
		IsDHCPv6:     true,
		DHCPv4Client: dhcpv4,
		DHCPv6Client: dhcpv6,
	}
	processor := &Processor{
		config: &kubevip.Config{
			DisableServiceUpdates: true,
			EnableARP:             true,
			KubernetesLeaderElection: kubevip.KubernetesLeaderElection{
				EnableLeaderElection: true,
			},
		},
		ServiceInstances: []*instance.Instance{serviceInstance},
		nodeLabelManager: noop.NewManager(),
	}
	wg := &sync.WaitGroup{}

	if err := processor.configureService(context.Background(), serviceInstance, service, wg); err != nil {
		t.Fatalf("configureService() error = %v", err)
	}
	dhcpv4.ips <- "192.0.2.10"
	dhcpv6.ips <- "2001:db8::10"
	close(dhcpv4.ips)
	close(dhcpv6.ips)
	wg.Wait()

	if got := serviceInstance.DHCPInterfaceIPv4; got != "192.0.2.10" {
		t.Fatalf("DHCPInterfaceIPv4 = %q, want %q", got, "192.0.2.10")
	}
	if got := serviceInstance.DHCPInterfaceIPv6; got != "2001:db8::10" {
		t.Fatalf("DHCPInterfaceIPv6 = %q, want %q", got, "2001:db8::10")
	}
	if got := serviceInstance.VIPConfigs[0].VIP; got != "192.0.2.10" {
		t.Fatalf("IPv4 VIP config = %q, want %q", got, "192.0.2.10")
	}
	if got := serviceInstance.VIPConfigs[1].VIP; got != "2001:db8::10" {
		t.Fatalf("IPv6 VIP config = %q, want %q", got, "2001:db8::10")
	}
}

func TestServiceSnapshotForEgressUsesCurrentInstanceState(t *testing.T) {
	captured := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
			kubevip.ActiveEndpoint: "10.0.0.1",
			kubevip.EgressIPv6:     "true",
		}},
		Spec: v1.ServiceSpec{LoadBalancerIP: "192.0.2.10"},
	}
	current := captured.DeepCopy()
	current.Annotations[kubevip.ActiveEndpoint] = "10.0.0.2"
	current.Annotations[kubevip.ActiveEndpointIPv6] = "fd00::2"
	serviceInstance := &instance.Instance{ServiceSnapshot: current}

	got := serviceSnapshotForEgress(serviceInstance, captured)
	if got == captured || got == current {
		t.Fatal("egress configuration did not create an isolated merged Service")
	}
	if got.Annotations[kubevip.ActiveEndpoint] != "10.0.0.2" ||
		got.Annotations[kubevip.ActiveEndpointIPv6] != "fd00::2" {
		t.Fatalf("merged endpoints = %q, %q", got.Annotations[kubevip.ActiveEndpoint], got.Annotations[kubevip.ActiveEndpointIPv6])
	}
	if got.Annotations[kubevip.EgressIPv6] != "true" || got.Spec.LoadBalancerIP != "192.0.2.10" {
		t.Fatal("merged Service did not preserve current configuration")
	}
	if got := serviceSnapshotForEgress(nil, captured); got != captured {
		t.Fatal("egress configuration did not fall back to the captured Service")
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
