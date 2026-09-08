package services

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/metrics"
	"github.com/kube-vip/kube-vip/pkg/node/noop"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	"github.com/prometheus/client_golang/prometheus/testutil"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
)

// TestStartServicesLeaderElection_ReturnsOnLeaseLossWithoutServiceDeletion is a regression test for
// the issue: StartServicesLeaderElection deadlocked forever whenever it returned for
// any reason other than the Service itself being deleted.
//
// The function starts a lease-cleanup goroutine that only exits once svcCtx.Ctx is cancelled (Service
// deleted), then defers wg.Wait() on the same WaitGroup that goroutine belonged to. Since the Service
// stays alive across an ordinary lease loss, that goroutine and therefore the deferred wg.Wait()
// never returned, permanently wedging the leader-election restart loop in startLeaderElection for that
// service. The only workaround was to delete and recreate the Service.
func TestStartServicesLeaderElection_ReturnsOnLeaseLossWithoutServiceDeletion(t *testing.T) {
	p := &Processor{
		config:   &kubevip.Config{},
		leaseMgr: lease.NewManager(),
	}

	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "matst-example",
			Namespace: "dsm-system",
			UID:       types.UID("test-uid"),
		},
	}

	leaseNamespace, serviceLease := lease.ServiceName(svc)
	id := lease.NewID(p.config.LeaderElectionType, leaseNamespace, serviceLease)
	svcLease := p.leaseMgr.Add(context.Background(), id)

	// Simulate ordinary leadership/lease loss (e.g. a renewal failure): the lease context ends
	// but the Service itself is untouched, so svcCtx.Ctx must stay alive.
	svcLease.Cancel()

	svcCtx := servicecontext.New(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- p.StartServicesLeaderElection(svcCtx, svc, nil, true)
	}()

	select {
	case <-done:
		// Expected: the function must return promptly when only the lease - not the service -
		// has gone away, so the restart loop can retry the election.
	case <-time.After(5 * time.Second):
		t.Fatal("StartServicesLeaderElection did not return after the lease context was " +
			"cancelled while the service context remained alive; this reproduces the deadlock " +
			"where leader election could never be retried for a live service")
	}

	if svcCtx.Ctx.Err() != nil {
		t.Fatal("service context should not have been cancelled by an ordinary lease loss")
	}

	// The lease-cleanup goroutine should still be running, waiting for the service to be
	// deleted; confirm it is not left dangling forever by cancelling the service context now.
	svcCtx.Cancel()
}

