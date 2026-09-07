package services

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/utils"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
)

// fakeWatchInterface is a minimal watch.Interface for testing.
type fakeWatchInterface struct {
	ch chan watch.Event
}

func newFakeWatchInterface() *fakeWatchInterface {
	return &fakeWatchInterface{ch: make(chan watch.Event)}
}

func (f *fakeWatchInterface) Stop()                          { close(f.ch) }
func (f *fakeWatchInterface) ResultChan() <-chan watch.Event { return f.ch }

func TestWatchWithAuthRetry(t *testing.T) {
	svcResource := schema.GroupResource{Resource: "services"}
	fw := newFakeWatchInterface()

	tcs := []struct {
		name         string
		watchFn      func(int) (watch.Interface, error)
		wantErr      bool
		wantAttempts int
	}{
		{
			name: "403 Forbidden retried then succeeds",
			watchFn: func(attempt int) (watch.Interface, error) {
				if attempt <= 2 {
					return nil, apierrors.NewForbidden(svcResource, "", nil)
				}
				return fw, nil
			},
			wantAttempts: 3,
		},
		{
			name: "401 Unauthorized retried then succeeds",
			watchFn: func(attempt int) (watch.Interface, error) {
				if attempt <= 2 {
					return nil, apierrors.NewUnauthorized("not authorized yet")
				}
				return fw, nil
			},
			wantAttempts: 3,
		},
		{
			name: "non-auth error fails immediately",
			watchFn: func(_ int) (watch.Interface, error) {
				return nil, fmt.Errorf("connection refused")
			},
			wantErr:      true,
			wantAttempts: 1,
		},
		{
			name: "immediate success no retry",
			watchFn: func(_ int) (watch.Interface, error) {
				return fw, nil
			},
			wantAttempts: 1,
		},
	}

	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			attempts := 0
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			w, err := utils.WatchWithAuthRetry(ctx, func(_ context.Context) (watch.Interface, error) {
				attempts++
				return tc.watchFn(attempts)
			})

			if tc.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected success, got: %v", err)
			}
			if !tc.wantErr && w != fw {
				t.Fatal("returned watcher does not match expected")
			}
			if attempts != tc.wantAttempts {
				t.Errorf("expected %d attempts, got %d", tc.wantAttempts, attempts)
			}
		})
	}
}

func TestServiceEventQueuePreservesOrderPerUID(t *testing.T) {
	queue := newServiceEventQueue(context.Background(), 2)
	key := types.NamespacedName{Namespace: "default", Name: "service"}
	releaseFirst := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseFirst) }) })
	order := make(chan int, 2)
	firstStarted := make(chan struct{})

	queue.Add(key, types.UID("service"), func() time.Duration {
		close(firstStarted)
		<-releaseFirst
		order <- 1
		return 0
	})
	<-firstStarted
	queue.Add(key, types.UID("service"), func() time.Duration {
		order <- 2
		return 0
	})
	releaseOnce.Do(func() { close(releaseFirst) })
	queue.Wait()

	if first, second := <-order, <-order; first != 1 || second != 2 {
		t.Fatalf("execution order = [%d %d], want [1 2]", first, second)
	}
}

func TestServiceEventQueueRunsDifferentUIDsConcurrently(t *testing.T) {
	queue := newServiceEventQueue(context.Background(), 2)
	releaseFirst := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseFirst) }) })
	firstStarted := make(chan struct{})
	secondStarted := make(chan struct{})

	queue.Add(types.NamespacedName{Namespace: "default", Name: "first"}, types.UID("first"), func() time.Duration {
		close(firstStarted)
		<-releaseFirst
		return 0
	})
	<-firstStarted
	queue.Add(types.NamespacedName{Namespace: "default", Name: "second"}, types.UID("second"), func() time.Duration {
		close(secondStarted)
		return 0
	})

	select {
	case <-secondStarted:
	case <-time.After(time.Second):
		t.Fatal("unrelated Service event waited for the blocked Service")
	}
	releaseOnce.Do(func() { close(releaseFirst) })
	queue.Wait()
}

func TestServiceEventQueueCoalescesPendingUpdates(t *testing.T) {
	queue := newServiceEventQueue(context.Background(), 0)
	key := types.NamespacedName{Namespace: "default", Name: "service"}
	ran := ""
	queue.Add(key, types.UID("service"), func() time.Duration { ran = "first"; return 0 })
	queue.Add(key, types.UID("service"), func() time.Duration { ran = "second"; return 0 })
	queue.wg.Go(queue.run)
	queue.Wait()

	if ran != "second" {
		t.Fatalf("pending update result = %q, want latest update", ran)
	}
}

func TestServiceEventQueueOrdersDeleteAndRecreateByName(t *testing.T) {
	queue := newServiceEventQueue(context.Background(), 2)
	key := types.NamespacedName{Namespace: "default", Name: "service"}
	releaseDelete := make(chan struct{})
	deleteStarted := make(chan struct{})
	addStarted := make(chan struct{})
	order := make(chan string, 2)

	queue.Add(key, types.UID("old"), func() time.Duration {
		close(deleteStarted)
		<-releaseDelete
		order <- "delete"
		return 0
	})
	<-deleteStarted
	queue.Add(key, types.UID("new"), func() time.Duration {
		close(addStarted)
		order <- "add"
		return 0
	})
	select {
	case <-addStarted:
		t.Fatal("recreated Service started before deletion finished")
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseDelete)
	queue.Wait()

	if first, second := <-order, <-order; first != "delete" || second != "add" {
		t.Fatalf("execution order = [%s %s], want [delete add]", first, second)
	}
}

func TestServiceEventQueueDelayedTasksDoNotStarveWorkers(t *testing.T) {
	queue := newServiceEventQueue(context.Background(), concurrentServiceEventWorkers)
	for index := range concurrentServiceEventWorkers {
		key := types.NamespacedName{Namespace: "default", Name: fmt.Sprintf("pending-%d", index)}
		queue.Add(key, types.UID(key.Name), func() time.Duration { return time.Hour })
	}
	run := make(chan struct{})
	queue.Add(types.NamespacedName{Namespace: "default", Name: "ready"}, types.UID("ready"), func() time.Duration {
		close(run)
		return 0
	})
	select {
	case <-run:
	case <-time.After(time.Second):
		t.Fatal("delayed address tasks starved an unrelated Service event")
	}
	queue.Wait()
}

func TestServiceMatchesWatcher(t *testing.T) {
	regular := &v1.Service{}
	forced := &v1.Service{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{kubevip.ForcePerServiceElection: "true"}}}
	if !serviceMatchesWatcher(regular, false) || serviceMatchesWatcher(regular, true) {
		t.Fatal("regular Service watcher ownership is incorrect")
	}
	if !serviceMatchesWatcher(forced, true) || serviceMatchesWatcher(forced, false) {
		t.Fatal("forced-election Service watcher ownership is incorrect")
	}
}
