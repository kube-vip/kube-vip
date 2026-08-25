package servicecontext

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestReadinessResetCreatesNewGeneration(t *testing.T) {
	svcCtx := New(context.Background())
	defer svcCtx.Cancel()

	first := svcCtx.Readiness()
	svcCtx.SignalReadiness()
	select {
	case <-first:
	default:
		t.Fatal("first readiness generation was not signalled")
	}

	svcCtx.ResetReadiness()
	second := svcCtx.Readiness()
	if first == second {
		t.Fatal("readiness reset reused the previous generation")
	}
	select {
	case <-second:
		t.Fatal("new readiness generation was already signalled")
	default:
	}

	svcCtx.SignalReadiness()
	select {
	case <-second:
	default:
		t.Fatal("second readiness generation was not signalled")
	}
}

func TestStartWatchingClaimsOnce(t *testing.T) {
	svcCtx := New(context.Background())
	defer svcCtx.Cancel()

	var claims atomic.Int64
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			if svcCtx.StartWatching() {
				claims.Add(1)
			}
		})
	}
	wg.Wait()

	if got := claims.Load(); got != 1 {
		t.Fatalf("watcher claims = %d, want 1", got)
	}
	svcCtx.StopWatching()
	if !svcCtx.StartWatching() {
		t.Fatal("watcher ownership was not released")
	}
}

func TestConcurrentStateTransitions(t *testing.T) {
	svcCtx := New(context.Background())
	defer svcCtx.Cancel()

	var cancelCalls atomic.Int64
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			svcCtx.SignalReadiness()
			svcCtx.ResetReadiness()
		})
		wg.Go(func() {
			svcCtx.SetLeaderCancel(func() {
				cancelCalls.Add(1)
			})
			svcCtx.CancelLeader()
		})
	}
	wg.Wait()

	if cancelCalls.Load() == 0 {
		t.Fatal("leader cancellation was never invoked")
	}
}

func TestCancelLeaderCancelsCallbackInstalledDuringCancellation(t *testing.T) {
	svcCtx := New(context.Background())
	defer svcCtx.Cancel()

	oldCancelStarted := make(chan struct{})
	releaseOldCancel := make(chan struct{})
	svcCtx.SetLeaderCancel(func() {
		close(oldCancelStarted)
		<-releaseOldCancel
	})

	cancelComplete := make(chan struct{})
	go func() {
		svcCtx.CancelLeader()
		close(cancelComplete)
	}()
	<-oldCancelStarted

	newCancelCalled := make(chan struct{})
	svcCtx.SetLeaderCancel(func() {
		close(newCancelCalled)
	})
	select {
	case <-newCancelCalled:
	case <-time.After(time.Second):
		t.Fatal("leader cancel installed during cancellation was not called")
	}

	close(releaseOldCancel)
	<-cancelComplete

	laterCancelCalled := make(chan struct{})
	svcCtx.SetLeaderCancel(func() {
		close(laterCancelCalled)
	})
	select {
	case <-laterCancelCalled:
		t.Fatal("pending cancellation was applied more than once")
	case <-time.After(10 * time.Millisecond):
	}
}

func TestCancelLeaderDoesNotCancelNextInstalledCallback(t *testing.T) {
	svcCtx := New(context.Background())
	defer svcCtx.Cancel()

	firstCancelCalled := make(chan struct{})
	svcCtx.SetLeaderCancel(func() {
		close(firstCancelCalled)
	})
	svcCtx.CancelLeader()
	<-firstCancelCalled

	nextCancelCalled := make(chan struct{})
	svcCtx.SetLeaderCancel(func() {
		close(nextCancelCalled)
	})
	select {
	case <-nextCancelCalled:
		t.Fatal("canceling an installed leader callback canceled the next election round")
	case <-time.After(10 * time.Millisecond):
	}
}
