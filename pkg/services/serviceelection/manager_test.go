package serviceelection

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// stateHook runs before the ServiceState answers one IsCurrent call. Tests use
// it to inject endpoint-watcher transitions at the exact point where the
// Manager validates a readiness generation.
type stateHook func(*v1.Service, *servicecontext.Context, servicecontext.ReadinessGeneration)

// hookedState wraps a ServiceState and runs a hook before selected IsCurrent
// calls, numbered from 1 in call order.
type hookedState struct {
	ServiceState
	mutex sync.Mutex
	calls int
	hooks map[int]stateHook
	fired map[int]chan struct{}
}

func newHookedState(state ServiceState) *hookedState {
	return &hookedState{ServiceState: state, hooks: make(map[int]stateHook), fired: make(map[int]chan struct{})}
}

// at registers hook for the given IsCurrent call and returns a channel that is
// closed once the hook has run.
func (s *hookedState) at(call int, hook stateHook) <-chan struct{} {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.hooks[call] = hook
	s.fired[call] = make(chan struct{})
	return s.fired[call]
}

func (s *hookedState) IsCurrent(service *v1.Service, svcCtx *servicecontext.Context,
	generation servicecontext.ReadinessGeneration) bool {
	s.mutex.Lock()
	s.calls++
	hook, fired := s.hooks[s.calls], s.fired[s.calls]
	s.mutex.Unlock()
	if hook != nil {
		hook(service, svcCtx, generation)
		close(fired)
	}
	return s.ServiceState.IsCurrent(service, svcCtx, generation)
}

// resetReadiness simulates the endpoint watcher losing all endpoints.
func resetReadiness(_ *v1.Service, svcCtx *servicecontext.Context, generation servicecontext.ReadinessGeneration) {
	svcCtx.ResetReadinessGeneration(generation)
}

// cancelServiceContext simulates the Service being deleted or replaced.
func cancelServiceContext(_ *v1.Service, svcCtx *servicecontext.Context, _ servicecontext.ReadinessGeneration) {
	svcCtx.Cancel()
}

// watchHarness runs Manager.Watch for one Service against test doubles.
type watchHarness struct {
	t       *testing.T
	manager *Manager
	adapter *testAdapter
	state   *hookedState
	leases  *lease.Manager
	service *v1.Service
	svcCtx  *servicecontext.Context
	done    chan struct{}
}

func newWatchHarness(t *testing.T) *watchHarness {
	t.Helper()
	adapter := &testAdapter{current: make(map[types.UID]*servicecontext.Context)}
	state := newHookedState(adapter)
	leases := lease.NewManager()
	manager, err := NewManager(&Dependencies{
		Config: &kubevip.Config{}, Leases: leases,
		State: state, Datapath: adapter, Runner: adapter, Scheduler: adapter,
	})
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: "default", UID: "service"},
		Spec: v1.ServiceSpec{LoadBalancerIP: "192.0.2.10"}}
	svcCtx := servicecontext.New(context.Background())
	adapter.current[service.UID] = svcCtx
	h := &watchHarness{
		t: t, manager: manager, adapter: adapter, state: state, leases: leases,
		service: service, svcCtx: svcCtx, done: make(chan struct{}),
	}
	t.Cleanup(h.stop)
	return h
}

func (h *watchHarness) leaseID() lease.ID {
	namespace, name := lease.ServiceName(h.service)
	return lease.NewID(h.manager.config.LeaderElectionType, namespace, name)
}

func (h *watchHarness) start() {
	go func() {
		defer close(h.done)
		h.manager.Watch(h.svcCtx, h.service)
	}()
}

// requireRunning fails if Watch returns within the observation window. The
// window exceeds restartBaseDelay so a retry path cannot mask an early exit.
func (h *watchHarness) requireRunning(reason string) {
	h.t.Helper()
	select {
	case <-h.done:
		h.t.Fatalf("Watch returned although the Service context is still live: %s", reason)
	case <-time.After(restartBaseDelay + 100*time.Millisecond):
	}
}

func (h *watchHarness) requireStopped(reason string) {
	h.t.Helper()
	select {
	case <-h.done:
	case <-time.After(time.Second):
		h.t.Fatalf("Watch did not return: %s", reason)
	}
}

func (h *watchHarness) waitFired(fired <-chan struct{}, hook string) {
	h.t.Helper()
	select {
	case <-fired:
	case <-time.After(time.Second):
		h.t.Fatalf("%s hook did not run", hook)
	}
}

func (h *watchHarness) activations() int {
	h.adapter.mutex.Lock()
	defer h.adapter.mutex.Unlock()
	return len(h.adapter.activations)
}

func (h *watchHarness) requireActivations(want int, reason string) {
	h.t.Helper()
	waitForAdapterCount(h.t, h.adapter, func(adapter *testAdapter) int { return len(adapter.activations) },
		want, reason)
}

func (h *watchHarness) stop() {
	h.svcCtx.Cancel()
	select {
	case <-h.done:
	case <-time.After(time.Second):
		h.t.Error("Watch did not return after Service context cancellation")
		return
	}
	h.manager.Wait()
}

// Join validates a generation twice: before admitting the member (call 1) and
// after the coordinator admitted it (call 2). Losing endpoints at either point
// obsoletes only that generation; Watch must wait for the next one.
func TestWatchContinuesAfterGenerationObsoletedDuringJoin(t *testing.T) {
	tests := []struct {
		name string
		call int
	}{
		{name: "before coordinator admission", call: 1},
		{name: "after coordinator admission", call: 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newWatchHarness(t)
			fired := h.state.at(test.call, resetReadiness)
			h.svcCtx.SignalReadiness()
			h.start()

			h.waitFired(fired, "readiness reset")
			h.requireRunning("generation became obsolete during join")
			if got := h.activations(); got != 0 {
				t.Fatalf("obsolete generation activated %d times", got)
			}
			if coordinator := h.manager.coordinatorMgr.current(h.leaseID()); coordinator != nil {
				t.Fatal("obsolete generation left a coordinator registered")
			}
			if svcLease := h.leases.Get(h.leaseID()); svcLease != nil {
				t.Fatal("obsolete generation left its Lease registered")
			}

			h.svcCtx.SignalReadiness()
			h.requireActivations(1, "activation of the next ready generation")
		})
	}
}

func TestWatchSurvivesRepeatedReadinessFlapping(t *testing.T) {
	h := newWatchHarness(t)
	firstReset := h.state.at(1, resetReadiness)
	secondReset := h.state.at(2, resetReadiness)
	h.svcCtx.SignalReadiness()
	h.start()

	h.waitFired(firstReset, "first readiness reset")
	h.svcCtx.SignalReadiness()
	h.waitFired(secondReset, "second readiness reset")
	h.requireRunning("two consecutive generations became obsolete during join")

	h.svcCtx.SignalReadiness()
	h.requireActivations(1, "activation after readiness settled")
}

func TestWatchStopsWhenServiceContextCancelledDuringJoin(t *testing.T) {
	h := newWatchHarness(t)
	fired := h.state.at(1, cancelServiceContext)
	h.svcCtx.SignalReadiness()
	h.start()

	h.waitFired(fired, "context cancellation")
	h.requireStopped("Service context was cancelled during join")
	if got := h.activations(); got != 0 {
		t.Fatalf("cancelled Service context activated %d times", got)
	}
}
