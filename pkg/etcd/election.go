package etcd

import (
	"context"
	"fmt"
	"hash/fnv"
	"sync"
	"sync/atomic"
	"time"

	log "log/slog"

	"github.com/pkg/errors"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/concurrency"
)

// LeaderElectionConfig allows to configure the leader election params.
type LeaderElectionConfig struct {
	// EtcdConfig contains the client to connect to the etcd cluster.
	EtcdConfig ClientConfig

	// Name uniquely identifies this leader election. All members of the same election
	// should use the same value here.
	Name string

	// MemberID identifies uniquely this contestant from other in the leader election.
	// It will be converted to an int64 using a hash, so theoretically collisions are possible
	// when using a string. If you want to guarantee safety, us MemberUniqueID to specify a unique
	// int64 directly.
	// If two processes start a leader election using the same MemberID, one of them will
	// fail.
	MemberID string

	// MemberUniqueID is the int equivalent to MemberID that allows to override the default conversion
	// from string to int using hashing.
	MemberUniqueID *uint64

	// LeaseDurationSeconds is the duration that non-leader candidates will
	// wait to force acquire leadership.
	// This is just a request to the etcd server but it's not guaranteed, the server
	// might decide to make the duration longer.
	LeaseDurationSeconds int64

	// Callbacks are callbacks that are triggered during certain lifecycle
	// events of the LeaderElector
	Callbacks LeaderCallbacks
}

// LeaderCallbacks are callbacks that are triggered during certain
// lifecycle events of the election.
type LeaderCallbacks struct {
	// OnStartedLeading is called when this member starts leading.
	OnStartedLeading func(context.Context)
	// OnStoppedLeading is called when this member stops leading.
	OnStoppedLeading func()
	// OnNewLeader is called when the client observes a leader that is
	// not the previously observed leader. This includes the first observed
	// leader when the client starts.
	OnNewLeader func(identity string)
}

// ClientConfig contains the client to connect to the etcd cluster.
type ClientConfig struct {
	Client *clientv3.Client
}

// RunElectionOrDie behaves the same way as RunElection but panics if there is an error.
func RunElectionOrDie(ctx context.Context, config *LeaderElectionConfig) error {
	if err := RunElection(ctx, config); err != nil {
		return fmt.Errorf("leaderelection error: %w", err)
	}
	return nil
}

// RunElection starts a client with the provided config or panics.
// RunElection blocks until leader election loop is
// stopped by ctx or it has stopped holding the leader lease.
func RunElection(ctx context.Context, config *LeaderElectionConfig) (runErr error) {
	var memberID uint64
	if config.MemberUniqueID != nil {
		memberID = *config.MemberUniqueID
	} else {
		h := fnv.New64a()
		if _, err := h.Write(append([]byte(config.Name), []byte(config.MemberID)...)); err != nil {
			return err
		}
		memberID = h.Sum64()
	}

	ttl := config.LeaseDurationSeconds
	r := &pb.LeaseGrantRequest{TTL: ttl, ID: int64(memberID)} //nolint
	lease, err := clientv3.RetryLeaseClient(
		config.EtcdConfig.Client,
	).LeaseGrant(ctx, r)
	if err != nil {
		return errors.Wrap(err, "creating lease")
	}

	leaseID := clientv3.LeaseID(lease.ID)

	s, err := concurrency.NewSession(
		config.EtcdConfig.Client,
		concurrency.WithTTL(int(lease.TTL)),
		concurrency.WithLease(leaseID),
		concurrency.WithContext(ctx),
	)
	if err != nil {
		_ = revokeLease(config.EtcdConfig.Client, leaseID, lease.TTL)
		return err
	}
	defer func() {
		if err := s.Close(); err != nil {
			if revokeErr := revokeLease(config.EtcdConfig.Client, leaseID, lease.TTL); revokeErr != nil && runErr == nil && ctx.Err() == nil {
				runErr = errors.Wrap(revokeErr, "revoking election lease")
			}
		}
	}()

	election := concurrency.NewElection(s, config.Name)

	m := &member{
		election:    election,
		callbacks:   config.Callbacks,
		memberID:    config.MemberID,
		leaderDelay: time.Second * time.Duration(lease.TTL),
	}
	return m.run(ctx, s.Done())
}

type election interface {
	Campaign(context.Context, string) error
	Key() string
	Observe(context.Context) <-chan *clientv3.GetResponse
}

type member struct {
	election    election
	callbacks   LeaderCallbacks
	memberID    string
	leaderDelay time.Duration
	state       atomic.Int32
}

type campaignResult struct {
	key string
	ack chan struct{}
}

