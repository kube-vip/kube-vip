package etcd

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

func TestObserverFailureBeforeCampaignSuccess(t *testing.T) {
	campaignCanceled := make(chan struct{})
	e := &fakeElection{
		campaign: func(ctx context.Context, _ string) error {
			<-ctx.Done()
			close(campaignCanceled)
			return ctx.Err()
		},
		observe: closedObserveChannel(),
	}

	err := testMember(e).run(context.Background(), make(chan struct{}))
	if err == nil || !strings.Contains(err.Error(), "leader observation ended") {
		t.Fatalf("run error = %v, want leader observation error", err)
	}
	select {
	case <-campaignCanceled:
	default:
		t.Fatal("campaign was not stopped before run returned")
	}
}

func TestCampaignErrorStopsWatcher(t *testing.T) {
	observe := make(chan *clientv3.GetResponse)
	observeCanceled := make(chan struct{})
	e := &fakeElection{
		campaign:        func(context.Context, string) error { return errors.New("campaign failed") },
		observe:         observe,
		observeCanceled: observeCanceled,
	}

	err := testMember(e).run(context.Background(), make(chan struct{}))
	if err == nil || !strings.Contains(err.Error(), "campaign failed") {
		t.Fatalf("run error = %v, want campaign error", err)
	}
	select {
	case <-observeCanceled:
	case <-time.After(time.Second):
		t.Fatal("watcher context was not canceled before run returned")
	}
}

func TestSessionLossStopsLeadership(t *testing.T) {
	observe := make(chan *clientv3.GetResponse)
	sessionDone := make(chan struct{})
	e := successfulFakeElection(observe)
	var started, stopped atomic.Int32
	leading := make(chan struct{})
	m := testMember(e)
	m.leaderDelay = 0
	m.callbacks.OnStartedLeading = func(ctx context.Context) {
		started.Add(1)
		close(leading)
		<-ctx.Done()
	}
	m.callbacks.OnStoppedLeading = func() { stopped.Add(1) }

	runDone := make(chan error, 1)
	go func() { runDone <- m.run(context.Background(), sessionDone) }()
	select {
	case <-leading:
	case <-time.After(time.Second):
		t.Fatal("leadership did not start")
	}
	close(sessionDone)

	err := receive(t, runDone)
	if err == nil || err.Error() != "election session ended" {
		t.Fatalf("run error = %v, want %q", err, "election session ended")
	}
	if got := started.Load(); got != 1 {
		t.Fatalf("OnStartedLeading calls = %d, want 1", got)
	}
	if got := stopped.Load(); got != 1 {
		t.Fatalf("OnStoppedLeading calls = %d, want 1", got)
	}
}

func TestCancellationStopsElectionWithoutStartingLeadership(t *testing.T) {
	observe := make(chan *clientv3.GetResponse)
	e := successfulFakeElection(observe)
	var started atomic.Int32
	m := testMember(e)
	m.callbacks.OnStartedLeading = func(context.Context) { started.Add(1) }
	ctx, cancel := context.WithCancel(context.Background())

	runDone := make(chan error, 1)
	go func() { runDone <- m.run(ctx, make(chan struct{})) }()
	waitFor(t, &e.keyCalls, 1)
	cancel()

	if err := receive(t, runDone); err != nil {
		t.Fatalf("run error = %v, want nil for caller cancellation", err)
	}
	if got := started.Load(); got != 0 {
		t.Fatalf("OnStartedLeading calls = %d, want 0", got)
	}
}

func TestCancellationStopsBlockedCampaign(t *testing.T) {
	observe := make(chan *clientv3.GetResponse)
	e := &fakeElection{
		campaign: func(ctx context.Context, _ string) error {
			<-ctx.Done()
			return ctx.Err()
		},
		observe: observe,
	}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- testMember(e).run(ctx, make(chan struct{})) }()
	waitFor(t, &e.observeCalls, 1)
	cancel()

	if err := receive(t, runDone); err != nil {
		t.Fatalf("run error = %v, want nil for caller cancellation", err)
	}
}

func TestObserverTerminationRacingCampaignNeverStartsLeadership(t *testing.T) {
	for i := 0; i < 100; i++ {
		observe := make(chan *clientv3.GetResponse)
		campaignRelease := make(chan struct{})
		e := &fakeElection{
			campaign: func(context.Context, string) error {
				<-campaignRelease
				return nil
			},
			key:     "/election/member",
			observe: observe,
		}
		var started atomic.Int32
		m := testMember(e)
		m.callbacks.OnStartedLeading = func(context.Context) { started.Add(1) }

		runDone := make(chan error, 1)
		go func() { runDone <- m.run(context.Background(), make(chan struct{})) }()
		waitFor(t, &e.observeCalls, 1)
		close(observe)
		waitFor(t, &m.state, 2)
		close(campaignRelease)
		if err := receive(t, runDone); err == nil {
			t.Fatal("run returned nil after observer termination")
		}
		if got := started.Load(); got != 0 {
			t.Fatalf("iteration %d: OnStartedLeading calls = %d, want 0", i, got)
		}
	}
}

type fakeElection struct {
	campaign        func(context.Context, string) error
	key             string
	observe         chan *clientv3.GetResponse
	observeCanceled chan struct{}
	keyCalls        atomic.Int32
	observeCalls    atomic.Int32
}

func (e *fakeElection) Campaign(ctx context.Context, memberID string) error {
	return e.campaign(ctx, memberID)
}

func (e *fakeElection) Key() string {
	e.keyCalls.Add(1)
	return e.key
}

func (e *fakeElection) Observe(ctx context.Context) <-chan *clientv3.GetResponse {
	e.observeCalls.Add(1)
	if e.observeCanceled != nil {
		go func() {
			<-ctx.Done()
			close(e.observeCanceled)
		}()
	}
	return e.observe
}

func successfulFakeElection(observe chan *clientv3.GetResponse) *fakeElection {
	return &fakeElection{
		campaign: func(context.Context, string) error { return nil },
		key:      "/election/member",
		observe:  observe,
	}
}

func testMember(e election) *member {
	return &member{
		election:    e,
		memberID:    "member",
		leaderDelay: time.Hour,
		callbacks: LeaderCallbacks{
			OnStartedLeading: func(context.Context) {},
			OnStoppedLeading: func() {},
			OnNewLeader:      func(string) {},
		},
	}
}

func closedObserveChannel() chan *clientv3.GetResponse {
	ch := make(chan *clientv3.GetResponse)
	close(ch)
	return ch
}

func waitFor(t *testing.T, value *atomic.Int32, want int32) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for value.Load() != want {
		if time.Now().After(deadline) {
			t.Fatalf("value = %d, want %d", value.Load(), want)
		}
		time.Sleep(time.Millisecond)
	}
}

func receive(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for election to stop")
		return nil
	}
}

var _ election = (*fakeElection)(nil)
