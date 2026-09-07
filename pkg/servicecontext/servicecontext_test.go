package servicecontext

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func resetReadiness(ctx *Context) bool {
	generation, _, _, ready := ctx.ReadinessState()
	return ready && ctx.ResetReadinessGeneration(generation)
}

func TestReadinessResetCreatesNewGeneration(t *testing.T) {
	svcCtx := New(context.Background())
	defer svcCtx.Cancel()

	firstGeneration, first, firstLost, firstReady := svcCtx.ReadinessState()
	if firstGeneration != 1 || firstReady {
		t.Fatalf("initial readiness state = generation %d, ready %t; want generation 1, ready false", firstGeneration, firstReady)
	}
	svcCtx.SignalReadiness()
	select {
	case <-first:
	default:
		t.Fatal("first readiness generation was not signalled")
	}

	if !resetReadiness(svcCtx) {
		t.Fatal("first readiness generation was not reset")
	}
	select {
	case <-firstLost:
	default:
		t.Fatal("first readiness generation loss was not signalled")
	}

	secondGeneration, second, secondLost, secondReady := svcCtx.ReadinessState()
	if secondGeneration != firstGeneration+1 || secondReady {
		t.Fatalf("reset readiness state = generation %d, ready %t; want generation %d, ready false", secondGeneration, secondReady, firstGeneration+1)
	}
	if first == second {
		t.Fatal("readiness reset reused the previous generation")
	}
	if firstLost == secondLost {
		t.Fatal("readiness reset reused the previous loss signal")
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

func TestParentSurvivesServiceCancellation(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	svcCtx := New(parent)
	svcCtx.Cancel()
	if parent.Err() != nil {
		t.Fatal("cancelling a service context cancelled its parent")
	}
}

func TestResetReadinessGenerationWaitsForActivation(t *testing.T) {
	svcCtx := New(context.Background())
	defer svcCtx.Cancel()
	svcCtx.SignalReadiness()
	generation, _, _, _ := svcCtx.ReadinessState()
	release, acquired := svcCtx.WaitForReadiness()
	if !acquired {
		t.Fatal("ready generation was not available for activation")
	}

	resetComplete := make(chan bool, 1)
	go func() {
		resetComplete <- svcCtx.ResetReadinessGeneration(generation)
	}()

	deadline := time.Now().Add(time.Second)
	for {
		currentGeneration, _, _, _ := svcCtx.ReadinessState()
		if currentGeneration != generation {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("readiness reset did not start")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-resetComplete:
		t.Fatal("readiness reset returned before the activation released its generation")
	default:
	}

	release()
	select {
	case reset := <-resetComplete:
		if !reset {
			t.Fatal("readiness reset rejected its current generation")
		}
	case <-time.After(time.Second):
		t.Fatal("readiness reset did not complete after activation released its generation")
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

func TestCancelledContextCannotStartWatching(t *testing.T) {
	svcCtx := New(context.Background())
	svcCtx.Cancel()
	if svcCtx.StartWatching() {
		t.Fatal("cancelled Service context acquired watcher ownership")
	}
}

func TestWaitForWatchingStopped(t *testing.T) {
	svcCtx := New(context.Background())
	defer svcCtx.Cancel()
	if !svcCtx.StartWatching() {
		t.Fatal("watcher ownership was not acquired")
	}

	done := make(chan error, 1)
	go func() {
		done <- svcCtx.WaitForWatchingStopped(context.Background())
	}()
	select {
	case <-done:
		t.Fatal("WaitForWatchingStopped returned while the watcher was active")
	case <-time.After(20 * time.Millisecond):
	}

	svcCtx.StopWatching()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WaitForWatchingStopped() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("WaitForWatchingStopped did not return after watcher shutdown")
	}
}

func TestConcurrentStateTransitions(t *testing.T) {
	svcCtx := New(context.Background())
	defer svcCtx.Cancel()

	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			svcCtx.SignalReadiness()
			resetReadiness(svcCtx)
		})
	}
	wg.Wait()
	if svcCtx.IsReady() && !resetReadiness(svcCtx) {
		t.Fatal("final readiness generation was not reset")
	}

	generation, ready, lost, isReady := svcCtx.ReadinessState()
	if isReady {
		t.Fatal("concurrent transitions left the context ready after every signal was reset")
	}
	if generation == 1 {
		t.Fatal("concurrent transitions did not advance the readiness generation")
	}
	select {
	case <-ready:
		t.Fatal("current readiness channel was already closed")
	default:
	}
	select {
	case <-lost:
		t.Fatal("current readiness-loss channel was already closed")
	default:
	}
}
