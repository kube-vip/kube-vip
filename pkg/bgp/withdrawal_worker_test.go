package bgp

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osrg/gobgp/v4/pkg/apiutil"
)

func TestWithdrawalWorkerSkipsCanceledQueuedRequest(t *testing.T) {
	b := &Server{deletePathFunc: func(apiutil.DeletePathRequest) error {
		t.Fatal("canceled request reached GoBGP")
		return nil
	}}
	b.startWithdrawalWorker()
	t.Cleanup(func() {
		close(b.withdrawStop)
		<-b.withdrawDone
	})

	result := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b.withdrawQueue <- withdrawalRequest{ctx: ctx, result: result}

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled request was not completed")
	}
}

func TestWithdrawalWorkerUsesBoundedQueueAndSingleBackendCall(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	var releaseOnce sync.Once
	b := &Server{deletePathFunc: func(apiutil.DeletePathRequest) error {
		calls.Add(1)
		<-release
		return nil
	}}
	b.startWithdrawalWorker()
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		close(b.withdrawStop)
		<-b.withdrawDone
	})

	first := make(chan error, 1)
	b.withdrawQueue <- withdrawalRequest{ctx: context.Background(), result: first}
	deadline := time.After(time.Second)
	for calls.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("worker did not start backend call")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	for range withdrawalQueueSize {
		b.withdrawQueue <- withdrawalRequest{ctx: context.Background(), result: make(chan error, 1)}
	}
	if got := len(b.withdrawQueue); got != withdrawalQueueSize {
		t.Fatalf("queue length = %d, want %d", got, withdrawalQueueSize)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("backend calls while first is blocked = %d, want 1", got)
	}
	releaseOnce.Do(func() { close(release) })
	if err := <-first; err != nil {
		t.Fatalf("first withdrawal: %v", err)
	}
}
