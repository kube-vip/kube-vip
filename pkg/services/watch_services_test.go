package services

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	"github.com/kube-vip/kube-vip/pkg/utils"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
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

	queue.Add(key, types.UID("service"), func() {
		close(firstStarted)
		<-releaseFirst
		order <- 1
	})
	<-firstStarted
	queue.Add(key, types.UID("service"), func() {
		order <- 2
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

	queue.Add(types.NamespacedName{Namespace: "default", Name: "first"}, types.UID("first"), func() {
		close(firstStarted)
		<-releaseFirst
	})
	<-firstStarted
	queue.Add(types.NamespacedName{Namespace: "default", Name: "second"}, types.UID("second"), func() {
		close(secondStarted)
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
	queue.Add(key, types.UID("service"), func() { ran = "first" })
	queue.Add(key, types.UID("service"), func() { ran = "second" })
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

	queue.Add(key, types.UID("old"), func() {
		close(deleteStarted)
		<-releaseDelete
		order <- "delete"
	})
	<-deleteStarted
	queue.Add(key, types.UID("new"), func() {
		close(addStarted)
		order <- "add"
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

// TestServiceEventWithoutAddressDoesNotRequestRetry asserts the desired
// behaviour: a watch event for a Service that has no load-balancer address
// yet must let processServiceEvent return nil instead of the sentinel that
// makes ServicesWatcher re-queue the event once per second forever. The
// address will arrive on a later Modified event instead.
func TestServiceEventWithoutAddressDoesNotRequestRetry(t *testing.T) {
	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "test-service", Namespace: "default", UID: "service-uid"},
		Spec:       v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
	}

	var requests int32
	// Processor.clientSet is a concrete *kubernetes.Clientset (not an interface),
	// so the client-go fake clientset cannot be substituted; an httptest server
	// stands in for the API server instead.
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/api/v1/namespaces/default/services/test-service" {
			t.Errorf("unexpected request: %s %s", request.Method, request.URL.Path)
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		atomic.AddInt32(&requests, 1)
		writer.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(writer).Encode(svc); err != nil {
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

	p := &Processor{
		config:        &kubevip.Config{},
		clientSet:     clientSet,
		lbClassFilter: func(*v1.Service, *kubevip.Config) bool { return false },
	}
	callback := Callback(func(*servicecontext.Context, *v1.Service, *sync.WaitGroup) error { return nil })

	err = p.processServiceEvent(context.Background(), watch.Event{Type: watch.Added, Object: svc}, callback, false, &sync.WaitGroup{}, func(error) {})
	if err != nil {
		t.Fatalf("processServiceEvent() error = %v, want nil: a Service without an address yet must not request a retry", err)
	}
	if got := atomic.LoadInt32(&requests); got != 1 {
		t.Fatalf("GET requests for the Service = %d, want 1", got)
	}
}
