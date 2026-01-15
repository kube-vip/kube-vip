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

const serviceLeaseAnnotation = kubevip.ServiceLease

// TestManager_Add_NewLease tests adding a new service with a new lease
func TestManager_Add_NewLease(t *testing.T) {
	mgr := NewManager()
	svc := createTestService("test-svc", "default", nil)

	lease, isNew := mgr.Add(svc)

	if !isNew {
		t.Error("expected isNew to be true for first Add")
	}
	if lease == nil {
		t.Fatal("expected lease to be non-nil")
	}
	if lease.Ctx == nil {
		t.Error("expected lease context to be non-nil")
	}
	if lease.Cancel == nil {
		t.Error("expected lease cancel func to be non-nil")
	}
	if lease.Started == nil {
		t.Error("expected lease Started channel to be non-nil")
	}
}

// TestManager_Add_ExistingLease tests adding a service with an existing lease
func TestManager_Add_ExistingLease(t *testing.T) {
	mgr := NewManager()
	svc := createTestService("test-svc", "default", nil)

	lease1, isNew1 := mgr.Add(svc)
	lease2, isNew2 := mgr.Add(svc)

	if !isNew1 {
		t.Error("expected first Add to return isNew=true")
	}
	if isNew2 {
		t.Error("expected second Add to return isNew=false")
	}
	if lease1 != lease2 {
		t.Error("expected same lease to be returned for same service")
	}
}

// TestManager_Delete_DecrementCounter tests the decrement counter functionality
func TestManager_Delete_DecrementCounter(t *testing.T) {
	mgr := NewManager()
	svc := createTestService("test-svc", "default", nil)

	// Add twice (simulating two services sharing the lease)
	mgr.Add(svc)
	mgr.Add(svc)

	// Delete once - should not remove the lease
	mgr.Delete(svc)

	lease := mgr.Get(svc)
	if lease == nil {
		t.Error("expected lease to still exist after first delete")
	}

	// Delete again - should remove the lease
	mgr.Delete(svc)

	lease = mgr.Get(svc)
	if lease != nil {
		t.Error("expected lease to be removed after second delete")
	}
}

// TestManager_Delete_CancelsContext tests the context cancellation on delete
func TestManager_Delete_CancelsContext(t *testing.T) {
	mgr := NewManager()
	svc := createTestService("test-svc", "default", nil)

	lease, _ := mgr.Add(svc)

	// Verify context is not cancelled
	select {
	case <-lease.Ctx.Done():
		t.Fatal("expected context to not be cancelled initially")
	default:
		// Expected
	}

	// Delete the lease
	mgr.Delete(svc)

	// Verify context is cancelled
	select {
	case <-lease.Ctx.Done():
		// Expected
	case <-time.After(100 * time.Millisecond):
		t.Error("expected context to be cancelled after delete")
	}
}

// TestManager_Add_AfterDelete_CreatesNewLease tests adding a service after deleting it
func TestManager_Add_AfterDelete_CreatesNewLease(t *testing.T) {
	mgr := NewManager()
	svc := createTestService("test-svc", "default", nil)

	// Add and delete
	lease1, _ := mgr.Add(svc)
	mgr.Delete(svc)

	// Add again - should create new lease
	lease2, isNew := mgr.Add(svc)

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

	lease1, isNew1 := mgr.Add(svc1)
	lease2, isNew2 := mgr.Add(svc2)

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

	lease1, isNew1 := mgr.Add(svc1)
	lease2, isNew2 := mgr.Add(svc2)

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

	// Concurrent adds
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mgr.Add(svc)
		}()
	}
	wg.Wait()

	// Concurrent deletes
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mgr.Delete(svc)
		}()
	}
	wg.Wait()

	// After all deletes, lease should be gone
	lease := mgr.Get(svc)
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

	name, id := GetName(svc)

	expectedName := "kubevip-my-service"
	expectedID := "kubevip-my-service/my-namespace"

	if name != expectedName {
		t.Errorf("expected name %q, got %q", expectedName, name)
	}
	if id != expectedID {
		t.Errorf("expected id %q, got %q", expectedID, id)
	}
}

// TestGetName_WithAnnotation tests with a shared lease annotation
func TestGetName_WithAnnotation(t *testing.T) {
	svc := createTestService("my-service", "my-namespace", map[string]string{
		serviceLeaseAnnotation: "shared-lease",
	})

	name, id := GetName(svc)

	expectedName := "shared-lease"
	expectedID := "shared-lease/my-namespace"

	if name != expectedName {
		t.Errorf("expected name %q, got %q", expectedName, name)
	}
	if id != expectedID {
		t.Errorf("expected id %q, got %q", expectedID, id)
	}
}

