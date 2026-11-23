package serviceelection

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func sharedLeaseService(name string) *v1.Service {
	return &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: "default", UID: types.UID(name),
		Annotations: map[string]string{kubevip.ServiceLease: "shared"},
	}}
}

// failActivations makes the first `times` activations of each listed Service
// fail. A negative value fails every attempt.
func failActivations(adapter *testAdapter, times int, services ...*v1.Service) {
	failing := make(map[types.UID]bool, len(services))
	for _, service := range services {
		failing[service.UID] = true
	}
	var mutex sync.Mutex
	failures := make(map[types.UID]int)
	adapter.mutex.Lock()
	defer adapter.mutex.Unlock()
	adapter.activate = func(service *v1.Service) error {
		if !failing[service.UID] {
			return nil
		}
		mutex.Lock()
		defer mutex.Unlock()
		if times >= 0 && failures[service.UID] >= times {
			return nil
		}
		failures[service.UID]++
		return errors.New("datapath failure")
	}
}

func activationAttempts(adapter *testAdapter, service *v1.Service) int {
	adapter.mutex.Lock()
	defer adapter.mutex.Unlock()
	attempts := 0
	for _, activated := range adapter.activations {
		if activated.UID == service.UID {
			attempts++
		}
	}
	return attempts
}

// leadSharedCampaign creates the coordinator's campaign and drives it through
// the leadership path synchronously, activating every current member.
func leadSharedCampaign(c *coordinator, wg *sync.WaitGroup) campaignCandidate {
	start := c.newCampaignCandidate()
	c.startedLeading(context.Background(), start.lease, start.campaign, wg)
	return start
}

func closeMembers(members ...*member) {
	for _, member := range members {
		member.coordinator.closeMember(member)
	}
}

func TestAdmittingMemberKeepsPendingRetryOfFailedSibling(t *testing.T) {
	scheduler := &queuedRestartScheduler{}
	manager, adapter, _ := newTestManagerWithScheduler(scheduler)
	healthyService, failingService := sharedLeaseService("healthy"), sharedLeaseService("failing")
	failActivations(adapter, 1, failingService)
	healthy := readyMember(t, manager, adapter, healthyService)
	failing := readyMember(t, manager, adapter, failingService)
	coordinator := healthy.coordinator
	var wg sync.WaitGroup
	leadSharedCampaign(coordinator, &wg)
	if got := scheduler.count(); got != 1 {
		t.Fatalf("scheduled retries = %d, want 1", got)
	}
	pending := scheduler.next(t)

	joinerService := sharedLeaseService("joiner")
	joiner := readyMember(t, manager, adapter, joinerService)
	coordinator.admitToCampaign(&wg, joiner)

	if got := activationAttempts(adapter, failingService); got != 1 {
		t.Fatalf("failed member attempts after admission = %d, want 1 (backoff bypassed)", got)
	}
	if pending.ctx.Err() != nil {
		t.Fatal("admission cancelled the failed member's pending retry")
	}
	if !joiner.active {
		t.Fatal("admitted member was not activated")
	}

	scheduler.runNext(t)
	if got := activationAttempts(adapter, failingService); got != 2 {
		t.Fatalf("failed member attempts after its retry = %d, want 2", got)
	}
	if !failing.active {
		t.Fatal("pending retry did not activate the failed member")
	}
	closeMembers(healthy, failing, joiner)
}

func TestAdmittingMemberActivatesOnlyAdmittedMember(t *testing.T) {
	manager, adapter, _ := newTestManager()
	first := readyMember(t, manager, adapter, sharedLeaseService("first"))
	second := readyMember(t, manager, adapter, sharedLeaseService("second"))
	coordinator := first.coordinator
	var wg sync.WaitGroup
	leadSharedCampaign(coordinator, &wg)

	joiner := readyMember(t, manager, adapter, sharedLeaseService("joiner"))
	coordinator.admitToCampaign(&wg, joiner)

	for _, member := range []*member{first, second, joiner} {
		if got := activationAttempts(adapter, member.service); got != 1 {
			t.Fatalf("%s activation attempts = %d, want 1", member.service.Name, got)
		}
		if !member.active {
			t.Fatalf("%s is not active", member.service.Name)
		}
	}
	closeMembers(first, second, joiner)
}

func TestAdmittedMemberFailureWithActiveSiblingSchedulesOwnRetry(t *testing.T) {
	scheduler := &queuedRestartScheduler{}
	manager, adapter, _ := newTestManagerWithScheduler(scheduler)
	joinerService := sharedLeaseService("joiner")
	failActivations(adapter, 1, joinerService)
	healthy := readyMember(t, manager, adapter, sharedLeaseService("healthy"))
	coordinator := healthy.coordinator
	var wg sync.WaitGroup
	start := leadSharedCampaign(coordinator, &wg)

	joiner := readyMember(t, manager, adapter, joinerService)
	coordinator.admitToCampaign(&wg, joiner)

	if start.campaign.ctx.Err() != nil {
		t.Fatal("admitted member failure cancelled a campaign with an active sibling")
	}
	if got := scheduler.count(); got != 1 {
		t.Fatalf("scheduled retries = %d, want 1", got)
	}
	scheduler.runNext(t)
	if !joiner.active || !healthy.active {
		t.Fatalf("after retry: joiner active=%v healthy active=%v, want both active", joiner.active, healthy.active)
	}
	if got := activationAttempts(adapter, healthy.service); got != 1 {
		t.Fatalf("healthy member attempts = %d, want 1", got)
	}
	closeMembers(healthy, joiner)
}

