package lease

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func createTestService(name, namespace string, annotations map[string]string) *v1.Service {
	return &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Annotations: annotations,
		},
	}
}

func getSvcID(svc *v1.Service) ID {
	namespace, name := ServiceName(svc)
	id := NewID("kubernetes", namespace, name)
	return id
}

func getSvcData(svc *v1.Service) (context.Context, ID) {
	return context.TODO(), getSvcID(svc)
}

const serviceLeaseAnnotation = kubevip.ServiceLease

// TestManager_Add_NewLease tests adding a new service with a new lease
func TestManager_Add_NewLease(t *testing.T) {
	mgr := NewManager()
	svc := createTestService("test-svc", "default", nil)

	leaseID := mgr.Add(getSvcData(svc))
	isNew := leaseID.Add(ServiceNamespacedName(svc))

	if !isNew {
		t.Error("expected isNew to be true for first Add")
	}
	if leaseID == nil {
		t.Fatal("expected lease to be non-nil")
	}
	if leaseID.Ctx == nil {
		t.Error("expected lease context to be non-nil")
	}
	if leaseID.Cancel == nil {
		t.Error("expected lease cancel func to be non-nil")
	}
	if leaseID.Started == nil {
		t.Error("expected lease Started channel to be non-nil")
	}

}

// TestManager_Add_ExistingLease tests adding a service with an existing lease
func TestManager_Add_ExistingLease(t *testing.T) {
	mgr := NewManager()
	svc := createTestService("test-svc", "default", nil)

	leaseID1 := mgr.Add(getSvcData(svc))
	isNew1 := leaseID1.Add(ServiceNamespacedName(svc))

	leaseID2 := mgr.Add(getSvcData(svc))
	isNew2 := leaseID2.Add(ServiceNamespacedName(svc))

	if !isNew1 {
		t.Error("expected first Add to return isNew=true")
	}
	if isNew2 {
		t.Error("expected second Add to return isNew=false")
	}
	if leaseID1 != leaseID2 {
		t.Error("expected same lease to be returned for same service")
	}
}

// TestManager_Delete_DecrementCounter tests the decrement counter functionality
func TestManager_Delete_DecrementCounter(t *testing.T) {
	mgr := NewManager()
	svc := createTestService("test-svc", "default", nil)

	// Add twice (simulating adding same service twice)
	objectName := ServiceNamespacedName(svc)

	ctx1, leaseID1 := getSvcData(svc)
	lease1 := mgr.Add(ctx1, leaseID1)
	_ = lease1.Add(objectName)

	ctx2, leaseID2 := getSvcData(svc)
	lease2 := mgr.Add(ctx2, leaseID2)
	_ = lease2.Add(objectName)

	// Delete once - should remove the lease

	mgr.Delete(leaseID1, objectName)

	lease := mgr.Get(getSvcID(svc))
	if lease != nil {
		t.Error("expected lease to be removed after first delete if same service was processed twice")
	}
}

// TestManager_Delete_CancelsContext tests the context cancellation on delete
func TestManager_Delete_CancelsContext(t *testing.T) {
	mgr := NewManager()
	svc := createTestService("test-svc", "default", nil)

	objectName := ServiceNamespacedName(svc)

	ctx1, leaseID1 := getSvcData(svc)
	lease1 := mgr.Add(ctx1, leaseID1)
	_ = lease1.Add(objectName)

	// Verify context is not cancelled
	select {
	case <-lease1.Ctx.Done():
		t.Fatal("expected context to not be cancelled initially")
	default:
		// Expected
	}

	// Delete the lease
	mgr.Delete(leaseID1, objectName)

	// Verify context is cancelled
	select {
	case <-lease1.Ctx.Done():
		// Expected
	case <-time.After(100 * time.Millisecond):
		t.Error("expected context to be cancelled after delete")
	}
}

// TestManager_Add_AfterDelete_CreatesNewLease tests adding a service after deleting it
func TestManager_Add_AfterDelete_CreatesNewLease(t *testing.T) {
	mgr := NewManager()
	svc := createTestService("test-svc", "default", nil)

	objectName := ServiceNamespacedName(svc)

	ctx1, leaseID1 := getSvcData(svc)
	lease1 := mgr.Add(ctx1, leaseID1)
	_ = lease1.Add(objectName)

	mgr.Delete(leaseID1, objectName)

	ctx2, leaseID2 := getSvcData(svc)
	lease2 := mgr.Add(ctx2, leaseID2)
	isNew := lease2.Add(objectName)

	if !isNew {
		t.Error("expected isNew to be true after delete and re-add")
	}
	if lease1 == lease2 {
		t.Error("expected new lease to be different from old lease")
	}
}

