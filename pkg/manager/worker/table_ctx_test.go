package worker

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
)

func TestConfigureCleanRoutingTableStopsWithContext(t *testing.T) {
	t.Parallel()

	var wg sync.WaitGroup
	table := &Table{Common: Common{
		config: &kubevip.Config{
			CleanRoutingTable:  true,
			EnableControlPlane: true,
			Address:            "10.254.254.254",
			RoutingTableID:     0x7fffffff,
			RoutingProtocol:    255,
		},
		mutex: &sync.Mutex{},
	}}
	ctx, cancel := context.WithCancel(context.Background())

	if err := table.Configure(ctx, &wg); err != nil {
		t.Fatalf("Configure returned an error: %v", err)
	}
	cancel()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		// DEFECT: pkg/manager/worker/table.go:43-48 uses an unconditional
		// 10-second sleep for cleanRoutingTable and ignores the canceled RT
		// worker context, delaying shutdown/recovery.
		t.Fatal("cleanRoutingTable worker did not stop after context cancellation")
	}
}
