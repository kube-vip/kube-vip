//go:build linux

package services

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	coordinationv1 "k8s.io/api/coordination/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const recoveryProtocol = 248

// requireNetworkNamespaces makes the privileged CI job fail instead of silently
// skipping when it cannot enter a network namespace.
var requireNetworkNamespaces = os.Getenv("KUBE_VIP_REQUIRE_NETNS") != ""

func TestRecoverServiceAddressesUsesLeaseHolderIdentity(t *testing.T) {
	for _, test := range []struct {
		name   string
		holder string
		remain bool
	}{
		{name: "current node retains address", holder: "node-a", remain: true},
		{name: "remote node removes address", holder: "node-b", remain: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()

			originalNamespace, err := netns.Get()
			if err != nil {
				t.Fatalf("getting current network namespace: %v", err)
			}
			defer originalNamespace.Close()
			testNamespace, err := netns.New()
			if err != nil {
				if requireNetworkNamespaces {
					t.Fatalf("creating isolated network namespace: %v", err)
				}
				t.Skipf("creating isolated network namespace: %v", err)
			}
			defer testNamespace.Close()
			defer func() {
				if err := netns.Set(originalNamespace); err != nil {
					t.Errorf("restoring network namespace: %v", err)
				}
			}()
			loopback, err := netlink.LinkByName("lo")
			if err != nil {
				t.Fatalf("getting loopback interface: %v", err)
			}
			if err := netlink.LinkSetUp(loopback); err != nil {
				t.Fatalf("bringing up loopback interface: %v", err)
			}

			link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "kvrecover0"}}
			if err := netlink.LinkAdd(link); err != nil {
				t.Fatalf("creating test interface: %v", err)
			}
			if err := netlink.LinkSetUp(link); err != nil {
				t.Fatalf("bringing test interface up: %v", err)
			}
			address, err := netlink.ParseAddr("192.0.2.10/32")
			if err != nil {
				t.Fatalf("parsing test address: %v", err)
			}
			address.Protocol = recoveryProtocol
			if err := netlink.AddrReplace(link, address); err != nil {
				t.Fatalf("adding tagged test address: %v", err)
			}

			clientSet := recoveryTestClient(t, test.holder)
			processor := &Processor{
				config: &kubevip.Config{
					EnableServicesElection: true,
					LeaderElectionType:     "kubernetes",
					NodeName:               "node-a",
					RoutingProtocol:        recoveryProtocol,
					ServiceNamespace:       "default",
				},
				clientSet:     clientSet,
				lbClassFilter: lbClassFilter,
			}
			if err := processor.RecoverAddresses(context.Background()); err != nil {
				t.Fatalf("recovering addresses: %v", err)
			}

			addresses, err := netlink.AddrList(link, netlink.FAMILY_V4)
			if err != nil {
				t.Fatalf("listing test addresses: %v", err)
			}
			found := false
			for _, configured := range addresses {
				if configured.IP.String() == "192.0.2.10" {
					found = true
				}
			}
			if found != test.remain {
				t.Fatalf("tagged address present = %t, want %t", found, test.remain)
			}
		})
	}
}

func TestServiceAddressRetainedUsesGlobalLeaseHolder(t *testing.T) {
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: "default"}}
	for _, test := range []struct {
		name   string
		holder string
		retain bool
	}{
		{name: "current node", holder: "node-a", retain: true},
		{name: "remote node", holder: "node-b", retain: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			processor := &Processor{
				config: &kubevip.Config{
					EnableARP:          true,
					NodeName:           "node-a",
					LeaderElectionType: "kubernetes",
					ServicesLeaseName:  "default/kubevip-service",
				},
				clientSet: recoveryTestClient(t, test.holder),
			}
			retained, err := processor.serviceAddressRetained(context.Background(), service, make(map[string]string))
			if err != nil {
				t.Fatalf("checking global lease ownership: %v", err)
			}
			if retained != test.retain {
				t.Fatalf("retained = %t, want %t", retained, test.retain)
			}
		})
	}
}

func TestRetainControlPlaneVIPsUsesLeaseHolder(t *testing.T) {
	for _, test := range []struct {
		name   string
		holder string
		retain bool
	}{
		{name: "current node", holder: "node-a", retain: true},
		{name: "remote node", holder: "node-b", retain: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			processor := &Processor{
				config: &kubevip.Config{
					EnableControlPlane: true,
					NodeName:           "node-a",
					LeaderElectionType: "kubernetes",
					VIP:                "2001:db8::10",
					KubernetesLeaderElection: kubevip.KubernetesLeaderElection{
						LeaseName: "default/kubevip-service",
					},
				},
				clientSet: recoveryTestClient(t, test.holder),
			}
			retained := make(map[string]struct{})
			canClean, err := processor.retainControlPlaneVIPs(context.Background(), make(map[string]string), retained)
			if err != nil {
				t.Fatalf("checking control-plane lease ownership: %v", err)
			}
			if !canClean {
				t.Fatal("known control-plane IP unexpectedly disabled recovery")
			}
			_, found := retained["2001:db8::10"]
			if found != test.retain {
				t.Fatalf("control-plane VIP retained = %t, want %t", found, test.retain)
			}
		})
	}
}