// TestManager_Add_DifferentServices tests adding services with different names
func TestManager_Add_DifferentServices(t *testing.T) {
	mgr := NewManager()
	svc1 := createTestService("svc1", "default", nil)
	svc2 := createTestService("svc2", "default", nil)

	objectName1 := ServiceNamespacedName(svc1)

	ctx1, leaseID1 := getSvcData(svc1)
	lease1 := mgr.Add(ctx1, leaseID1)
	isNew1 := lease1.Add(objectName1)

	objectName2 := ServiceNamespacedName(svc2)

	ctx2, leaseID2 := getSvcData(svc2)
	lease2 := mgr.Add(ctx2, leaseID2)
	isNew2 := lease1.Add(objectName2)

	if !isNew1 || !isNew2 {
		t.Error("expected both adds to return isNew=true")
	}
	if lease1 == lease2 {
		t.Error("expected different leases for different services")
	}
}

// TestManager_Add_SameNameDifferentNamespace tests adding services with the same name but different namespaces
func TestManager_Add_SameNameDifferentNamespace(t *testing.T) {
	mgr := NewManager()
	svc1 := createTestService("test-svc", "namespace1", nil)
	svc2 := createTestService("test-svc", "namespace2", nil)

	objectName1 := ServiceNamespacedName(svc1)

	ctx1, leaseID1 := getSvcData(svc1)
	lease1 := mgr.Add(ctx1, leaseID1)
	isNew1 := lease1.Add(objectName1)

	objectName2 := ServiceNamespacedName(svc2)

	ctx2, leaseID2 := getSvcData(svc2)
	lease2 := mgr.Add(ctx2, leaseID2)
	isNew2 := lease1.Add(objectName2)

	if !isNew1 || !isNew2 {
		t.Error("expected both adds to return isNew=true")
	}
	if lease1 == lease2 {
		t.Error("expected different leases for services in different namespaces")
	}
}

// TestManager_ConcurrentAccess tests concurrent access to the lease manager
func TestManager_ConcurrentAccess(t *testing.T) {
	mgr := NewManager()
	svc := createTestService("test-svc", "default", nil)

	var wg sync.WaitGroup
	const numGoroutines = 100

	objectName1 := ServiceNamespacedName(svc)

	ctx1, leaseID1 := getSvcData(svc)
	lease1 := mgr.Add(ctx1, leaseID1)

	added := lease1.Add(objectName1)
	if !added {
		t.Error("expected lease to be added")
	}

	// Concurrent adds
	for range numGoroutines {
		wg.Go(func() {
			added := lease1.Add(objectName1)
			if added {
				t.Error("expected lease to already exist")
			}
		})
	}
	wg.Wait()

	mgr.Delete(leaseID1, objectName1)

	// After a one delete, lease should be gone
	lease := mgr.Get(getSvcID(svc))
	if lease != nil {
		t.Error("expected lease to be removed after all concurrent deletes")
	}
}

// TestLease_StartedChannel tests the Started channel behavior
func TestLease_StartedChannel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lease := newLease(ctx, cancel)

	// Started channel should be open initially
	select {
	case <-lease.Started:
		t.Fatal("expected Started channel to be open initially")
	default:
		// Expected
	}

	// Close the channel
	close(lease.Started)

	// Now it should be closed
	select {
	case <-lease.Started:
		// Expected
	default:
		t.Error("expected Started channel to be closed after close()")
	}
}

// TestGetName_WithoutAnnotation tests with no annotation
func TestGetName_WithoutAnnotation(t *testing.T) {
	svc := createTestService("my-service", "my-namespace", nil)

	namespace, name := ServiceName(svc)
	id := NewID("kubernetes", namespace, name)

	expectedName := "kubevip-my-service"
	expectedID := "my-namespace/kubevip-my-service"

	if id.Name() != expectedName {
		t.Errorf("expected name %q, got %q", expectedName, id.Name())
	}
	if id.NamespacedName() != expectedID {
		t.Errorf("expected id %q, got %q", expectedID, id.NamespacedName())
	}
}

