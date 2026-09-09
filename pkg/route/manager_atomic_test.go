package route

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

var errTransientRouteAdd = errors.New("transient route add failure")

func TestManagerRetriesRouteAfterInitialAddFailure(t *testing.T) {
	m := NewManager()
	r := &mockRoute{hash: "retry", addErr: errTransientRouteAdd}

	if err := m.Add("service", r, false, true); !errors.Is(err, errTransientRouteAdd) {
		t.Fatalf("first add error = %v, want %v", err, errTransientRouteAdd)
	}

	// A transient netlink failure (for example while the link is being recreated)
	// must not poison the in-memory tracker. The next reconciliation has to retry
	// the kernel operation.
	r.addErr = nil
	r.added = true
	if err := m.Add("service", r, false, true); err != nil {
		t.Fatalf("retry add failed: %v", err)
	}
	if r.addCalls != 2 {
		t.Fatalf("AddRoute called %d times, want 2", r.addCalls)
	}
}

func TestManagerConcurrentLifecycle(t *testing.T) {
	m := NewManager()
	var wg sync.WaitGroup
	for i := range 20 {
		r := concurrentRoute{hash: fmt.Sprintf("route-%d", i)}
		wg.Go(func() {
			for j := range 100 {
				object := fmt.Sprintf("service-%d", j)
				if err := m.Add(object, r, false, false); err != nil {
					t.Errorf("adding route: %v", err)
				}
				m.Check(r.RouteHash())
				if err := m.Delete(object, r); err != nil {
					t.Errorf("deleting route: %v", err)
				}
				if j%10 == 0 {
					m.Clear()
				}
			}
		})
	}
	wg.Wait()
}

type concurrentRoute struct{ hash string }

func (r concurrentRoute) AddRoute(bool) (bool, error) { return true, nil }
func (r concurrentRoute) UpdateRoutes() (bool, error) { return false, nil }
func (r concurrentRoute) DeleteRoute() error          { return nil }
func (r concurrentRoute) RouteHash() string           { return r.hash }
func (concurrentRoute) Interface() string             { return "test" }