func TestSharedLeaseMemberCancellationDoesNotWaitForLeaseRetirement(t *testing.T) {
	p := &Processor{
		config:   &kubevip.Config{},
		leaseMgr: lease.NewManager(),
	}
	newService := func(name string) *v1.Service {
		return &v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: "default",
				UID:       types.UID(name + "-uid"),
				Annotations: map[string]string{
					kubevip.ServiceLease: "shared",
				},
			},
		}
	}

	leader := newService("leader")
	follower := newService("follower")
	leaderCtx := servicecontext.New(context.Background())
	followerCtx := servicecontext.New(context.Background())
	p.svcMap.Store(leader.UID, leaderCtx)
	p.svcMap.Store(follower.UID, followerCtx)
	p.ServiceInstances = []*instance.Instance{
		{ServiceSnapshot: leader.DeepCopy(), AddCalled: true},
		{ServiceSnapshot: follower.DeepCopy(), AddCalled: true},
	}

	namespace, name := lease.ServiceName(leader)
	id := lease.NewID(p.config.LeaderElectionType, namespace, name)
	svcLease := p.leaseMgr.Add(context.Background(), id)
	svcLease.Add(lease.ServiceNamespacedName(leader))
	svcLease.Elected.Store(true)
	close(svcLease.Started)

	if !followerCtx.StartWatching() {
		t.Fatal("failed to start follower watcher")
	}
	followerCtx.SignalReadiness()
	electionDone := make(chan error, 1)
	go func() {
		defer followerCtx.StopWatching()
		electionDone <- p.StartServicesLeaderElection(followerCtx, follower, nil, true)
	}()

	deadline := time.After(time.Second)
	for !svcLease.Has(lease.ServiceNamespacedName(follower)) {
		select {
		case <-deadline:
			t.Fatal("follower did not join the shared lease")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	joined := make(chan bool, 1)
	go func() {
		svcLease.Lock()
		isMember := svcLease.Has(lease.ServiceNamespacedName(follower))
		svcLease.Unlock()
		joined <- isMember
	}()
	select {
	case isMember := <-joined:
		if !isMember {
			t.Fatal("follower membership disappeared during campaign setup")
		}
	case <-time.After(time.Second):
		t.Fatal("follower did not finish joining the active campaign")
	}

	deleteDone := make(chan error, 1)
	go func() {
		deleteDone <- p.Delete(watch.Event{Type: watch.Deleted, Object: follower}, false)
	}()

	select {
	case err := <-electionDone:
		if err != nil {
			t.Fatalf("StartServicesLeaderElection returned an error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("follower election routine waited for the shared lease to retire")
	}
	select {
	case err := <-deleteDone:
		if err != nil {
			t.Fatalf("deleting follower returned an error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("follower watcher did not return")
	}

	if got := p.leaseMgr.Get(id); got != svcLease {
		t.Fatal("shared lease was retired while its leader remained")
	}
	if svcLease.Ctx.Err() != nil {
		t.Fatal("shared lease context was cancelled while its leader remained")
	}
	if _, ok := p.svcMap.Load(follower.UID); ok {
		t.Fatal("follower context remained registered")
	}
	if len(p.ServiceInstances) != 1 || p.ServiceInstances[0].ServiceSnapshot.UID != leader.UID {
		t.Fatal("follower instance was not removed without disturbing its sibling")
	}

	processed := make(chan bool, 1)
	go func() {
		processed <- p.withActiveService(leader.UID, leaderCtx, func() {})
	}()
	select {
	case active := <-processed:
		if !active {
			t.Fatal("remaining service event was rejected")
		}
	case <-time.After(time.Second):
		t.Fatal("remaining service event processing was blocked")
	}

	if err := p.Delete(watch.Event{Type: watch.Deleted, Object: leader}, false); err != nil {
		t.Fatalf("deleting final member returned an error: %v", err)
	}
	if p.leaseMgr.Get(id) != nil {
		t.Fatal("shared lease remained after deleting its final member")
	}
	if svcLease.Ctx.Err() == nil {
		t.Fatal("shared lease context remained live after deleting its final member")
	}
	if len(p.ServiceInstances) != 0 {
		t.Fatalf("service instances remained after final cleanup: %d", len(p.ServiceInstances))
	}
	if _, ok := p.svcMap.Load(leader.UID); ok {
		t.Fatal("final service context remained registered")
	}
}

func TestSharedLeaseOwnerCancellationStopsCampaignBeforeWatcherWait(t *testing.T) {
	p := &Processor{
		config:   &kubevip.Config{},
		leaseMgr: lease.NewManager(),
	}
	newService := func(name string) *v1.Service {
		return &v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: "default",
				UID:       types.UID(name + "-uid"),
				Annotations: map[string]string{
					kubevip.ServiceLease: "shared",
				},
			},
		}
	}

	owner := newService("owner")
	sibling := newService("sibling")
	ownerCtx := servicecontext.New(context.Background())
	siblingCtx := servicecontext.New(context.Background())
	p.svcMap.Store(owner.UID, ownerCtx)
	p.svcMap.Store(sibling.UID, siblingCtx)
	p.ServiceInstances = []*instance.Instance{
		{ServiceSnapshot: owner.DeepCopy(), AddCalled: true},
		{ServiceSnapshot: sibling.DeepCopy(), AddCalled: true},
	}

	namespace, name := lease.ServiceName(owner)
	id := lease.NewID(p.config.LeaderElectionType, namespace, name)
	svcLease := p.leaseMgr.Add(context.Background(), id)
	ownerName := lease.ServiceNamespacedName(owner)
	siblingName := lease.ServiceNamespacedName(sibling)
	svcLease.Add(ownerName)
	svcLease.Add(siblingName)
	svcLease.Elected.Store(true)
	close(svcLease.Started)

	if !ownerCtx.StartWatching() {
		t.Fatal("failed to start owner watcher")
	}
	campaignStopped := make(chan struct{})
	var stopCampaign sync.Once
	ownerCtx.SetLeaderCancel(func() {
		stopCampaign.Do(func() {
			svcLease.Elected.Store(false)
			svcLease.Started = make(chan any)
			close(campaignStopped)
		})
	})
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		<-ownerCtx.Ctx.Done()
		<-campaignStopped
		ownerCtx.StopWatching()
	}()

	deleteDone := make(chan error, 1)
	go func() {
		deleteDone <- p.Delete(watch.Event{Type: watch.Deleted, Object: owner}, false)
	}()
	select {
	case err := <-deleteDone:
		if err != nil {
			t.Fatalf("deleting campaign owner returned an error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("deleting campaign owner waited for its watcher")
	}
	<-watcherDone
	select {
	case <-campaignStopped:
	default:
		t.Fatal("owner campaign was not cancelled before the watcher wait completed")
	}

	if got := p.leaseMgr.Get(id); got != svcLease {
		t.Fatal("shared lease was replaced while its sibling remained")
	}
	if svcLease.Ctx.Err() != nil {
		t.Fatal("shared lease was cancelled while its sibling remained")
	}
	if svcLease.Has(ownerName) {
		t.Fatal("campaign owner remained a lease member")
	}
	if !svcLease.Has(siblingName) {
		t.Fatal("sibling membership disappeared with the campaign owner")
	}
	if _, ok := p.svcMap.Load(owner.UID); ok {
		t.Fatal("campaign owner context remained registered")
	}
	if len(p.ServiceInstances) != 1 || p.ServiceInstances[0].ServiceSnapshot.UID != sibling.UID {
		t.Fatal("campaign owner cleanup disturbed its sibling instance")
	}

	campaigned := make(chan struct{})
	go func() {
		svcLease.Lock()
		if !svcLease.Elected.Load() && svcLease.Has(siblingName) {
			svcLease.Elected.Store(true)
			close(svcLease.Started)
		}
		svcLease.Unlock()
		close(campaigned)
	}()
	select {
	case <-campaigned:
	case <-time.After(time.Second):
		t.Fatal("sibling could not campaign on the shared lease")
	}
	if !svcLease.Elected.Load() {
		t.Fatal("sibling did not take over the shared lease")
	}

	if err := p.Delete(watch.Event{Type: watch.Deleted, Object: sibling}, false); err != nil {
		t.Fatalf("deleting final member returned an error: %v", err)
	}
	if p.leaseMgr.Get(id) != nil || svcLease.Ctx.Err() == nil {
		t.Fatal("final member did not retire the shared lease")
	}
	if len(p.ServiceInstances) != 0 {
		t.Fatalf("service instances remained after final cleanup: %d", len(p.ServiceInstances))
	}
	if _, ok := p.svcMap.Load(sibling.UID); ok {
		t.Fatal("final service context remained registered")
	}

	replacement := p.leaseMgr.Add(context.Background(), id)
	replacement.Add(siblingName)
	if err := p.Delete(watch.Event{Type: watch.Deleted, Object: sibling}, false); err != nil {
		t.Fatalf("repeating final member deletion returned an error: %v", err)
	}
	p.leaseMgr.Delete(id, siblingName, svcLease)
	if p.leaseMgr.Get(id) != replacement || replacement.Ctx.Err() != nil {
		t.Fatal("stale final cleanup retired the replacement lease")
	}
}
func TestStartServicesLeaderElectionMetricsLifecycleAndRetries(t *testing.T) {
	p := &Processor{
		config:   &kubevip.Config{LeaderElectionType: "test", PerServiceElectionOnDemand: true},
		leaseMgr: lease.NewManager(),
	}
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "metrics", Namespace: "on-demand", UID: types.UID("metrics"),
	}}
	metrics.ServiceElectionAttemptsTotal.DeleteLabelValues(service.Namespace, service.Name)
	defer metrics.ServiceElectionAttemptsTotal.DeleteLabelValues(service.Namespace, service.Name)

	namespace, name := lease.ServiceName(service)
	p.leaseMgr.Add(context.Background(), lease.NewID(p.config.LeaderElectionType, namespace, name))
	svcCtx := servicecontext.New(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.StartServicesLeaderElection(svcCtx, service, nil, true) }()

	deadline := time.Now().Add(time.Second)
	for testutil.ToFloat64(metrics.ServiceElectionLoops.WithLabelValues(service.Namespace, service.Name)) != 1 {
		if time.Now().After(deadline) {
			t.Fatal("on-demand election loop metric did not become active")
		}
		time.Sleep(time.Millisecond)
	}
	if got := testutil.ToFloat64(metrics.ServiceElectionAttemptsTotal.WithLabelValues(service.Namespace, service.Name)); got != 0 {
		t.Fatalf("attempts before election invocation = %v, want 0", got)
	}

	svcCtx.SignalReadiness()
	if err := <-done; err != nil {
		t.Fatalf("first StartServicesLeaderElection() error = %v", err)
	}
	if got := testutil.ToFloat64(metrics.ServiceElectionAttemptsTotal.WithLabelValues(service.Namespace, service.Name)); got != 1 {
		t.Fatalf("attempts after first invocation = %v, want 1", got)
	}
	if err := p.StartServicesLeaderElection(svcCtx, service, nil, true); err != nil {
		t.Fatalf("second StartServicesLeaderElection() error = %v", err)
	}
	if got := testutil.ToFloat64(metrics.ServiceElectionAttemptsTotal.WithLabelValues(service.Namespace, service.Name)); got != 2 {
		t.Fatalf("attempts after retry = %v, want 2", got)
	}
	svcCtx.Cancel()
}

func TestStartServicesLeaderElectionFullModeDoesNotTrackWrapperMetric(t *testing.T) {
	p := &Processor{
		config:   &kubevip.Config{EnableServicesElection: true, LeaderElectionType: "test"},
		leaseMgr: lease.NewManager(),
	}
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "full-mode", Namespace: "metrics", UID: types.UID("full-mode"),
	}}
	namespace, name := lease.ServiceName(service)
	svcLease := p.leaseMgr.Add(context.Background(), lease.NewID(p.config.LeaderElectionType, namespace, name))
	svcLease.Cancel()
	svcCtx := servicecontext.New(context.Background())
	wrapperDone := metrics.TrackServiceElectionLoop(service.Namespace, service.Name)
	defer wrapperDone()

	_ = p.StartServicesLeaderElection(svcCtx, service, nil, true)
	if got := testutil.ToFloat64(metrics.ServiceElectionLoops.WithLabelValues(service.Namespace, service.Name)); got != 1 {
		t.Fatalf("full-mode loop gauge = %v, want wrapper-owned value 1", got)
	}
	svcCtx.Cancel()
}