// TestGetName_WithAnnotation tests with a shared lease annotation
func TestGetName_WithAnnotation(t *testing.T) {
	svc := createTestService("my-service", "my-namespace", map[string]string{
		serviceLeaseAnnotation: "shared-lease",
	})

	namespace, name := ServiceName(svc)
	id := NewID("kubernetes", namespace, name)

	expectedName := "shared-lease"
	expectedID := "my-namespace/shared-lease"

	if id.Name() != expectedName {
		t.Errorf("expected name %q, got %q", expectedName, id.Name())
	}
	if id.NamespacedName() != expectedID {
		t.Errorf("expected id %q, got %q", expectedID, id.NamespacedName())
	}
}

// TestGetName_WithAnnotation tests with a shared lease annotation
func TestGetName_WithAnnotationAndOverriddenNamespace(t *testing.T) {
	svc := createTestService("my-service", "my-namespace", map[string]string{
		serviceLeaseAnnotation: "other-namespace/shared-lease",
	})

	namespace, name := ServiceName(svc)
	id := NewID("kubernetes", namespace, name)

	expectedName := "shared-lease"
	expectedID := "other-namespace/shared-lease"
	expectedNamespace := "other-namespace"

	if id.Name() != expectedName {
		t.Errorf("expected name %q, got %q", expectedName, id.Name())
	}
	if id.NamespacedName() != expectedID {
		t.Errorf("expected id %q, got %q", expectedID, id.NamespacedName())
	}
	if id.Namespace() != expectedNamespace {
		t.Errorf("expected namespace %q, got %q", expectedNamespace, id.Namespace())
	}
}

// TestGetName_WithoutAnnotation_Etcd tests with no annotation
func TestGetName_WithoutAnnotation_Etcd(t *testing.T) {
	svc := createTestService("my-service", "my-namespace", nil)

	namespace, name := ServiceName(svc)
	id := NewID("etcd", namespace, name)

	expectedName := "kubevip-my-service"
	expectedID := "my-namespace-kubevip-my-service"

	if id.Name() != expectedName {
		t.Errorf("expected name %q, got %q", expectedName, id.Name())
	}
	if id.NamespacedName() != expectedID {
		t.Errorf("expected id %q, got %q", expectedID, id.NamespacedName())
	}
}

// TestGetName_WithAnnotation_Etcd tests with a shared lease annotation
func TestGetName_WithAnnotation_Etcd(t *testing.T) {
	svc := createTestService("my-service", "my-namespace", map[string]string{
		serviceLeaseAnnotation: "shared-lease",
	})

	namespace, name := ServiceName(svc)
	id := NewID("etcd", namespace, name)

	expectedName := "shared-lease"
	expectedID := "my-namespace-shared-lease"

	if id.Name() != expectedName {
		t.Errorf("expected name %q, got %q", expectedName, id.Name())
	}
	if id.NamespacedName() != expectedID {
		t.Errorf("expected id %q, got %q", expectedID, id.NamespacedName())
	}
}

// TestGetName_WithAnnotation_Etcd tests with a shared lease annotation
func TestGetName_WithAnnotationAndOverriddenNamespace_Etcd(t *testing.T) {
	svc := createTestService("my-service", "my-namespace", map[string]string{
		serviceLeaseAnnotation: "other-namespace/shared-lease",
	})

	namespace, name := ServiceName(svc)
	id := NewID("etcd", namespace, name)

	expectedName := "shared-lease"
	expectedID := "other-namespace-shared-lease"
	expectedNamespace := "other-namespace"

	if id.Name() != expectedName {
		t.Errorf("expected name %q, got %q", expectedName, id.Name())
	}
	if id.NamespacedName() != expectedID {
		t.Errorf("expected id %q, got %q", expectedID, id.NamespacedName())
	}
	if id.Namespace() != expectedNamespace {
		t.Errorf("expected namespace %q, got %q", expectedNamespace, id.Namespace())
	}
}

