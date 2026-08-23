package etcd

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/concurrency"
)

func TestWatchLeaderChangesHandlesObserveChannelClosure(t *testing.T) {
	client, session := newElectionUnitClient(t, func(context.Context, string, ...clientv3.OpOption) (*clientv3.GetResponse, error) {
		return nil, errors.New("watch stream disconnected")
	})
	election := concurrency.NewElection(session, "kube-vip-test")

	var stopped atomic.Int32
	m := &member{
		client:   client,
		election: election,
		isLeader: true,
		callbacks: LeaderCallbacks{
			OnStoppedLeading: func() { stopped.Add(1) },
		},
	}

	result := make(chan any, 1)
	go func() {
		defer func() { result <- recover() }()
		m.watchLeaderChanges(context.Background())
	}()

	select {
	case recovered := <-result:
		if recovered != nil {
			t.Fatalf("watchLeaderChanges panicked when Observe closed: %v", recovered)
		}
	case <-time.After(time.Second):
		t.Fatal("watchLeaderChanges did not finish after Observe closed")
	}

	// DEFECT: pkg/etcd/election.go:160-169 reads from the Observe channel
	// without checking its closed state. An etcd watch disconnect therefore
	// supplies a nil response and panics instead of reporting leadership loss.
	if got := stopped.Load(); got != 1 {
		t.Fatalf("OnStoppedLeading calls = %d, want 1 after Observe closure", got)
	}
}

func TestTryToBeLeaderStopsPromptlyWhenContextIsCanceled(t *testing.T) {
	client, session := newElectionUnitClient(t, func(context.Context, string, ...clientv3.OpOption) (*clientv3.GetResponse, error) {
		return &clientv3.GetResponse{Header: &pb.ResponseHeader{Revision: 1}}, nil
	})
	election := concurrency.NewElection(session, "kube-vip-test")

	var started atomic.Int32
	m := &member{
		client:         client,
		election:       election,
		memberID:       "node-a",
		leaseTTL:       1,
		weAreTheLeader: make(chan struct{}, 1),
		callbacks: LeaderCallbacks{
			OnStartedLeading: func(context.Context) { started.Add(1) },
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	done := make(chan struct{})
	go func() {
		m.tryToBeLeader(ctx, &wg)
		close(done)
	}()

	select {
	case <-m.weAreTheLeader:
	case <-time.After(time.Second):
		t.Fatal("Campaign did not signal leadership")
	}
	cancel()

	prompt := true
	select {
	case <-done:
	case <-time.After(250 * time.Millisecond):
		prompt = false
	}

	// Keep the test goroutine cleanup deterministic even when the current
	// implementation is still sleeping for its one-second lease TTL.
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("tryToBeLeader did not finish after context cancellation")
	}
	wg.Wait()

	// DEFECT: pkg/etcd/election.go:219-227 unconditionally sleeps for the
	// lease TTL after Campaign. Cancellation during that window delays election
	// restart and still invokes OnStartedLeading with an already-canceled context.
	if !prompt {
		t.Error("tryToBeLeader remained blocked in the lease-TTL sleep after cancellation")
	}
	if got := started.Load(); got != 0 {
		t.Fatalf("OnStartedLeading calls after cancellation = %d, want 0", got)
	}
}

type electionUnitKV struct {
	clientv3.KV
	get func(context.Context, string, ...clientv3.OpOption) (*clientv3.GetResponse, error)
}

func (f *electionUnitKV) Get(ctx context.Context, key string, opts ...clientv3.OpOption) (*clientv3.GetResponse, error) {
	return f.get(ctx, key, opts...)
}

func (f *electionUnitKV) Txn(context.Context) clientv3.Txn {
	return &electionUnitTxn{}
}

type electionUnitTxn struct{ clientv3.Txn }

func (t *electionUnitTxn) If(...clientv3.Cmp) clientv3.Txn  { return t }
func (t *electionUnitTxn) Then(...clientv3.Op) clientv3.Txn { return t }
func (t *electionUnitTxn) Else(...clientv3.Op) clientv3.Txn { return t }
func (t *electionUnitTxn) Commit() (*clientv3.TxnResponse, error) {
	return &clientv3.TxnResponse{
		Header:    &pb.ResponseHeader{Revision: 1},
		Succeeded: true,
	}, nil
}

type electionUnitLease struct{ clientv3.Lease }

func (f *electionUnitLease) KeepAlive(ctx context.Context, _ clientv3.LeaseID) (<-chan *clientv3.LeaseKeepAliveResponse, error) {
	ch := make(chan *clientv3.LeaseKeepAliveResponse)
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
}

func (f *electionUnitLease) Revoke(context.Context, clientv3.LeaseID) (*clientv3.LeaseRevokeResponse, error) {
	return &clientv3.LeaseRevokeResponse{}, nil
}

func (f *electionUnitLease) Close() error { return nil }

type electionUnitWatcher struct{ clientv3.Watcher }

func (f *electionUnitWatcher) Watch(context.Context, string, ...clientv3.OpOption) clientv3.WatchChan {
	ch := make(chan clientv3.WatchResponse)
	close(ch)
	return ch
}

func (f *electionUnitWatcher) RequestProgress(context.Context) error { return nil }
func (f *electionUnitWatcher) Close() error                          { return nil }

func newElectionUnitClient(t *testing.T, get func(context.Context, string, ...clientv3.OpOption) (*clientv3.GetResponse, error)) (*clientv3.Client, *concurrency.Session) {
	t.Helper()

	client := clientv3.NewCtxClient(context.Background())
	client.KV = &electionUnitKV{get: get}
	client.Lease = &electionUnitLease{}
	client.Watcher = &electionUnitWatcher{}

	session, err := concurrency.NewSession(client,
		concurrency.WithLease(clientv3.LeaseID(42)),
		concurrency.WithTTL(1),
	)
	if err != nil {
		client.Close()
		t.Fatalf("creating election test session: %v", err)
	}
	t.Cleanup(func() {
		_ = session.Close()
		_ = client.Close()
	})
	return client, session
}
