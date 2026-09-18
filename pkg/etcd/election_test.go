//go:build integration
// +build integration

package etcd_test

import (
	"context"
	"log"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/kube-vip/kube-vip/pkg/etcd"
	. "github.com/onsi/gomega"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.uber.org/zap"
)

func TestRunElectionWithMemberIDCollision(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cli := client(g)
	defer cli.Close()

	electionName := randomElectionNameForTest("memberIDConflict")
	log.Printf("Election name %s\n", electionName)
	memberCtx, cancelMember1 := context.WithCancel(ctx)

	// Use a channel to signal when the first member has observed a new leader
	// This ensures proper ordering without relying on sleep timing
	firstMemberObservedLeader := make(chan struct{})
	var firstMemberObservedOnce sync.Once

	config := &etcd.LeaderElectionConfig{
		EtcdConfig: etcd.ClientConfig{
			Client: cli,
		},
		Name:                 electionName,
		MemberID:             randomElectionNameForTest("my-host"),
		LeaseDurationSeconds: 1,
		Callbacks: etcd.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				log.Println("I'm the leader!!!!")
				log.Println("Renouncing as leader by canceling context")
				cancelMember1()
			},
			OnNewLeader: func(identity string) {
				log.Printf("New leader: %s\n", identity)
				// Signal that the first member has observed a leader
				// This means the lease has been created
				firstMemberObservedOnce.Do(func() {
					close(firstMemberObservedLeader)
				})
			},
			OnStoppedLeading: func() {
				log.Println("I'm not the leader anymore")
			},
		},
	}

	member1Result := make(chan error, 1)
	go func() { member1Result <- etcd.RunElection(memberCtx, config) }()

	select {
	case <-firstMemberObservedLeader:
		// First member has created the lease, so a conflicting election can now be attempted.
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for first member to observe leader")
	}

	member2Ctx, cancelMember2 := context.WithTimeout(ctx, 5*time.Second)
	defer cancelMember2()
	member2Result := make(chan error, 1)
	go func() { member2Result <- etcd.RunElection(member2Ctx, config) }()

	g.Expect(receiveElectionResult(t, member2Result)).To(MatchError(ContainSubstring("creating lease")))
	g.Expect(receiveElectionResult(t, member1Result)).To(Succeed())
}

func TestRunElectionAllowsImmediateSameMemberRestart(t *testing.T) {
	g := NewWithT(t)
	cli := client(g)
	defer cli.Close()

	uniqueID := rand.Uint64()
	config := &etcd.LeaderElectionConfig{
		EtcdConfig:           etcd.ClientConfig{Client: cli},
		Name:                 randomElectionNameForTest("same-member-restart"),
		MemberID:             "same-member",
		MemberUniqueID:       &uniqueID,
		LeaseDurationSeconds: 1,
	}

	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		started := make(chan struct{})
		config.Callbacks = baseCallbacksForName(config.MemberID)
		config.Callbacks.OnStartedLeading = func(context.Context) {
			close(started)
			cancel()
		}

		done := make(chan error, 1)
		go func() { done <- etcd.RunElection(ctx, config) }()
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatalf("attempt %d did not become leader", i+1)
		}
		g.Expect(receiveElectionResult(t, done)).To(Succeed())
	}
}

func TestRunElectionWithTwoMembersAndReelection(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cli := client(g)
	defer cli.Close()

	cliMember1 := client(g)
	defer cliMember1.Close()

	electionName := randomElectionNameForTest("steppingDown")
	configBase := etcd.LeaderElectionConfig{
		EtcdConfig: etcd.ClientConfig{
			Client: cli,
		},
		Name:                 electionName,
		LeaseDurationSeconds: 1,
	}

	member1Ctx := ctx
	member2Ctx, cancelMember2 := context.WithCancel(ctx)

	config1 := configBase
	config1.EtcdConfig.Client = cliMember1
	config1.MemberID = randomElectionNameForTest("my-host")
	uniqueID := rand.Uint64()
	config1.MemberUniqueID = &uniqueID
	config1.Callbacks = baseCallbacksForName(config1.MemberID)
	syncMembers := make(chan struct{})
	leaseCloseResult := make(chan error, 1)
	config1.Callbacks.OnStartedLeading = func(ctx context.Context) {
		log.Println("I'm my-host, the new leader!!!!")
		close(syncMembers)
		log.Println("Losing the leadership on purpose by stopping renewing the lease")
		leaseCloseResult <- cliMember1.Lease.Close()
		log.Println("Member1 leases closed")
		<-ctx.Done()
	}

	config2 := configBase
	config2.MemberID = randomElectionNameForTest("my-other-host")
	config2.Callbacks = baseCallbacksForName(config2.MemberID)
	config2.Callbacks.OnStartedLeading = func(_ context.Context) {
		log.Println("I'm my-other-host, the new leader!!!!")
		log.Println("Renouncing as leader by canceling context")
		cancelMember2()
	}

	member1Result := make(chan error, 1)
	member2Result := make(chan error, 1)
	go func() {
		member1Result <- etcd.RunElection(member1Ctx, &config1)
	}()
	go func() {
		<-syncMembers
		member2Result <- etcd.RunElection(member2Ctx, &config2)
	}()

	g.Expect(receiveElectionResult(t, leaseCloseResult)).To(Succeed())
	g.Expect(receiveElectionResult(t, member1Result)).To(MatchError("election session ended"))
	g.Expect(receiveElectionResult(t, member2Result)).To(Succeed())
}

func baseCallbacksForName(name string) etcd.LeaderCallbacks {
	return etcd.LeaderCallbacks{
		OnStartedLeading: func(ctx context.Context) {
			log.Printf("[%s] I'm the new leader!!!!\n", name)
		},
		OnNewLeader: func(identity string) {
			log.Printf("[%s] New leader: %s\n", name, identity)
		},
		OnStoppedLeading: func() {
			log.Printf("[%s] I'm not the leader anymore\n", name)
		},
	}
}

func randomElectionNameForTest(name string) string {
	return name + "-" + randomString(6)
}

const charSet = "0123456789abcdefghijklmnopqrstuvwxyz"

func randomString(n int) string {
	result := make([]byte, n)
	for i := range result {
		result[i] = charSet[rand.Intn(len(charSet))]
	}
	return string(result)
}

func client(g Gomega) *clientv3.Client {
	c, err := clientv3.New(clientv3.Config{
		Endpoints: []string{"localhost:2379"},
		Logger:    zap.NewNop(),
	})
	g.Expect(err).NotTo(HaveOccurred())
	return c
}

func receiveElectionResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for election to stop")
		return nil
	}
}