// TestManager_LeaderElectionRestartScenario simulates the bug scenario where
// leadership is lost and the restartable service watcher tries to restart
// the leader election. This test verifies that after deleting the lease,
// a new lease can be created.
func TestManager_LeaderElectionRestartScenario_etcd(t *testing.T) {
	mgr := NewManager()
	svc := createTestService("traefik", "traefik", nil)

	objectName1 := ServiceNamespacedName(svc)

	ctx1, leaseID1 := getSvcData(svc)
	lease1 := mgr.Add(ctx1, leaseID1)
	isNew1 := lease1.Add(objectName1)

	if !isNew1 {
		t.Fatal("expected first add to return isNew=true")
	}

	// Simulate leadership acquired - close Started channel
	close(lease1.Started)

	// Simulate leadership lost - the leader election function should delete the lease
	// This is the fix: delete the lease when RunOrDie returns
	mgr.Delete(leaseID1, objectName1)

	// Verify lease is removed
	if mgr.Get(getSvcID(svc)) != nil {
		t.Error("expected lease to be removed after delete")
	}

	// Simulate restartable service watcher calling StartServicesLeaderElection again
	ctx2, leaseID2 := getSvcData(svc)
	lease2 := mgr.Add(ctx2, leaseID2)
	isNew2 := lease1.Add(objectName1)
	if !isNew2 {
		t.Fatal("expected second add after delete to return isNew=true")
	}

	// Verify we got a new lease with a fresh Started channel
	if lease1 == lease2 {
		t.Error("expected new lease to be different from old lease")
	}

	// Verify the new Started channel is not closed
	select {
	case <-lease2.Started:
		t.Error("expected new lease's Started channel to be open")
	default:
		// Expected
	}
}

// TestManager_CommonLeaseScenario tests the common lease feature where
// multiple services share the same lease.
func TestManager_CommonLeaseScenario(t *testing.T) {
	mgr := NewManager()

	// Two services sharing the same lease via annotation
	sharedLeaseAnnotations := map[string]string{
		serviceLeaseAnnotation: "shared-lease",
	}
	svc1 := createTestService("svc1", "default", sharedLeaseAnnotations)
	svc2 := createTestService("svc2", "default", sharedLeaseAnnotations)

	// First service gets a new lease
	objectName1 := ServiceNamespacedName(svc1)

	ctx1, leaseID1 := getSvcData(svc1)
	lease1 := mgr.Add(ctx1, leaseID1)
	isNew1 := lease1.Add(objectName1)
	if !isNew1 {
		t.Error("expected first add to return isNew=true")
	}

	// Simulate first service starting leadership
	close(lease1.Started)

	objectName2 := ServiceNamespacedName(svc2)

	ctx2, leaseID2 := getSvcData(svc2)
	lease2 := mgr.Add(ctx2, leaseID2)
	isNew2 := lease2.Add(objectName2)

	// Second service should get the same lease
	if !isNew2 {
		t.Error("expected second add with same lease name to return isNew=true")
	}
	if lease1 != lease2 {
		t.Error("expected same lease for services with same lease annotation")
	}

	// Delete first service - lease should still exist
	mgr.Delete(leaseID1, objectName1)
	if mgr.Get(getSvcID(svc1)) == nil {
		t.Error("expected lease to still exist after first delete")
	}

	// Delete second service - lease should be removed
	mgr.Delete(leaseID2, objectName2)
	if mgr.Get(getSvcID(svc2)) != nil {
		t.Error("expected lease to be removed after all services deleted")
	}
}

// TestManager_RaceCondition_LeaseExistsBeforeDelete tests the scenario where
// a second goroutine calls Add before the first goroutine's defer deletes the lease.
// This simulates the race condition that could cause the gaps in the logs where there is no leader.
func TestManager_RaceCondition_LeaseExistsBeforeDelete(t *testing.T) {
	mgr := NewManager()
	svc := createTestService("traefik", "traefik", nil)

	// Simulate first leader election start
	objectName1 := ServiceNamespacedName(svc)

	ctx1, leaseID1 := getSvcData(svc)
	lease1 := mgr.Add(ctx1, leaseID1)
	isNew1 := lease1.Add(objectName1)
	if !isNew1 {
		t.Fatal("expected first add to return isNew=true")
	}

	// Simulate leadership acquired - close Started channel
	close(lease1.Started)

	// Simulate a second goroutine calling Add BEFORE the first goroutine's defer deletes the lease
	// This is the race condition scenario
	ctx2, leaseID2 := getSvcData(svc)
	lease2 := mgr.Add(ctx2, leaseID2)
	isNew2 := lease1.Add(objectName1)

	if isNew2 {
		t.Error("expected second add before delete to return isNew=false")
	}
	if lease1 != lease2 {
		t.Error("expected same lease to be returned")
	}

	// The Started channel should be closed (from the first run)
	select {
	case <-lease2.Started:
		// Expected - channel is closed
	default:
		t.Error("expected Started channel to be closed")
	}

	// Now the first goroutine's defer deletes the lease
	mgr.Delete(leaseID1, objectName1)

	// The lease should not still exist because same service was processed twice, so we do not increment the counter
	if mgr.Get(getSvcID(svc)) != nil {
		t.Error("expected lease tonot exist")
	}

	// Second delete does nothing
	mgr.Delete(leaseID2, objectName1)
	if mgr.Get(getSvcID(svc)) != nil {
		t.Error("expected lease to not exist")
	}
}

