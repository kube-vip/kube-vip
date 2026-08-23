package debouncer

import (
	"context"
	"runtime"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
)

func TestStartReturnsWhenCancellationInterruptsObjectForwarding(t *testing.T) {
	input := make(chan watch.Event)
	d := &debouncer{
		input:        input,
		output:       make(chan watch.Event),
		stopChan:     make(chan any),
		debounceTime: 200 * time.Millisecond,
	}

	// Leave the object without a receiver. This is the state reached when its
	// worker exits on context cancellation just before Start forwards an event.
	ns := d.addNs("default")
	object := ns.add("example", d.output)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.Start(ctx) }()

	event := watch.Event{
		Type: watch.Modified,
		Object: &v1.Service{ObjectMeta: metav1.ObjectMeta{
			Name: "example", Namespace: "default",
		}},
	}
	sent := make(chan struct{})
	go func() {
		input <- event
		close(sent)
	}()
	select {
	case <-sent:
	case <-time.After(250 * time.Millisecond):
		cancel()
		t.Fatal("debouncer did not receive the test event")
	}
	cancel()

	select {
	case <-done:
		return
	case <-object.input:
		// Receiving here unblocks the production send. If cancellation had
		// interrupted that send, Start would already have returned above.
		select {
		case <-done:
		case <-time.After(250 * time.Millisecond):
			t.Fatal("debouncer remained blocked forwarding an event after context cancellation")
		}
		t.Fatal("debouncer forwarded an event after context cancellation")
	case <-time.After(250 * time.Millisecond):
		t.Fatal("debouncer did not return after context cancellation")
	}
}

func TestStartRecreatesObjectAfterDeletionWithoutCancellation(t *testing.T) {
	previousProcs := runtime.GOMAXPROCS(1)
	t.Cleanup(func() { runtime.GOMAXPROCS(previousProcs) })

	input := make(chan watch.Event)
	d, err := New(input, "200ms")
	if err != nil {
		t.Fatalf("failed to create debouncer: %s", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	done := make(chan error, 1)
	go func() { done <- d.Start(ctx) }()

	service := func(eventType watch.EventType, resourceVersion string) watch.Event {
		return watch.Event{
			Type: eventType,
			Object: &v1.Service{ObjectMeta: metav1.ObjectMeta{
				Name:            "example",
				Namespace:       "default",
				ResourceVersion: resourceVersion,
			}},
		}
	}

	send := func(event watch.Event) {
		t.Helper()
		select {
		case input <- event:
		case <-time.After(time.Second):
			t.Fatal("debouncer did not receive the test event")
		}
	}

	send(service(watch.Added, "initial"))
	select {
	case event := <-d.output:
		if event.Type != watch.Added {
			t.Fatalf("expected initial Added event, got %s", event.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("debouncer did not emit the initial event")
	}

	eventNs, exists := d.getNs("default")
	if !exists {
		t.Fatal("debouncer did not create the namespace map")
	}
	oldObject, exists := eventNs.get("example")
	if !exists {
		t.Fatal("debouncer did not create the object")
	}

	send(service(watch.Deleted, "deleted"))
	select {
	case event := <-d.output:
		if event.Type != watch.Deleted {
			t.Fatalf("expected Deleted event, got %s", event.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("debouncer did not emit the Deleted event")
	}

	select {
	case <-oldObject.stopChan:
	case <-time.After(time.Second):
		t.Fatal("object did not self-terminate")
	}
	if _, exists := eventNs.get("example"); exists {
		t.Fatal("self-terminated object remained in the namespace map")
	}

	send(service(watch.Modified, "fresh"))
	select {
	case event := <-d.output:
		if event.Type != watch.Modified {
			t.Fatalf("expected fresh Modified event, got %s", event.Type)
		}
		service, ok := event.Object.(*v1.Service)
		if !ok {
			t.Fatalf("expected a Service event, got %T", event.Object)
		}
		if service.ResourceVersion != "fresh" {
			t.Fatalf("expected the fresh event, got resource version %q", service.ResourceVersion)
		}
	case <-time.After(time.Second):
		t.Fatal("debouncer did not process the fresh event after object deletion")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("debouncer returned an error: %s", err)
		}
	case <-time.After(time.Second):
		t.Fatal("debouncer did not stop")
	}
}
