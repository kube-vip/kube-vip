package cluster

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestStopConcurrentDoesNotRaceOrPanic(t *testing.T) {
	c := &Cluster{stop: make(chan bool)}
	start := make(chan struct{})
	var wg sync.WaitGroup
	var panics atomic.Int64

	for range 128 {
		wg.Go(func() {
			<-start
			defer func() {
				if recover() != nil {
					panics.Add(1)
				}
			}()
			c.Stop()
		})
	}

	close(start)
	wg.Wait()
	if got := panics.Load(); got != 0 {
		t.Fatalf("concurrent Stop panicked %d time(s)", got)
	}
}