// TestUsesCommon tests the common lease detection
func TestUsesCommon(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		expected    bool
	}{
		{
			name:        "no annotation",
			annotations: nil,
			expected:    false,
		},
		{
			name:        "empty annotations",
			annotations: map[string]string{},
			expected:    false,
		},
		{
			name: "with service-lease annotation",
			annotations: map[string]string{
				serviceLeaseAnnotation: "shared-lease",
			},
			expected: true,
		},
		{
			name: "with other annotation",
			annotations: map[string]string{
				"some-other-annotation": "value",
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := createTestService("test-svc", "default", tt.annotations)
			result := UsesCommon(svc)
			if result != tt.expected {
				t.Errorf("UsesCommon() = %v, expected %v", result, tt.expected)
			}
		})
	}
}

// TestManager_LeaderElectionRestartScenario simulates the bug scenario where
// leadership is lost and the restartable service watcher tries to restart
// the leader election. This test verifies that after deleting the lease,
// a new lease can be created.
func TestManager_LeaderElectionRestartScenario(t *testing.T) {
	mgr := NewManager()
	svc := createTestService("traefik", "traefik", nil)

	// Simulate first leader election start
	lease1, isNew1 := mgr.Add(svc)
	if !isNew1 {
		t.Fatal("expected first add to return isNew=true")
	}

	// Simulate leadership acquired - close Started channel
	close(lease1.Started)

	// Simulate leadership lost - the leader election function should delete the lease
	// This is the fix: delete the lease when RunOrDie returns
	mgr.Delete(svc)

	// Verify lease is removed
	if mgr.Get(svc) != nil {
		t.Error("expected lease to be removed after delete")
	}

	// Simulate restartable service watcher calling StartServicesLeaderElection again
	lease2, isNew2 := mgr.Add(svc)
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
	lease1, isNew1 := mgr.Add(svc1)
	if !isNew1 {
		t.Error("expected first add to return isNew=true")
	}

	// Simulate first service starting leadership
	close(lease1.Started)

	// Second service should get the same lease
	lease2, isNew2 := mgr.Add(svc2)
	if isNew2 {
		t.Error("expected second add with same lease name to return isNew=false")
	}
	if lease1 != lease2 {
		t.Error("expected same lease for services with same lease annotation")
	}

	// Delete first service - lease should still exist
	mgr.Delete(svc1)
	if mgr.Get(svc1) == nil {
		t.Error("expected lease to still exist after first delete")
	}

	// Delete second service - lease should be removed
	mgr.Delete(svc2)
	if mgr.Get(svc1) != nil {
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
	lease1, isNew1 := mgr.Add(svc)
	if !isNew1 {
		t.Fatal("expected first add to return isNew=true")
	}

	// Simulate leadership acquired - close Started channel
	close(lease1.Started)

	// Simulate a second goroutine calling Add BEFORE the first goroutine's defer deletes the lease
	// This is the race condition scenario
	lease2, isNew2 := mgr.Add(svc)
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
	mgr.Delete(svc)

	// The lease should still exist because the second Add incremented the counter
	if mgr.Get(svc) == nil {
		t.Error("expected lease to still exist after first delete (counter was incremented)")
	}

	// Second delete removes the lease
	mgr.Delete(svc)
	if mgr.Get(svc) != nil {
		t.Error("expected lease to be removed after second delete")
	}
}

// TestManager_NonCommonLease_MultipleAdds tests that multiple Adds for a non-common
// lease service increment the counter correctly.
func TestManager_NonCommonLease_MultipleAdds(t *testing.T) {
	mgr := NewManager()
	svc := createTestService("traefik", "traefik", nil) // No common lease annotation

	// First Add
	lease1, isNew1 := mgr.Add(svc)
	if !isNew1 {
		t.Error("expected first add to return isNew=true")
	}

	// Close Started to simulate leadership acquired
	close(lease1.Started)

	// Second Add (simulating another goroutine or restart attempt)
	lease2, isNew2 := mgr.Add(svc)
	if isNew2 {
		t.Error("expected second add to return isNew=false")
	}
	if lease1 != lease2 {
		t.Error("expected same lease")
	}

	// Third Add
	lease3, isNew3 := mgr.Add(svc)
	if isNew3 {
		t.Error("expected third add to return isNew=false")
	}
	if lease1 != lease3 {
		t.Error("expected same lease")
	}

	// Need 3 deletes to remove the lease
	mgr.Delete(svc)
	if mgr.Get(svc) == nil {
		t.Error("expected lease to exist after first delete")
	}

	mgr.Delete(svc)
	if mgr.Get(svc) == nil {
		t.Error("expected lease to exist after second delete")
	}

	mgr.Delete(svc)
	if mgr.Get(svc) != nil {
		t.Error("expected lease to be removed after third delete")
	}
}

// TestManager_LeaseContextCancelledBeforeStarted tests the scenario where
// the lease context is cancelled before the Started channel is closed.
// This can happen if leadership is never acquired and the context times out.
func TestManager_LeaseContextCancelledBeforeStarted(t *testing.T) {
	mgr := NewManager()
	svc := createTestService("traefik", "traefik", nil)

	// First Add
	lease1, isNew1 := mgr.Add(svc)
	if !isNew1 {
		t.Fatal("expected first add to return isNew=true")
	}

	// Second Add before Started is closed
	lease2, isNew2 := mgr.Add(svc)
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
	mgr.Delete(svc)
	mgr.Delete(svc)

	if mgr.Get(svc) != nil {
		t.Error("expected lease to be removed")
	}
}

// TestManager_RestartAfterLeaseContextCancelled tests that after the lease
// context is cancelled and the lease is deleted, a new lease can be created.
func TestManager_RestartAfterLeaseContextCancelled(t *testing.T) {
	mgr := NewManager()
	svc := createTestService("traefik", "traefik", nil)

	// First Add
	lease1, _ := mgr.Add(svc)

	// Cancel context before Started is closed
	lease1.Cancel()

	// Delete the lease
	mgr.Delete(svc)

	// Verify lease is gone
	if mgr.Get(svc) != nil {
		t.Error("expected lease to be removed after delete")
	}

	// Add again - should create new lease
	lease2, isNew2 := mgr.Add(svc)
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
	lease1, isNew1 := mgr.Add(svc)
	if !isNew1 {
		t.Fatal("expected first add to return isNew=true")
	}

	// Simulate leadership acquired
	close(lease1.Started)

	// Second Add - simulates another goroutine trying to start leader election
	// This should return isNew=false
	lease2, isNew2 := mgr.Add(svc)
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
	mgr.Delete(svc)

	// The lease context should now be cancelled (because counter went to 0)
	// But we added twice, so we need to delete twice
	mgr.Delete(svc)

	// Now the goroutine should have completed
	select {
	case <-waitDone:
		// Expected - lease context was cancelled
	case <-time.After(200 * time.Millisecond):
		t.Error("expected goroutine to complete after lease context cancelled")
	}

	// Verify lease is removed
	if mgr.Get(svc) != nil {
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
	lease1, isNew1 := mgr.Add(svc)
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
			lease, isNew := mgr.Add(svc)
			addCount++
			if isNew {
				// This shouldn't happen while the first lease exists
				t.Error("unexpected isNew=true")
				break
			}
			// In the fixed code, we would block here on lease.Ctx.Done()
			// For this test, we just verify that isNew=false is returned
			// and the same lease is returned each time
			if lease != lease1 {
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

	// The lease counter should be 101 (1 original + 100 from loop)
	// We need 101 deletes to remove the lease
	for i := 0; i < 100; i++ {
		mgr.Delete(svc)
	}
	if mgr.Get(svc) == nil {
		t.Error("expected lease to still exist after 100 deletes")
	}

	mgr.Delete(svc)
	if mgr.Get(svc) != nil {
		t.Error("expected lease to be removed after 101 deletes")
	}
}

// TestManager_NonCommonLease_ServiceContextCancellation tests that when
// a service is deleted (svcCtx.Ctx cancelled), the waiting goroutine
// should also unblock. This is the other exit path from the wait.
func TestManager_NonCommonLease_ServiceContextCancellation(t *testing.T) {
	mgr := NewManager()
	svc := createTestService("egress-service", "default", nil)

	// First Add - leader election starts
	lease1, _ := mgr.Add(svc)
	close(lease1.Started)

	// Second Add - returns isNew=false
	lease2, isNew2 := mgr.Add(svc)
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
