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

func TestRecoverAddressesUsesMixedElectionLeaseOwnership(t *testing.T) {
	for _, test := range []struct {
		name   string
		holder string
		remain bool
	}{
		{name: "local per-service lease retains address", holder: "node-a", remain: true},
		{name: "remote per-service lease removes address", holder: "node-b", remain: false},
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
				if os.Getenv("KUBE_VIP_REQUIRE_NETNS") != "" {
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

			link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "kvrecover0"}}
			if err := netlink.LinkAdd(link); err != nil {
				t.Fatalf("creating test interface: %v", err)
			}
			address, err := netlink.ParseAddr("192.0.2.10/32")
			if err != nil {
				t.Fatalf("parsing test address: %v", err)
			}
			address.Protocol = recoveryProtocol
			if err := netlink.AddrReplace(link, address); err != nil {
				t.Fatalf("adding tagged test address: %v", err)
			}

			processor := &Processor{
				config: &kubevip.Config{
					EnableARP:                  true,
					PerServiceElectionOnDemand: true,
					LeaderElectionType:         "kubernetes",
					ServicesLeaseName:          "default/plndr-svcs-lock",
					NodeName:                   "node-a",
					RoutingProtocol:            recoveryProtocol,
					ServiceNamespace:           "default",
				},
				clientSet: mixedRecoveryClient(t, test.holder),
			}
			if err := processor.RecoverAddresses(context.Background()); err != nil {
				t.Fatalf("RecoverAddresses() error = %v", err)
			}
			addresses, err := netlink.AddrList(link, netlink.FAMILY_V4)
			if err != nil {
				t.Fatalf("listing test addresses: %v", err)
			}
			found := false
			for _, configured := range addresses {
				found = found || configured.IP.String() == "192.0.2.10"
			}
			if found != test.remain {
				t.Fatalf("tagged address present = %t, want %t", found, test.remain)
			}
		})
	}
}

func TestServiceRecoveryLeaseMixedMode(t *testing.T) {
	processor := &Processor{config: &kubevip.Config{PerServiceElectionOnDemand: true, ServicesLeaseName: "default/plndr-svcs-lock"}}
	for _, test := range []struct {
		name        string
		annotations map[string]string
		want        string
	}{
		{name: "forced", annotations: map[string]string{kubevip.ForcePerServiceElection: "true"}, want: "kubevip-service"},
		{name: "ordinary", annotations: map[string]string{}, want: "plndr-svcs-lock"},
		{name: "exact value", annotations: map[string]string{kubevip.ForcePerServiceElection: "True"}, want: "plndr-svcs-lock"},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: "default", Annotations: test.annotations}}
			namespace, name := processor.serviceRecoveryLease(service)
			if namespace != "default" || name != test.want {
				t.Fatalf("serviceRecoveryLease() = %s/%s, want default/%s", namespace, name, test.want)
			}
		})
	}
}

func mixedRecoveryClient(t *testing.T, holder string) *kubernetes.Clientset {
	t.Helper()
	clientSet, err := kubernetes.NewForConfig(&rest.Config{
		Host: "https://recovery.test",
		Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			var object any
			switch request.URL.Path {
			case "/api/v1/namespaces/default/services":
				object = &v1.ServiceList{Items: []v1.Service{{
					ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: "default", Annotations: map[string]string{kubevip.ForcePerServiceElection: "true"}},
					Spec:       v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer, LoadBalancerIP: "192.0.2.10"},
				}}}
			case "/apis/coordination.k8s.io/v1/namespaces/default/leases":
				object = &coordinationv1.LeaseList{}
			case "/apis/coordination.k8s.io/v1/namespaces/default/leases/kubevip-service":
				object = &coordinationv1.Lease{Spec: coordinationv1.LeaseSpec{HolderIdentity: &holder}}
			default:
				return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(bytes.NewReader(nil)), Request: request}, nil
			}
			body, err := json.Marshal(object)
			if err != nil {
				return nil, err
			}
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewReader(body)), Request: request}, nil
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
