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
	ctx := context.Background()
	cli := client(g)
	defer cli.Close()

	electionName := randomElectionNameForTest("memberIDConflict")
	log.Printf("Election name %s\n", electionName)
	memberCtx, cancelMember1 := context.WithCancel(ctx)
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
			},
			OnStoppedLeading: func() {
				log.Println("I'm not the leader anymore")
			},
		},
	}

	wg := &sync.WaitGroup{}
	wg.Add(2)

	go func() {
		defer wg.Done()
		g.Expect(etcd.RunElection(memberCtx, config)).To(Succeed())
	}()

	go func() {
		defer wg.Done()
		time.Sleep(time.Millisecond * 100) // make sure the first one becomes leader
		g.Expect(etcd.RunElection(ctx, config)).Should(MatchError(ContainSubstring("creating lease")))
	}()

	wg.Wait()
}

func TestRunElectionWithTwoMembersAndReelection(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)
	ctx := context.Background()
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

	member1Ctx, _ := context.WithCancel(ctx)
	member2Ctx, cancelMember2 := context.WithCancel(ctx)

	config1 := configBase
	config1.EtcdConfig.Client = cliMember1
	config1.MemberID = randomElectionNameForTest("my-host")
	uniqueID := rand.Uint64()
	config1.MemberUniqueID = &uniqueID
	config1.Callbacks = baseCallbacksForName(config1.MemberID)
	syncMembers := make(chan (any))
	config1.Callbacks.OnStartedLeading = func(_ context.Context) {
		log.Println("I'm my-host, the new leader!!!!")
		close(syncMembers)
		log.Println("Losing the leadership on purpose by stopping renewing the lease")
		g.Expect(cliMember1.Lease.Close()).To(Succeed())
		log.Println("Member1 leases closed")
	}

	config2 := configBase
	config2.MemberID = randomElectionNameForTest("my-other-host")
	config2.Callbacks = baseCallbacksForName(config2.MemberID)
	config2.Callbacks.OnStartedLeading = func(_ context.Context) {
		log.Println("I'm my-other-host, the new leader!!!!")
		log.Println("Renouncing as leader by canceling context")
		cancelMember2()
	}

	wg := &sync.WaitGroup{}
	wg.Add(2)

	go func() {
		defer wg.Done()
		g.Expect(etcd.RunElection(member1Ctx, &config1)).To(Succeed())
		log.Printf("%s routine done\n", config1.MemberID)
	}()

	go func() {
		defer wg.Done()
		<-syncMembers
		g.Expect(etcd.RunElection(member2Ctx, &config2)).To(Succeed())
		log.Printf("%s routine done\n", config2.MemberID)
	}()

	wg.Wait()
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

var rnd = rand.New(rand.NewSource(time.Now().UnixNano()))

func randomString(n int) string {
	result := make([]byte, n)
	for i := range result {
		result[i] = charSet[rnd.Intn(len(charSet))]
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