// TestManager_NonCommonLease_MultipleAdds tests that multiple Adds for a non-common
// lease service increment the counter correctly.
func TestManager_NonCommonLease_MultipleAdds(t *testing.T) {
	mgr := NewManager()
	svc := createTestService("traefik", "traefik", nil) // No common lease annotation

	// First Add
	objectName1 := ServiceNamespacedName(svc)

	ctx1, leaseID1 := getSvcData(svc)
	lease1 := mgr.Add(ctx1, leaseID1)
	isNew1 := lease1.Add(objectName1)
	if !isNew1 {
		t.Error("expected first add to return isNew=true")
	}

	// Close Started to simulate leadership acquired
	close(lease1.Started)

	// Second Add (simulating another goroutine or restart attempt)
	ctx2, leaseID2 := getSvcData(svc)
	lease2 := mgr.Add(ctx2, leaseID2)
	isNew2 := lease1.Add(objectName1)
	if isNew2 {
		t.Error("expected second add to return isNew=false")
	}
	if lease1 != lease2 {
		t.Error("expected same lease")
	}

	// Third Add
	ctx3, leaseID3 := getSvcData(svc)
	lease3 := mgr.Add(ctx3, leaseID3)
	isNew3 := lease1.Add(objectName1)
	if isNew3 {
		t.Error("expected third add to return isNew=false")
	}
	if lease1 != lease3 {
		t.Error("expected same lease")
	}

	// Need one delete to remove the lease, another delete runs do nothing
	mgr.Delete(leaseID1, objectName1)
	if mgr.Get(getSvcID(svc)) != nil {
		t.Error("expected lease to be deleted")
	}

	mgr.Delete(leaseID2, objectName1)
	if mgr.Get(getSvcID(svc)) != nil {
		t.Error("expected lease to be deleted")
	}

	mgr.Delete(leaseID3, objectName1)
	if mgr.Get(getSvcID(svc)) != nil {
		t.Error("expected lease to be deleted")
	}
}

// TestManager_LeaseContextCancelledBeforeStarted tests the scenario where
// the lease context is cancelled before the Started channel is closed.
// This can happen if leadership is never acquired and the context times out.
func TestManager_LeaseContextCancelledBeforeStarted(t *testing.T) {
	mgr := NewManager()
	svc := createTestService("traefik", "traefik", nil)

	// First Add
	objectName1 := ServiceNamespacedName(svc)

	ctx1, leaseID1 := getSvcData(svc)
	lease1 := mgr.Add(ctx1, leaseID1)
	isNew1 := lease1.Add(objectName1)

	if !isNew1 {
		t.Fatal("expected first add to return isNew=true")
	}

	ctx2, leaseID2 := getSvcData(svc)
	lease2 := mgr.Add(ctx2, leaseID2)
	isNew2 := lease1.Add(objectName1)

	if isNew2 {
		t.Error("expected second add to return isNew=false")
	}

	// Verify Started is not closed yet
	select {
	case <-lease2.Started:
		t.Error("expected Started channel to be open")
	default:
		// Expected
	}

	// Cancel the lease context (simulating timeout or leadership loss before acquiring)
	lease1.Cancel()

	// Verify context is cancelled
	select {
	case <-lease2.Ctx.Done():
		// Expected
	case <-time.After(100 * time.Millisecond):
		t.Error("expected context to be cancelled")
	}

	// Delete should still work
	mgr.Delete(leaseID1, objectName1)
	mgr.Delete(leaseID2, objectName1)

	if mgr.Get(getSvcID(svc)) != nil {
		t.Error("expected lease to be removed")
	}
}