func TestAdmittedMemberFailureWithoutActiveMemberCancelsCampaign(t *testing.T) {
	scheduler := &queuedRestartScheduler{}
	manager, adapter, _ := newTestManagerWithScheduler(scheduler)
	failingService, joinerService := sharedLeaseService("failing"), sharedLeaseService("joiner")
	failActivations(adapter, -1, failingService, joinerService)
	healthy := readyMember(t, manager, adapter, sharedLeaseService("healthy"))
	failing := readyMember(t, manager, adapter, failingService)
	coordinator := healthy.coordinator
	var wg sync.WaitGroup
	start := leadSharedCampaign(coordinator, &wg)
	if got := scheduler.count(); got != 1 {
		t.Fatalf("scheduled retries = %d, want 1", got)
	}

	// The only active member leaves; the failed member still waits for its retry.
	coordinator.closeMember(healthy)
	joiner := readyMember(t, manager, adapter, joinerService)
	coordinator.admitToCampaign(&wg, joiner)

	if start.campaign.ctx.Err() == nil {
		t.Fatal("campaign kept leadership although no member is active")
	}
	if got := coordinator.campaigns.restartFailures; got != 1 {
		t.Fatalf("restartFailures = %d, want 1", got)
	}
	closeMembers(failing, joiner)
}

func TestMemberAdmittedDuringLeadershipStartIsActivatedOnce(t *testing.T) {
	for iteration := range 100 {
		manager, adapter, _ := newTestManager()
		first := readyMember(t, manager, adapter, sharedLeaseService("first"))
		coordinator := first.coordinator
		var wg sync.WaitGroup
		start := coordinator.newCampaignCandidate()

		leadershipDone := make(chan struct{})
		go func() {
			defer close(leadershipDone)
			coordinator.startedLeading(context.Background(), start.lease, start.campaign, &wg)
		}()
		joiner := readyMember(t, manager, adapter, sharedLeaseService("joiner"))
		coordinator.admitToCampaign(&wg, joiner)
		<-leadershipDone

		if got := activationAttempts(adapter, joiner.service); got != 1 {
			t.Fatalf("iteration %d: admitted member activation attempts = %d, want exactly 1", iteration, got)
		}
		if !joiner.active {
			t.Fatalf("iteration %d: admitted member is not active", iteration)
		}
		closeMembers(first, joiner)
	}
}

func TestRestartJoiningLeadingCampaignDoesNotActivateMembers(t *testing.T) {
	scheduler := &queuedRestartScheduler{}
	manager, adapter, _ := newTestManagerWithScheduler(scheduler)
	failingService := sharedLeaseService("failing")
	failActivations(adapter, -1, failingService)
	healthy := readyMember(t, manager, adapter, sharedLeaseService("healthy"))
	failing := readyMember(t, manager, adapter, failingService)
	coordinator := healthy.coordinator
	var wg sync.WaitGroup
	leadSharedCampaign(coordinator, &wg)
	pending := scheduler.next(t)

	coordinator.startCampaign(&wg)

	if got := activationAttempts(adapter, failingService); got != 1 {
		t.Fatalf("restart re-attempted the failed member: attempts = %d, want 1", got)
	}
	if got := activationAttempts(adapter, healthy.service); got != 1 {
		t.Fatalf("restart re-attempted the active member: attempts = %d, want 1", got)
	}
	if pending.ctx.Err() != nil {
		t.Fatal("restart cancelled the failed member's pending retry")
	}
	if got := scheduler.count(); got != 1 {
		t.Fatalf("scheduled retries after restart = %d, want 1", got)
	}
	closeMembers(healthy, failing)
}

func TestAdmissionsValidateEachMemberLinearly(t *testing.T) {
	const memberCount = 10
	adapter := &testAdapter{current: make(map[types.UID]*servicecontext.Context)}
	state := newHookedState(adapter)
	manager, err := NewManager(&Dependencies{
		Config: &kubevip.Config{}, Leases: lease.NewManager(),
		State: state, Datapath: adapter, Runner: adapter, Scheduler: adapter,
	})
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	members := []*member{readyMember(t, manager, adapter, sharedLeaseService("member-0"))}
	coordinator := members[0].coordinator
	var wg sync.WaitGroup
	leadSharedCampaign(coordinator, &wg)
	for index := 1; index < memberCount; index++ {
		joiner := readyMember(t, manager, adapter, sharedLeaseService(fmt.Sprintf("member-%d", index)))
		coordinator.admitToCampaign(&wg, joiner)
		members = append(members, joiner)
	}

	// Each member costs two checks to join and two to activate.
	state.mutex.Lock()
	calls := state.calls
	state.mutex.Unlock()
	if want := 4 * memberCount; calls > want {
		t.Fatalf("IsCurrent calls = %d for %d members, want at most %d", calls, memberCount, want)
	}
	for _, member := range members {
		if got := activationAttempts(adapter, member.service); got != 1 {
			t.Fatalf("%s activation attempts = %d, want 1", member.service.Name, got)
		}
	}
	closeMembers(members...)
}
