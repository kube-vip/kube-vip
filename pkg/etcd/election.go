package etcd

import (
	"context"
	"hash/fnv"
	"time"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
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
func RunElectionOrDie(ctx context.Context, config *LeaderElectionConfig) {
	if err := RunElection(ctx, config); err != nil {
		panic(err)
	}
}

// RunElection starts a client with the provided config or panics.
// RunElection blocks until leader election loop is
// stopped by ctx or it has stopped holding the leader lease.
func RunElection(ctx context.Context, config *LeaderElectionConfig) error {
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
	)
	if err != nil {
		return err
	}

	election := concurrency.NewElection(s, config.Name)

	m := &member{
		client:         config.EtcdConfig.Client,
		election:       election,
		callbacks:      config.Callbacks,
		memberID:       config.MemberID,
		weAreTheLeader: make(chan struct{}, 1),
		leaseTTL:       lease.TTL,
	}

	go m.tryToBeLeader(ctx)
	m.watchLeaderChanges(ctx)

	return nil
}

type member struct {
	key              string
	client           *clientv3.Client
	election         *concurrency.Election
	isLeader         bool
	currentLeaderKey string
	callbacks        LeaderCallbacks
	memberID         string
	weAreTheLeader   chan struct{}
	leaseTTL         int64
}

func (m *member) watchLeaderChanges(ctx context.Context) {
	observeCtx, observeCancel := context.WithCancel(ctx)
	defer observeCancel()
	changes := m.election.Observe(observeCtx)

watcher:
	for {
		select {
		case <-ctx.Done():
			break watcher
		case <-m.weAreTheLeader:

			m.isLeader = true
			m.key = m.election.Key() // by this time, this should already be set, since Campaign has already returned
			log.Debugf("[%s] Marking self as leader with key %s\n", m.memberID, m.key)
		case response := <-changes:
			log.Debugf("[%s] Leader Changes: %+v\n", m.memberID, response)
			if len(response.Kvs) == 0 {
				// There is a race condition where just after we stop being the leader
				// if there are no more leaders, we might get a response with no key-values
				// just before the response channel is closed or the context is cancel
				// In that case, just continue and let one of those two things happen
				continue
			}
			newLeaderKey := response.Kvs[0].Key
			if m.isLeader && m.key != string(newLeaderKey) {
				// We stopped being leaders

				// exit the loop, so we cancel the observe context so we stop watching
				// for new leaders. That will close the channel and make this function exit,
				// which also makes the routine to finish and RunElection returns
				break watcher
			}

			if m.currentLeaderKey != string(newLeaderKey) {
				// we observed a leader, this could be us or someone else
				m.currentLeaderKey = string(newLeaderKey)
				m.callbacks.OnNewLeader(string(response.Kvs[0].Value))
			}
		}
	}

	// If we are here, either we have stopped being leaders or we lost the watcher
	// Make sure we call OnStoppedLeading if we were the leader.
	if m.isLeader {
		m.callbacks.OnStoppedLeading()
	}

	log.Debugf("[%s] Exiting watcher\n", m.memberID)
}

func (m *member) tryToBeLeader(ctx context.Context) {
	if err := m.election.Campaign(ctx, m.memberID); err != nil {
		log.Errorf("Failed trying to become the leader: %s", err)
		// Resign just in case we acquired leadership just before failing
		if err := m.election.Resign(m.client.Ctx()); err != nil {
			log.Warnf("Failed to resign after we failed becoming the leader, this might not be a problem if we were never the leader: %s", err)
		}
		return
		// TODO: what to do here?
		// We probably want watchLeaderChanges to exit as well, since Run
		// is expecting us to try to become the leader, but if we are here,
		// we won't. So if we don't panic, we need to signal it somehow
	}

	// Inform the observer that we are the leader as soon as possible,
	// so it can detect if we stop being it
	m.weAreTheLeader <- struct{}{}

	// Once we are the leader, start the routine to resign if context is canceled
	go m.resignOnCancel(ctx)

	// After becoming the leader, we wait for at least a lease TTL to wait for
	// the previous leader to detect the new leadership (if there was one) and
	// stop its processes
	// TODO: is this too cautious?
	log.Debugf("[%s] Waiting %d seconds before running OnStartedLeading", m.memberID, m.leaseTTL)
	time.Sleep(time.Second * time.Duration(m.leaseTTL))

	// We are the leader, execute our code
	m.callbacks.OnStartedLeading(ctx)

	// Here the routine dies if OnStartedLeading doesn't block, there is nothing else to do
}

func (m *member) resignOnCancel(ctx context.Context) {
	<-ctx.Done()
	if err := m.election.Resign(m.client.Ctx()); err != nil {
		log.Errorf("Failed to resign after the context was canceled: %s", err)
	}
}