// TestManager_RestartAfterLeaseContextCancelled tests that after the lease
// context is cancelled and the lease is deleted, a new lease can be created.
func TestManager_RestartAfterLeaseContextCancelled(t *testing.T) {
	mgr := NewManager()
	svc := createTestService("traefik", "traefik", nil)

	// First Add
	objectName1 := ServiceNamespacedName(svc)

	ctx1, leaseID1 := getSvcData(svc)
	lease1 := mgr.Add(ctx1, leaseID1)
	_ = lease1.Add(objectName1)

	// Cancel context before Started is closed
	lease1.Cancel()

	// Delete the lease
	mgr.Delete(leaseID1, objectName1)

	// Verify lease is gone
	if mgr.Get(getSvcID(svc)) != nil {
		t.Error("expected lease to be removed after delete")
	}

	// Add again - should create new lease
	ctx2, leaseID2 := getSvcData(svc)
	lease2 := mgr.Add(ctx2, leaseID2)
	isNew2 := lease1.Add(objectName1)

	if !isNew2 {
		t.Error("expected new lease after delete")
	}

	// Verify new lease has fresh context and Started channel
	select {
	case <-lease2.Ctx.Done():
		t.Error("expected new lease context to be active")
	default:
		// Expected
	}

	select {
	case <-lease2.Started:
		t.Error("expected new lease Started channel to be open")
	default:
		// Expected
	}
}

// TestManager_NonCommonLease_WaitForLeaseContextDone tests the scenario where
// a non-common lease service calls Add while another leader election is running.
// The caller should wait for the lease context to be done before returning.
// This test verifies the fix for the tight spin loop issue.
func TestManager_NonCommonLease_WaitForLeaseContextDone(t *testing.T) {
	mgr := NewManager()
	svc := createTestService("egress-service", "default", nil) // Non-common lease

	// First Add - simulates the first leader election starting
	objectName1 := ServiceNamespacedName(svc)

	ctx1, leaseID1 := getSvcData(svc)
	lease1 := mgr.Add(ctx1, leaseID1)
	isNew1 := lease1.Add(objectName1)

	if !isNew1 {
		t.Fatal("expected first add to return isNew=true")
	}

	// Simulate leadership acquired
	close(lease1.Started)

	// Second Add - simulates another goroutine trying to start leader election
	// This should return isNew=false
	ctx2, leaseID2 := getSvcData(svc)
	lease2 := mgr.Add(ctx2, leaseID2)
	isNew2 := lease1.Add(objectName1)

	if isNew2 {
		t.Error("expected second add to return isNew=false")
	}

	if lease1 != lease2 {
		t.Error("expected same lease to be returned")
	}

	// Verify Started channel is closed (leadership was acquired by first)
	select {
	case <-lease2.Started:
		// Expected - channel is closed
	default:
		t.Error("expected Started channel to be closed")
	}

	// In the actual code (leader.go), when isNew=false for non-common lease,
	// the code waits on either svcCtx.Ctx.Done() or svcLease.Ctx.Done()
	// Here we verify that the lease context gets cancelled when we delete the lease

	// Start a goroutine that waits for the lease context to be done
	// This simulates what the leader.go code does
	waitDone := make(chan struct{})
	go func() {
		select {
		case <-lease2.Ctx.Done():
			close(waitDone)
		case <-time.After(1 * time.Second):
			// Timeout - test will fail
		}
	}()

	// Verify the goroutine is still waiting (lease context not yet cancelled)
	select {
	case <-waitDone:
		t.Fatal("goroutine should still be waiting")
	case <-time.After(50 * time.Millisecond):
		// Expected - still waiting
	}

	// Now simulate the first leader election ending (defer deletes the lease)
	mgr.Delete(leaseID1, objectName1)

	// The lease context should now be cancelled (because counter went to 0)
	// But we added twice, so we need to delete twice
	mgr.Delete(leaseID2, objectName1)

	// Now the goroutine should have completed
	select {
	case <-waitDone:
		// Expected - lease context was cancelled
	case <-time.After(200 * time.Millisecond):
		t.Error("expected goroutine to complete after lease context cancelled")
	}

	// Verify lease is removed
	if mgr.Get(getSvcID(svc)) != nil {
		t.Error("expected lease to be removed")
	}
}

