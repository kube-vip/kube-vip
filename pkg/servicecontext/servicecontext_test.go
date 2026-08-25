package servicecontext

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
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
