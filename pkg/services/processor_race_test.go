package services

import (
	"context"
	"sync"
	"testing"

	"github.com/kube-vip/kube-vip/pkg/servicecontext"
)

func TestWatchedFlagConcurrentWithWatcherTeardown(t *testing.T) {
	svcCtx := servicecontext.New(context.Background())
	start := make(chan struct{})
	var wg sync.WaitGroup

	wg.Go(func() {
		<-start
		for range 1000 {
			svcCtx.SetWatched(false)
		}
	})
	wg.Go(func() {
		<-start
		for range 1000 {
			if !svcCtx.IsWatchedLocked() {
				svcCtx.SetWatched(true)
			}
		}
	})

	close(start)
	wg.Wait()
}
