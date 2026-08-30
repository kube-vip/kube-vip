package cluster

import (
	"sync"
	"testing"
	"time"
)

func TestStopWorkersAndWaitPreservingWaitsForGeneration(t *testing.T) {
	c := &Cluster{stop: make(chan struct{})}
	stop := c.stop
	workerDone := make(chan struct{})
	generation := &workerGeneration{stop: stop, done: workerDone}
	c.workers = generation
	unblock := make(chan struct{})
	go func() {
		<-stop
		<-unblock
		close(workerDone)
	}()
	done := make(chan struct{})
	go func() {
		c.StopWorkersAndWaitPreserving(map[string]struct{}{"*": {}})
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("StopWorkersAndWait returned before worker completion")
	case <-time.After(25 * time.Millisecond):
	}
	close(unblock)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("StopWorkersAndWait did not return after worker completion")
	}
	if _, ok := generation.preservedAddresses()["*"]; !ok {
		t.Fatal("shared generation did not preserve its VIP")
	}
}

func TestStopUpdatesPreservationUntilCleanupReadsIt(t *testing.T) {
	c := &Cluster{stop: make(chan struct{})}
	generation := &workerGeneration{stop: c.stop, done: make(chan struct{})}
	c.workers = generation
	unblock := make(chan struct{})
	go func() {
		<-generation.stop
		<-unblock
		close(generation.done)
	}()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		c.StopWorkersAndWaitPreserving(nil)
	}()
	<-generation.stop
	go func() {
		defer wg.Done()
		c.StopWorkersAndWaitPreserving(map[string]struct{}{"192.0.2.10": {}})
	}()
	close(unblock)
	wg.Wait()

	if _, ok := generation.preservedAddresses()["192.0.2.10"]; !ok {
		t.Fatal("cancellation cleanup did not observe the later preservation decision")
	}
}

func TestContextCancellationCanStillPreserveSharedVIP(t *testing.T) {
	c := &Cluster{stop: make(chan struct{})}
	generation := &workerGeneration{stop: c.stop, done: make(chan struct{})}
	c.workers = generation
	cleanup := make(chan struct{})
	go func() {
		<-cleanup
		if _, ok := generation.preservedAddresses()["192.0.2.10"]; !ok {
			t.Error("context cancellation cleanup missed shared VIP preservation")
		}
		c.finishWorkers(generation)
	}()

	generation.setPreserve(map[string]struct{}{"192.0.2.10": {}})
	close(cleanup)
	c.StopWorkersAndWaitPreserving(map[string]struct{}{"192.0.2.10": {}})
}

func TestFinishOldGenerationDoesNotClearReplacement(t *testing.T) {
	c := &Cluster{stop: make(chan struct{})}
	old := &workerGeneration{stop: c.stop, done: make(chan struct{})}
	replacement := &workerGeneration{stop: make(chan struct{}), done: make(chan struct{})}
	c.workers = replacement
	c.finishWorkers(old)
	if c.workers != replacement || !c.WorkersRunning() {
		t.Fatal("old generation completion cleared replacement workers")
	}
	c.finishWorkers(replacement)
}

func TestStopWorkersAndWaitWithoutWorkerReturns(t *testing.T) {
	c := &Cluster{stop: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		c.StopWorkersAndWaitPreserving(nil)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("StopWorkersAndWait blocked without a published worker")
	}
}

func TestWorkersRunningTracksGenerationCompletion(t *testing.T) {
	c := &Cluster{stop: make(chan struct{})}
	generation := &workerGeneration{stop: c.stop, done: make(chan struct{})}
	c.workers = generation
	if !c.WorkersRunning() {
		t.Fatal("published generation was not running")
	}
	c.finishWorkers(generation)
	if c.WorkersRunning() {
		t.Fatal("completed generation remained running")
	}
}
