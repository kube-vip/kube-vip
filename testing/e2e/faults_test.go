//go:build e2e

package e2e

import (
	"context"
	"testing"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestPodManifestPaths(t *testing.T) {
	tests := []struct {
		name      string
		wantSrc   string
		wantDst   string
		wantError bool
	}{
		{name: "kube-apiserver.yaml", wantSrc: "/etc/kubernetes/manifests/kube-apiserver.yaml", wantDst: "/tmp/kube-apiserver.yaml"},
		{name: "", wantError: true},
		{name: "../manifest.yaml", wantError: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			src, dst, err := podManifestPaths(test.name)
			if (err != nil) != test.wantError {
				t.Fatalf("podManifestPaths() error = %v, wantError %t", err, test.wantError)
			}
			if src != test.wantSrc || dst != test.wantDst {
				t.Fatalf("podManifestPaths() = (%q, %q), want (%q, %q)", src, dst, test.wantSrc, test.wantDst)
			}
		})
	}
}

func TestNodeIsReady(t *testing.T) {
	if nodeIsReady(&corev1.Node{}) {
		t.Fatal("nodeIsReady() = true without a Ready condition")
	}
	node := &corev1.Node{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	if !nodeIsReady(node) {
		t.Fatal("nodeIsReady() = false for Ready=True")
	}
}

func TestStealLeaseRetriesConflicts(t *testing.T) {
	client := fake.NewClientset(&coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "plndr-cp-lock"}})
	conflicts := 1
	client.PrependReactor("update", "leases", func(k8stesting.Action) (bool, runtime.Object, error) {
		if conflicts == 0 {
			return false, nil, nil
		}
		conflicts--
		return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "coordination.k8s.io", Resource: "leases"}, "plndr-cp-lock", nil)
	})

	if err := StealLease(context.Background(), client, "kube-system", "plndr-cp-lock", "thief"); err != nil {
		t.Fatalf("StealLease() error = %v", err)
	}
	lease, err := client.CoordinationV1().Leases("kube-system").Get(context.Background(), "plndr-cp-lock", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != "thief" {
		t.Fatalf("holder = %v, want thief", lease.Spec.HolderIdentity)
	}
}