func TestRetainAnnotatedLeaseVIPsAcrossInstances(t *testing.T) {
	annotations, err := kubevip.WithLeaseVIPs(nil, "release_b", recoveryProtocol, []string{"192.0.2.20"})
	if err != nil {
		t.Fatalf("WithLeaseVIPs() error = %v", err)
	}
	holder := "node-a"
	clientSet := recoveryTestClientWithLeases(t, holder, []coordinationv1.Lease{{
		ObjectMeta: metav1.ObjectMeta{Name: "release-b", Namespace: "other", Annotations: annotations},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: &holder},
	}})
	processor := &Processor{
		config:    &kubevip.Config{NodeName: holder, RoutingProtocol: recoveryProtocol},
		clientSet: clientSet,
	}
	holders := make(map[string]string)
	retained := make(map[string]struct{})
	if err := processor.retainAnnotatedLeaseVIPs(context.Background(), holders, retained); err != nil {
		t.Fatalf("retainAnnotatedLeaseVIPs() error = %v", err)
	}
	if _, found := retained["192.0.2.20"]; !found {
		t.Fatal("locally held VIP from another kube-vip instance was not retained")
	}
	if holders["other/release-b"] != holder {
		t.Fatal("Lease holder cache was not populated from the ownership scan")
	}
}

func TestLeaseOwnershipCurrentRejectsExpiredLease(t *testing.T) {
	now := time.Unix(1_000, 0)
	duration := int32(15)
	renewed := metav1.NewMicroTime(now.Add(-time.Minute))
	resource := &coordinationv1.Lease{Spec: coordinationv1.LeaseSpec{
		RenewTime:            &renewed,
		LeaseDurationSeconds: &duration,
	}}
	if leaseOwnershipCurrent(resource, now) {
		t.Fatal("expired Lease ownership was treated as current")
	}
	renewed = metav1.NewMicroTime(now.Add(-time.Second))
	resource.Spec.RenewTime = &renewed
	if !leaseOwnershipCurrent(resource, now) {
		t.Fatal("unexpired Lease ownership was rejected")
	}
}

func TestRecoverAddressesRemainsRetryableForHostnameControlPlaneVIP(t *testing.T) {
	processor := &Processor{
		config: &kubevip.Config{
			EnableControlPlane: true,
			LeaderElectionType: "kubernetes",
			RoutingProtocol:    recoveryProtocol,
			ServiceNamespace:   "default",
			Address:            "api.example.test",
		},
		clientSet:     recoveryTestClient(t, "node-a"),
		lbClassFilter: lbClassFilter,
	}
	if err := processor.RecoverAddresses(context.Background()); err != nil {
		t.Fatalf("RecoverAddresses() error = %v", err)
	}
	if processor.recovered {
		t.Fatal("hostname-backed control-plane VIP disabled future recovery")
	}
}

func recoveryTestClient(t *testing.T, holder string) *kubernetes.Clientset {
	return recoveryTestClientWithLeases(t, holder, nil)
}

func recoveryTestClientWithLeases(t *testing.T, holder string, leases []coordinationv1.Lease) *kubernetes.Clientset {
	t.Helper()
	clientSet, err := kubernetes.NewForConfig(&rest.Config{
		Host: "https://recovery.test",
		Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			var object any
			switch request.URL.Path {
			case "/apis/coordination.k8s.io/v1/leases":
				object = &coordinationv1.LeaseList{Items: leases}
			case "/apis/coordination.k8s.io/v1/namespaces/default/leases":
				object = &coordinationv1.LeaseList{Items: leases}
			case "/api/v1/namespaces/default/services":
				object = &v1.ServiceList{Items: []v1.Service{{
					ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: "default"},
					Spec:       v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer, LoadBalancerIP: "192.0.2.10"},
				}}}
			case "/apis/coordination.k8s.io/v1/namespaces/default/leases/kubevip-service":
				object = &coordinationv1.Lease{
					ObjectMeta: metav1.ObjectMeta{Name: "kubevip-service", Namespace: "default"},
					Spec:       coordinationv1.LeaseSpec{HolderIdentity: &holder},
				}
			default:
				return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(bytes.NewReader(nil)), Request: request}, nil
			}
			body, err := json.Marshal(object)
			if err != nil {
				return nil, err
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(bytes.NewReader(body)),
				Request:    request,
			}, nil
		}),
	})
	if err != nil {
		t.Fatalf("creating Kubernetes client: %v", err)
	}
	return clientSet
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}