func TestSharedLeaseFollowerWithdrawsBeforeTakeover(t *testing.T) {
	p := &Processor{
		config:           &kubevip.Config{DisableServiceUpdates: true},
		leaseMgr:         lease.NewManager(),
		nodeLabelManager: noop.NewManager(),
	}
	newService := func(name string) *v1.Service {
		return &v1.Service{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default", UID: types.UID(name + "-uid"),
			Annotations: map[string]string{kubevip.ServiceLease: "shared"},
		}}
	}
	leader := newService("leader")
	follower := newService("follower")
	followerCtx := servicecontext.New(context.Background())
	t.Cleanup(followerCtx.Cancel)
	p.svcMap.Store(follower.UID, followerCtx)
	followerInstance := &instance.Instance{ServiceSnapshot: follower.DeepCopy()}
	p.ServiceInstances = []*instance.Instance{
		{ServiceSnapshot: leader.DeepCopy(), AddCalled: true},
		followerInstance,
	}

	namespace, name := lease.ServiceName(follower)
	id := lease.NewID(p.config.LeaderElectionType, namespace, name)
	svcLease := p.leaseMgr.Add(context.Background(), id)
	svcLease.Add(lease.ServiceNamespacedName(leader))
	svcLease.Elected.Store(true)
	close(svcLease.Started)
	followerCtx.SignalReadiness()

	done := make(chan error, 1)
	go func() { done <- p.StartServicesLeaderElection(followerCtx, follower, nil, true) }()

	deadline := time.After(time.Second)
	for {
		unlock := p.lockService(follower.UID)
		started := followerInstance.AddCalled
		unlock()
		if started {
			break
		}
		select {
		case <-deadline:
			t.Fatal("follower did not enter the active shared lease campaign")
		case <-time.After(time.Millisecond):
		}
	}

	// Local leadership loss must withdraw the follower before the restart loop
	// campaigns again; the old implementation waited for service deletion.
	svcLease.Elected.Store(false)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("follower did not return after leader loss: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("follower remained blocked after shared lease leadership was lost")
	}

	if len(p.ServiceInstances) != 1 || p.ServiceInstances[0].ServiceSnapshot.UID != leader.UID {
		t.Fatal("follower cleanup removed the sibling service")
	}
	if !svcLease.Has(lease.ServiceNamespacedName(leader)) || !svcLease.Has(lease.ServiceNamespacedName(follower)) {
		t.Fatal("follower cleanup changed shared lease membership")
	}
	if svcLease.Ctx.Err() != nil {
		t.Fatal("follower cleanup retired the shared lease while sibling remained")
	}
}
