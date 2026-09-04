package election

import (
	"context"
	"testing"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

func TestAnnotatedLeaseLockPersistsAnnotationsOnCreateAndUpdate(t *testing.T) {
	client := fake.NewSimpleClientset()
	leaseClient := client.CoordinationV1().Leases("default")
	base := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{Name: "lease", Namespace: "default"},
		Client:    client.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: "node-a",
		},
	}
	annotations, err := kubevip.WithLeaseVIPs(map[string]string{"example.test/preserved": "true"},
		"release_a", 248, []string{"192.0.2.10"})
	if err != nil {
		t.Fatalf("WithLeaseVIPs() error = %v", err)
	}
	lock := newAnnotatedLeaseLock(base, leaseClient, "lease", annotations)
	record := resourcelock.LeaderElectionRecord{HolderIdentity: "node-a"}
	if err := lock.Create(context.Background(), record); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := lock.Update(context.Background(), record); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	resource, err := leaseClient.Get(context.Background(), "lease", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get Lease: %v", err)
	}
	value, err := kubevip.ParseLeaseVIPs(resource.Annotations[kubevip.LeaseVIPs])
	if err != nil {
		t.Fatalf("ParseLeaseVIPs() error = %v", err)
	}
	if value.InstanceName != "release_a" || value.IFAProto != 248 || len(value.VIPs) != 1 ||
		value.VIPs[0] != (kubevip.LeaseVIP{Index: 0, Value: "192.0.2.10"}) {
		t.Fatalf("Lease VIP metadata = %+v", value)
	}
	if resource.Annotations["example.test/preserved"] != "true" {
		t.Fatal("Lease update dropped a configured annotation")
	}
}

func TestAnnotatedLeaseLockFollowerDoesNotOverwriteAnnotations(t *testing.T) {
	client := fake.NewSimpleClientset()
	leaseClient := client.CoordinationV1().Leases("default")
	newBase := func(identity string) *resourcelock.LeaseLock {
		return &resourcelock.LeaseLock{
			LeaseMeta:  metav1.ObjectMeta{Name: "lease", Namespace: "default"},
			Client:     client.CoordinationV1(),
			LockConfig: resourcelock.ResourceLockConfig{Identity: identity},
		}
	}
	ownerBase := newBase("node-a")
	followerBase := newBase("node-b")
	active, err := kubevip.WithLeaseVIPs(nil, "release_a", 248, []string{"192.0.2.10"})
	if err != nil {
		t.Fatal(err)
	}
	creator := newAnnotatedLeaseLock(ownerBase, leaseClient, "lease", active)
	if err := creator.Create(context.Background(), resourcelock.LeaderElectionRecord{HolderIdentity: "node-a"}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	follower, err := kubevip.WithLeaseVIPs(nil, "release_b", 249, []string{"192.0.2.20"})
	if err != nil {
		t.Fatal(err)
	}
	observer := newAnnotatedLeaseLock(followerBase, leaseClient, "lease", follower)
	if _, _, err := observer.Get(context.Background()); err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	resource, err := leaseClient.Get(context.Background(), "lease", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := kubevip.ParseLeaseVIPs(resource.Annotations[kubevip.LeaseVIPs])
	if err != nil {
		t.Fatal(err)
	}
	if metadata.InstanceName != "release_a" || metadata.IFAProto != 248 {
		t.Fatalf("follower overwrote active metadata: %+v", metadata)
	}
}

func TestAnnotatedLeaseLockReleaseDoesNotOverwriteSuccessorMetadata(t *testing.T) {
	client := fake.NewSimpleClientset()
	leaseClient := client.CoordinationV1().Leases("default")
	newLock := func(identity, instanceName string, protocol int, vip string) resourcelock.Interface {
		base := &resourcelock.LeaseLock{
			LeaseMeta:  metav1.ObjectMeta{Name: "lease", Namespace: "default"},
			Client:     client.CoordinationV1(),
			LockConfig: resourcelock.ResourceLockConfig{Identity: identity},
		}
		annotations, err := kubevip.WithLeaseVIPs(nil, instanceName, protocol, []string{vip})
		if err != nil {
			t.Fatal(err)
		}
		return newAnnotatedLeaseLock(base, leaseClient, "lease", annotations)
	}

	first := newLock("node-a", "release_a", 248, "192.0.2.10")
	if err := first.Create(context.Background(), resourcelock.LeaderElectionRecord{HolderIdentity: "node-a"}); err != nil {
		t.Fatal(err)
	}
	if err := first.Update(context.Background(), resourcelock.LeaderElectionRecord{}); err != nil {
		t.Fatal(err)
	}

	second := newLock("node-b", "release_b", 249, "192.0.2.20")
	if _, _, err := second.Get(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := second.Update(context.Background(), resourcelock.LeaderElectionRecord{HolderIdentity: "node-b"}); err != nil {
		t.Fatal(err)
	}

	resource, err := leaseClient.Get(context.Background(), "lease", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := kubevip.ParseLeaseVIPs(resource.Annotations[kubevip.LeaseVIPs])
	if err != nil {
		t.Fatal(err)
	}
	if metadata.InstanceName != "release_b" || metadata.IFAProto != 249 || metadata.VIPs[0].Value != "192.0.2.20" {
		t.Fatalf("successor metadata = %+v", metadata)
	}
}
