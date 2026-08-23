package route

import (
	"errors"
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
