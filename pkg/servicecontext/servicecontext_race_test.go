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
			resetReadiness(ctx)
		}
	})
	wg.Go(func() {
		<-start
		for range 1000 {
			_, ready, _, _ := ctx.ReadinessState()
			select {
			case <-ready:
			default:
			}
		}
	})

	close(start)
	wg.Wait()
}