func (m *member) run(ctx context.Context, sessionDone <-chan struct{}) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	elected := make(chan campaignResult, 1)
	campaignDone := make(chan error, 1)
	observerDone := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Go(func() { campaignDone <- m.campaign(runCtx, elected, sessionDone) })
	wg.Go(func() { observerDone <- m.watchLeaderChanges(runCtx, elected) })

	var result error
	for campaignDone != nil && observerDone != nil {
		select {
		case <-ctx.Done():
			m.stop()
			cancel()
			wg.Wait()
			return nil
		case <-sessionDone:
			m.stop()
			if ctx.Err() != nil {
				cancel()
				wg.Wait()
				return nil
			}
			result = errors.New("election session ended")
			cancel()
			wg.Wait()
			return result
		case err := <-campaignDone:
			campaignDone = nil
			if err != nil {
				if ctx.Err() != nil {
					m.stop()
					cancel()
					wg.Wait()
					return nil
				}
				result = errors.Wrap(err, "campaigning for leadership")
				m.stop()
				cancel()
				wg.Wait()
				return result
			}
		case err := <-observerDone:
			observerDone = nil
			if err != nil {
				if ctx.Err() != nil {
					m.stop()
					cancel()
					wg.Wait()
					return nil
				}
				result = errors.Wrap(err, "observing leader changes")
				m.stop()
				cancel()
				wg.Wait()
				return result
			}
		}
	}

	m.stop()
	cancel()
	wg.Wait()
	return result
}

func (m *member) watchLeaderChanges(ctx context.Context, elected <-chan campaignResult) error {
	changes := m.election.Observe(ctx)
	var key, currentLeaderKey string
	var isLeader bool
	defer func() {
		if isLeader {
			m.callbacks.OnStoppedLeading()
		}
		log.Debug("Exiting watcher", "id", m.memberID)
	}()
	defer m.stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case result := <-elected:
			isLeader = true
			key = result.key
			close(result.ack)
			log.Debug("Marking self as leader with key", "id", m.memberID, "key", key)
		case response, ok := <-changes:
			if !ok {
				if ctx.Err() != nil {
					return nil
				}
				return errors.New("leader observation ended")
			}
			log.Debug("Leader Changes", "id", m.memberID, "response", response)
			if len(response.Kvs) == 0 {
				// There is a race condition where just after we stop being the leader
				// if there are no more leaders, we might get a response with no key-values
				// just before the response channel is closed or the context is cancel
				// In that case, just continue and let one of those two things happen
				continue
			}
			newLeaderKey := response.Kvs[0].Key
			if isLeader && key != string(newLeaderKey) {
				// We stopped being leaders

				// exit the loop, so we cancel the observe context so we stop watching
				// for new leaders. That will close the channel and make this function exit,
				// which also makes the routine to finish and RunElection returns
				return errors.New("leadership lost")
			}

			if currentLeaderKey != string(newLeaderKey) {
				// we observed a leader, this could be us or someone else
				currentLeaderKey = string(newLeaderKey)
				m.callbacks.OnNewLeader(string(response.Kvs[0].Value))
			}
		}
	}
}

func (m *member) campaign(ctx context.Context, elected chan<- campaignResult, sessionDone <-chan struct{}) error {
	if err := m.election.Campaign(ctx, m.memberID); err != nil {
		return err
	}

	result := campaignResult{key: m.election.Key(), ack: make(chan struct{})}
	select {
	case elected <- result:
	case <-ctx.Done():
		return nil
	case <-sessionDone:
		return errors.New("election session ended")
	}
	select {
	case <-result.ack:
	case <-ctx.Done():
		return nil
	case <-sessionDone:
		return errors.New("election session ended")
	}

	// After becoming the leader, we wait for at least a lease TTL to wait for
	// the previous leader to detect the new leadership (if there was one) and
	// stop its processes
	log.Debug("timeout before OnStartedLeading", "id", m.memberID, "timeout", m.leaderDelay)
	timer := time.NewTimer(m.leaderDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil
	case <-sessionDone:
		return errors.New("election session ended")
	case <-timer.C:
	}

	if ctx.Err() != nil || !m.state.CompareAndSwap(0, 1) {
		return nil
	}
	select {
	case <-sessionDone:
		return errors.New("election session ended")
	default:
	}
	m.callbacks.OnStartedLeading(ctx)
	return nil
}

func (m *member) stop() {
	m.state.CompareAndSwap(0, 2)
}

func revokeLease(client *clientv3.Client, leaseID clientv3.LeaseID, ttl int64) error {
	timeout := time.Second * time.Duration(ttl)
	if timeout <= 0 {
		timeout = time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, err := client.Revoke(ctx, leaseID)
	return err
}