// TestManager_NonCommonLease_SpinLoopPrevention tests that the fix prevents
// a tight spin loop when a non-common lease service repeatedly calls Add
// while leader election is running. The key behavior is that when isNew=false,
// the lease context should be used to block until the leader election ends.
func TestManager_NonCommonLease_SpinLoopPrevention(t *testing.T) {
	mgr := NewManager()
	svc := createTestService("egress-service", "default", nil) // Non-common lease

	// First Add - leader election starts
	objectName1 := ServiceNamespacedName(svc)

	ctx1, leaseID1 := getSvcData(svc)
	lease1 := mgr.Add(ctx1, leaseID1)
	isNew1 := lease1.Add(objectName1)

	if !isNew1 {
		t.Fatal("expected first add to return isNew=true")
	}

	close(lease1.Started)

	// Track how many times Add is called in a tight loop
	// In the buggy code, this would spin forever
	// In the fixed code, Add returns isNew=false and the caller blocks on lease.Ctx.Done()
	addCount := 0
	done := make(chan struct{})

	go func() {
		for i := 0; i < 100; i++ {
			objectName1 := ServiceNamespacedName(svc)

			ctxTmp, leaseIDTmp := getSvcData(svc)
			leaseTmp := mgr.Add(ctxTmp, leaseIDTmp)
			isNewTmp := leaseTmp.Add(objectName1)
			addCount++
			if isNewTmp {
				// This shouldn't happen while the first lease exists
				t.Error("unexpected isNew=true")
				break
			}
			// In the fixed code, we would block here on lease.Ctx.Done()
			// For this test, we just verify that isNew=false is returned
			// and the same lease is returned each time
			if leaseTmp != lease1 {
				t.Error("expected same lease")
				break
			}
		}
		close(done)
	}()

	// Wait for the loop to complete
	select {
	case <-done:
		// Expected
	case <-time.After(1 * time.Second):
		t.Fatal("loop timed out")
	}

	// All 100 adds should have completed (returning isNew=false)
	if addCount != 100 {
		t.Errorf("expected 100 adds, got %d", addCount)
	}

	mgr.Delete(leaseID1, objectName1)
	if mgr.Get(getSvcID(svc)) != nil {
		t.Error("expected lease to be removed after first delete")
	}
}

// TestManager_NonCommonLease_ServiceContextCancellation tests that when
// a service is deleted (svcCtx.Ctx cancelled), the waiting goroutine
// should also unblock. This is the other exit path from the wait.
func TestManager_NonCommonLease_ServiceContextCancellation(t *testing.T) {
	mgr := NewManager()
	svc := createTestService("egress-service", "default", nil)

	// First Add - leader election starts
	objectName1 := ServiceNamespacedName(svc)

	ctx1, leaseID1 := getSvcData(svc)
	lease1 := mgr.Add(ctx1, leaseID1)
	_ = lease1.Add(objectName1)
	close(lease1.Started)

	// Second Add - returns isNew=false
	ctx2, leaseID2 := getSvcData(svc)
	lease2 := mgr.Add(ctx2, leaseID2)
	isNew2 := lease2.Add(objectName1)
	if isNew2 {
		t.Error("expected isNew=false")
	}

	// Create a simulated service context
	svcCtx, svcCancel := context.WithCancel(context.Background())

	// Start a goroutine that waits on either svcCtx or lease context
	// This simulates the behavior in leader.go
	waitDone := make(chan string)
	go func() {
		select {
		case <-svcCtx.Done():
			waitDone <- "svcCtx"
		case <-lease2.Ctx.Done():
			waitDone <- "leaseCtx"
		case <-time.After(1 * time.Second):
			waitDone <- "timeout"
		}
	}()

	// Cancel the service context (simulates service deletion)
	svcCancel()

	// The goroutine should unblock via svcCtx.Done()
	select {
	case result := <-waitDone:
		if result != "svcCtx" {
			t.Errorf("expected to unblock via svcCtx, got %s", result)
		}
	case <-time.After(200 * time.Millisecond):
		t.Error("goroutine should have unblocked")
	}
}
