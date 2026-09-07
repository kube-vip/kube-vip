package instance

import (
	"sync"
	"testing"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
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
