package serviceelection

import (
	"context"
	"sync"
	"testing"

	"github.com/kube-vip/kube-vip/pkg/lease"
)

// blockingLeaseStore wraps the production lease manager and can pause one
// ClaimRegistration call after it has been entered. It is intended for tests
// that need to arrange a lease lifecycle transition at that exact point.
type blockingLeaseStore struct {
	manager *lease.Manager

	mutex sync.Mutex
	gate  *claimGate
}

type claimGate struct {
	entered chan struct{}
	release chan struct{}
	claimed bool
	once    sync.Once
}

func newBlockingLeaseStore(t *testing.T, manager *lease.Manager) *blockingLeaseStore {
	t.Helper()
	store := &blockingLeaseStore{manager: manager}
	t.Cleanup(store.releaseClaim)
	return store
}

// armNextClaim arranges for the next ClaimRegistration call to block. The
// returned channel is closed after that call reaches the blocking point.
func (s *blockingLeaseStore) armNextClaim() <-chan struct{} {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if s.gate != nil {
		panic("blockingLeaseStore: previous claim gate is still active")
	}
	s.gate = &claimGate{entered: make(chan struct{}), release: make(chan struct{})}
	return s.gate.entered
}

// releaseClaim unblocks the currently armed ClaimRegistration. It is safe to
// call more than once and is also used by test cleanup to prevent goroutine
// leaks when a test fails before releasing the claim explicitly.
func (s *blockingLeaseStore) releaseClaim() {
	s.mutex.Lock()
	gate := s.gate
	s.mutex.Unlock()
	if gate != nil {
		gate.once.Do(func() { close(gate.release) })
	}
}

func (s *blockingLeaseStore) AcquireRegistrations(ctx context.Context, id lease.ID,
	specs []lease.RegistrationSpec) (*lease.Lease, map[string]*lease.Registration, error) {
	return s.manager.AcquireRegistrations(ctx, id, specs)
}

func (s *blockingLeaseStore) ClaimRegistration(id lease.ID,
	spec lease.RegistrationSpec) (*lease.Registration, bool) {
	s.mutex.Lock()
	gate := s.gate
	if gate == nil || gate.claimed {
		s.mutex.Unlock()
		return s.manager.ClaimRegistration(id, spec)
	}
	gate.claimed = true
	s.mutex.Unlock()

	close(gate.entered)
	<-gate.release
	registration, added := s.manager.ClaimRegistration(id, spec)

	s.mutex.Lock()
	if s.gate == gate {
		s.gate = nil
	}
	s.mutex.Unlock()
	return registration, added
}
