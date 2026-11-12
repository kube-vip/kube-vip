package instance

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/kube-vip/kube-vip/pkg/arp"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/networkinterface"
	"github.com/kube-vip/kube-vip/pkg/route"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestUIDUsesImmutableServiceUID(t *testing.T) {
	serviceUID := types.UID("original-service")
	instance := &Instance{
		ServiceUID: serviceUID,
		ServiceSnapshot: &v1.Service{ObjectMeta: metav1.ObjectMeta{
			UID: types.UID("replacement-service"),
		}},
	}

	if got := instance.UID(); got != serviceUID {
		t.Fatalf("UID() = %q, want %q", got, serviceUID)
	}

	instance.ServiceUID = ""
	if got := instance.UID(); got != "" {
		t.Fatalf("UID() without ServiceUID = %q, want empty UID", got)
	}
}

func TestCleanupStateIsImmutable(t *testing.T) {
	original := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "service", Namespace: "default", Annotations: map[string]string{kubevip.ServiceLease: "shared"},
	}, Spec: v1.ServiceSpec{LoadBalancerIP: "192.0.2.10", ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeCluster}}
	immutableInfo := serviceCleanupInfo(original)
	instance := &Instance{
		ServiceSnapshot:  original,
		ServiceAddresses: []string{"192.0.2.10"},
		cleanupInfo:      &immutableInfo,
	}
	instance.ServiceSnapshot = &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "changed"}}

	cleanupInfo, ok := instance.CleanupInfo()
	if !ok || cleanupInfo.Namespace != "default" || cleanupInfo.Name != "service" || cleanupInfo.Lease != "shared" ||
		cleanupInfo.ExternalTrafficPolicy != v1.ServiceExternalTrafficPolicyTypeCluster {
		t.Fatalf("CleanupInfo() = %+v, %t, want creation-time Service policy", cleanupInfo, ok)
	}
}

func TestNewInstanceDHCPVendorClass(t *testing.T) {
	for _, test := range []struct {
		name        string
		vendorClass string
		wantErr     bool
	}{
		{name: "not annotated"},
		{name: "annotated", vendorClass: "kube-vip"},
		{name: "longest vendor class", vendorClass: strings.Repeat("a", 255)},
		{name: "vendor class too long", vendorClass: strings.Repeat("a", 256), wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := &kubevip.Config{Interface: "lo", VIPSubnet: "32", EnableBGP: true}
			annotations := map[string]string{kubevip.LoadbalancerIPAnnotation: "192.0.2.10"}
			if test.vendorClass != "" {
				annotations[kubevip.DHCPVendorClass] = test.vendorClass
			}
			svc := &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: "default", Annotations: annotations}}

			instance, err := NewInstance(context.Background(), svc, config,
				networkinterface.NewManager(), arp.NewManager(config), route.NewManager(), nil, &sync.WaitGroup{})
			if test.wantErr {
				if err == nil || !strings.Contains(err.Error(), kubevip.DHCPVendorClass) {
					t.Fatalf("NewInstance() error = %v, want an error about %q", err, kubevip.DHCPVendorClass)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewInstance() error = %v", err)
			}
			if instance.dhcpVendorClass != test.vendorClass {
				t.Fatalf("dhcpVendorClass = %q, want %q", instance.dhcpVendorClass, test.vendorClass)
			}
		})
	}
}

func TestTransferLinkAttachmentOwnershipConcurrent(t *testing.T) {
	target := &Instance{IsVLAN: true, VLANInterface: "eth0.42"}
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			if !transferLinkAttachmentOwnership("eth0.42", []*Instance{target}, true) {
				t.Error("transferLinkAttachmentOwnership() did not find target")
			}
		})
	}
	wg.Wait()
	if !target.vlanOwned.Load() {
		t.Fatal("target did not receive VLAN ownership")
	}
}
