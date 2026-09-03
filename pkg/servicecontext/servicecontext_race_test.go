package servicecontext

import (
	"context"
	"sync"
	"testing"
)

func TestReadinessResetConcurrentWithSignal(t *testing.T) {
	ctx := New(context.Background())
	start := make(chan struct{})
	var wg sync.WaitGroup

	wg.Go(func() {
		<-start
		for range 1000 {
			ctx.SignalReadiness()
			ctx.ResetReadiness()
		}
	})
	wg.Go(func() {
		<-start
		for range 1000 {
			ready := ctx.GetEndpointsReady()
			select {
			case <-ready:
			default:
			}
		}
	})

	close(start)
	wg.Wait()
}

func TestLeaderCancelConcurrentWithEndpointCleanup(t *testing.T) {
	ctx := New(context.Background())
	start := make(chan struct{})
	var wg sync.WaitGroup

	wg.Go(func() {
		<-start
		for range 1000 {
			ctx.SetLeaderCancel(func() {})
		}
	})
	wg.Go(func() {
		<-start
		for range 1000 {
			ctx.CallLeaderCancel()
		}
	})

	close(start)
	wg.Wait()
}
