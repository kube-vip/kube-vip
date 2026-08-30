package services

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/election"
	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/metrics"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	v1 "k8s.io/api/core/v1"
)

var errServiceLeaseStopped = errors.New("service lease stopped")
var errServiceCleanupShutdown = errors.New("service cleanup failed during watcher shutdown")

type electionMemberKey struct {
	claim  string
	svcCtx *servicecontext.Context
	lost   <-chan any
}

type serviceElectionMember struct {
	key              electionMemberKey
	service          *v1.Service
	lease            *lease.Lease
	active           bool
	activeEpoch      uint64
	activationCancel context.CancelFunc
	instance         *instance.Instance
	cleaning         bool
	removing         bool
	finished         bool
	running          bool
	err              error
	retrying         bool
	activationWG     sync.WaitGroup
	retired          chan struct{}
	cleanupAttempted chan struct{}
	cleanupOnce      sync.Once
	finalizers       []func()
	stopSet          map[*instance.Instance]struct{}
	forceDelete      map[string]struct{}
	leadershipLost   bool
	stopPrepared     <-chan struct{}
}

type serviceCampaign struct {
	epoch   uint64
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}
	lease   *lease.Lease
	service *v1.Service
	owned   bool
	started bool
}

type serviceElection struct {
	p  *Processor
	id lease.ID

	mu        sync.Mutex
	members   map[electionMemberKey]*serviceElectionMember
	campaign  *serviceCampaign
	elected   bool
	stopping  bool
	epoch     uint64
	changed   chan struct{}
	retrying  bool
	retryStop chan struct{}
	backoff   time.Duration
}

func (p *Processor) startLeaderElection(parent context.Context, svcCtx *servicecontext.Context, service *v1.Service, _ *sync.WaitGroup) error {
	loops := metrics.ServiceElectionLoops.WithLabelValues(service.Namespace, service.Name)
	loops.Inc()
	defer loops.Dec()
	for svcCtx.Ctx.Err() == nil {
		desired, _ := p.desiredService(service.UID)
		if desired == nil {
			desired = service
		}
		err := p.runServiceElectionMember(svcCtx, desired)
		if svcCtx.Ctx.Err() != nil {
			break
		}
		if err == nil {
			continue
		}
		log.Error(err.Error())

		if c, member := p.serviceElectionMember(service, svcCtx); member != nil {
			select {
			case <-member.retired:
				c.mu.Lock()
				finalErr := member.err
				c.mu.Unlock()
				if finalErr != nil {
					return finalErr
				}
			case <-svcCtx.Ctx.Done():
				continue
			}
		}

		ns, name := lease.ServiceName(service)
		id := lease.NewID(p.config.LeaderElectionType, ns, name)
		registered := p.leaseMgr.Get(id)
		if registered == nil || registered.Ctx.Err() != nil || errors.Is(err, errServiceLeaseStopped) {
			p.leaseMgr.Add(parent, id)
		}
		timer := time.NewTimer(200 * time.Millisecond)
		select {
		case <-svcCtx.Ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
	if c, member := p.serviceElectionMember(service, svcCtx); member != nil {
		select {
		case <-member.retired:
			c.mu.Lock()
			err := member.err
			c.mu.Unlock()
			return err
		case <-parent.Done():
			c.mu.Lock()
			err := member.err
			c.mu.Unlock()
			return err
		}
	}
	return svcCtx.LeaderError()
}

func (p *Processor) runServiceElectionMember(svcCtx *servicecontext.Context, service *v1.Service) error {
	if c, member := p.serviceElectionMember(service, svcCtx); member != nil {
		return c.runMemberOnce(member)
	}
	if err := svcCtx.Ctx.Err(); err != nil {
		return fmt.Errorf("service context cancelled before election start: %w", err)
	}
	ready, lost := svcCtx.ReadinessGeneration()
	ns, name := lease.ServiceName(service)
	id := lease.NewID(p.config.LeaderElectionType, ns, name)
	registered := p.leaseMgr.Get(id)
	if registered == nil {
		return fmt.Errorf("no existing lease found for service %q with UID %q", service.Name, service.UID)
	}
	select {
	case <-svcCtx.Ctx.Done():
		return nil
	case <-lost:
		return nil
	case <-ready:
	case <-registered.Ctx.Done():
		return fmt.Errorf("lease context cancelled before election start: %w", registered.Ctx.Err())
	}
	desired, version := p.desiredService(service.UID)
	if desired != nil {
		service = desired
	}
	expected := &serviceExpectation{svcCtx: svcCtx, lost: lost, version: version, lifecycle: serviceLifecycleFor(service)}
	unlock := p.lockService(service.UID)
	if !p.serviceExpectationCurrentLocked(service, expected) {
		unlock()
		return nil
	}
	claim := fmt.Sprintf("%s#%d", lease.ServiceClaimID(service), p.claimSeq.Add(1))
	key := electionMemberKey{claim: claim, svcCtx: svcCtx, lost: lost}
	c, member, err := p.claimAndRegisterServiceElectionMember(service, id, key)
	unlock()
	if err != nil {
		return err
	}
	err = c.runMemberOnce(member)
	c.waitForCampaignIfEmpty(member)
	if member.lease.Ctx.Err() != nil && err != nil {
		return fmt.Errorf("%w: %v", errServiceLeaseStopped, member.lease.Ctx.Err())
	}
	return err
}

func (c *serviceElection) runMemberOnce(member *serviceElectionMember) error {
	c.mu.Lock()
	if member.running {
		retired := member.retired
		c.mu.Unlock()
		select {
		case <-retired:
			c.mu.Lock()
			err := member.err
			c.mu.Unlock()
			return err
		case <-member.key.svcCtx.Ctx.Done():
			return nil
		case <-member.key.lost:
			return nil
		}
	}
	member.running = true
	c.mu.Unlock()
	return c.runMember(member)
}

func (p *Processor) serviceElectionMember(service *v1.Service, svcCtx *servicecontext.Context) (*serviceElection, *serviceElectionMember) {
	ns, name := lease.ServiceName(service)
	id := lease.NewID(p.config.LeaderElectionType, ns, name)
	p.electionsMu.Lock()
	c := p.elections[id.NamespacedName()]
	p.electionsMu.Unlock()
	if c == nil {
		return nil, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, member := range c.members {
		if key.svcCtx == svcCtx {
			return c, member
		}
	}
	return c, nil
}

// serviceElectionMemberForContext is called while the Service UID lock is held
// when a Modified event may have changed the lease name. It obtains the captured
// snapshot without requiring the caller to derive the old coordinator key.
func (p *Processor) serviceElectionMemberForContext(svcCtx *servicecontext.Context) (*serviceElection, *serviceElectionMember) {
	p.electionsMu.Lock()
	defer p.electionsMu.Unlock()
	for _, c := range p.elections {
		c.mu.Lock()
		for key, member := range c.members {
			if key.svcCtx == svcCtx {
				c.mu.Unlock()
				return c, member
			}
		}
		c.mu.Unlock()
	}
	return nil, nil
}

// Lock order is Service UID, electionsMu, then coordinator. Coordinator paths
// never acquire a Service UID while holding the coordinator lock.
func (p *Processor) claimAndRegisterServiceElectionMember(service *v1.Service, id lease.ID, key electionMemberKey) (*serviceElection, *serviceElectionMember, error) {
	p.electionsMu.Lock()
	if p.elections == nil {
		p.elections = make(map[string]*serviceElection)
	}
	c := p.elections[id.NamespacedName()]
	if c != nil {
		c.mu.Lock()
		for existingKey, member := range c.members {
			if existingKey.svcCtx == key.svcCtx && existingKey.lost == key.lost {
				c.mu.Unlock()
				p.electionsMu.Unlock()
				return c, member, nil
			}
		}
		c.mu.Unlock()
	}
	svcLease, _ := p.leaseMgr.Claim(id, key.claim)
	if svcLease == nil {
		p.electionsMu.Unlock()
		metrics.ServiceElectionErrorsTotal.WithLabelValues(service.Namespace, service.Name, "no_lease").Inc()
		return nil, nil, fmt.Errorf("no existing lease found for service %q with UID %q", service.Name, service.UID)
	}
	if c == nil {
		c = &serviceElection{p: p, id: id, members: make(map[electionMemberKey]*serviceElectionMember), changed: make(chan struct{})}
		p.elections[id.NamespacedName()] = c
	}
	c.mu.Lock()
	for existingKey, member := range c.members {
		if existingKey.svcCtx == key.svcCtx && existingKey.lost == key.lost {
			p.leaseMgr.Delete(id, key.claim, svcLease)
			c.mu.Unlock()
			p.electionsMu.Unlock()
			return c, member, nil
		}
	}
	if member := c.members[key]; member != nil {
		c.mu.Unlock()
		p.electionsMu.Unlock()
		return c, member, nil
	}
	member := &serviceElectionMember{key: key, service: service.DeepCopy(), lease: svcLease, retired: make(chan struct{}), cleanupAttempted: make(chan struct{})}
	c.members[key] = member
	if c.campaign == nil && !c.stopping && !c.retrying {
		c.startCampaignLocked()
	}
	c.signalLocked()
	c.mu.Unlock()
	p.electionsMu.Unlock()
	return c, member, nil
}

func (c *serviceElection) runMember(member *serviceElectionMember) error {
	for {
		c.mu.Lock()
		if member.finished {
			err := member.err
			c.mu.Unlock()
			return err
		}
		changed, active, stop := c.changed, member.active, c.stopping
		activate := c.elected && !stop && !active && !member.cleaning && !member.removing
		var activationCtx context.Context
		var campaignDone <-chan struct{}
		var version uint64
		if activate {
			var desired *v1.Service
			desired, version = c.p.desiredService(member.service.UID)
			if desired != nil {
				member.service = desired
			}
			member.active, member.activeEpoch = true, c.epoch
			member.stopSet, member.forceDelete, member.leadershipLost, member.stopPrepared = nil, nil, false, nil
			campaignDone = c.campaign.ctx.Done()
			activationCtx, member.activationCancel = context.WithCancel(context.WithoutCancel(member.key.svcCtx.Ctx))
		}
		epoch := member.activeEpoch
		c.mu.Unlock()
		if activate {
			activationCancel := member.activationCancel
			go func() {
				select {
				case <-member.key.svcCtx.Ctx.Done():
					c.markMemberRemoving(member)
				case <-member.key.lost:
					c.markMemberRemoving(member)
				case <-campaignDone:
				case <-activationCtx.Done():
				}
				c.prepareAndCancelMember(member, activationCancel)
			}()
			expected := &serviceExpectation{
				svcCtx:    member.key.svcCtx,
				lost:      member.key.lost,
				version:   version,
				lifecycle: serviceLifecycleFor(member.service),
				valid:     func() bool { return c.memberCanActivate(member, epoch) },
				track: func(inst *instance.Instance) {
					c.mu.Lock()
					if member.active && member.activeEpoch == epoch {
						member.instance = inst
					}
					c.mu.Unlock()
				},
			}
			if err := c.p.onStartedLeading(activationCtx, member.key.svcCtx, member.service, &member.activationWG, expected); err != nil {
				metrics.ServiceElectionErrorsTotal.WithLabelValues(member.service.Namespace, member.service.Name, "service_sync").Inc()
				if c.memberCanRetryNextCampaign(member, epoch) {
					if cleanupErr := c.deactivateMember(member); cleanupErr != nil {
						return fmt.Errorf("activate service: %w; cleanup: %w", err, cleanupErr)
					}
					continue
				}
				if cleanupErr := c.cleanupMember(member); cleanupErr != nil {
					return fmt.Errorf("activate service: %w; cleanup: %w", err, cleanupErr)
				}
				return fmt.Errorf("activate service: %w", err)
			}
			continue
		}
		if stop && active && !member.cleaning {
			if err := c.deactivateMember(member); err != nil {
				return err
			}
			continue
		}
		select {
		case <-member.key.svcCtx.Ctx.Done():
			c.markMemberRemoving(member)
			return c.cleanupMember(member)
		case <-member.key.lost:
			c.markMemberRemoving(member)
			return c.cleanupMember(member)
		case <-member.lease.Ctx.Done():
			if err := c.cleanupMember(member); err != nil {
				return err
			}
			return fmt.Errorf("%w: %v", errServiceLeaseStopped, member.lease.Ctx.Err())
		case <-changed:
		}
	}
}

func (c *serviceElection) markMemberRemoving(member *serviceElectionMember) {
	c.mu.Lock()
	if c.members[member.key] == member {
		member.removing = true
		if member.stopSet == nil {
			member.leadershipLost = false
			member.forceDelete = nil
		}
		c.signalLocked()
	}
	c.mu.Unlock()
}

func (c *serviceElection) memberServiceSnapshot(member *serviceElectionMember) *v1.Service {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.members[member.key] != member || member.service == nil {
		return nil
	}
	return member.service.DeepCopy()
}

func (c *serviceElection) memberCanRetryNextCampaign(member *serviceElectionMember, epoch uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.members[member.key] != member || member.removing || member.key.svcCtx.Ctx.Err() != nil || member.lease.Ctx.Err() != nil {
		return false
	}
	select {
	case <-member.key.lost:
		return false
	default:
	}
	return c.campaign != nil && c.campaign.epoch == epoch && c.stopping
}

func (c *serviceElection) cleanupMember(member *serviceElectionMember) error {
	c.mu.Lock()
	if c.members[member.key] != member {
		c.mu.Unlock()
		return nil
	}
	if member.finished {
		err := member.err
		c.mu.Unlock()
		return err
	}
	if member.cleaning {
		err := member.err
		c.mu.Unlock()
		return err
	}
	member.cleaning = true
	activationCancel := member.activationCancel
	active := member.active
	c.mu.Unlock()
	if activationCancel != nil {
		c.prepareAndCancelMember(member, activationCancel)
	}
	var cleanupErr error
	if active {
		cleanupErr = c.stopMemberWithRetry(member)
	}
	if cleanupErr == nil {
		c.retireMember(member)
		return nil
	}
	c.mu.Lock()
	member.err = cleanupErr
	queued := true
	if !member.retrying {
		member.retrying = true
		if !c.p.queueMemberCleanup(c, member) {
			member.retrying = false
			queued = false
		}
	}
	c.signalLocked()
	c.mu.Unlock()
	member.cleanupOnce.Do(func() { close(member.cleanupAttempted) })
	if !queued {
		return c.retryMemberCleanup(member.key.svcCtx.Parent(), member)
	}
	return cleanupErr
}

func (c *serviceElection) deactivateMember(member *serviceElectionMember) error {
	c.mu.Lock()
	if member.cleaning || !member.active {
		c.mu.Unlock()
		return nil
	}
	member.cleaning = true
	activationCancel := member.activationCancel
	c.mu.Unlock()
	if activationCancel != nil {
		c.prepareAndCancelMember(member, activationCancel)
	}
	cleanupErr := c.stopMemberWithRetry(member)
	if cleanupErr == nil {
		c.mu.Lock()
		member.active, member.cleaning, member.instance, member.activationCancel = false, false, nil, nil
		member.stopSet, member.forceDelete, member.leadershipLost, member.stopPrepared = nil, nil, false, nil
		c.signalLocked()
		c.mu.Unlock()
		return nil
	}
	c.mu.Lock()
	member.err = cleanupErr
	queued := true
	if !member.retrying {
		member.retrying = true
		if !c.p.queueMemberCleanup(c, member) {
			member.retrying = false
			queued = false
		}
	}
	c.signalLocked()
	c.mu.Unlock()
	if !queued {
		return c.retryMemberCleanup(member.key.svcCtx.Parent(), member)
	}
	return cleanupErr
}

func (c *serviceElection) stopMember(member *serviceElectionMember) error {
	c.mu.Lock()
	inst := member.instance
	stopSet := member.stopSet
	forceDelete := member.forceDelete
	leadershipLost := member.leadershipLost
	stopPrepared := member.stopPrepared
	if stopSet == nil {
		if member.removing || member.key.svcCtx.Ctx.Err() != nil {
			leadershipLost = false
			forceDelete = nil
		} else {
			select {
			case <-member.key.lost:
				leadershipLost = false
				forceDelete = nil
			default:
			}
		}
	}
	c.mu.Unlock()
	if stopPrepared != nil {
		<-stopPrepared
		c.mu.Lock()
		forceDelete = member.forceDelete
		leadershipLost = member.leadershipLost
		c.mu.Unlock()
	}
	if inst != nil {
		if err := c.p.onStoppedLeadingExcluding(member.key.svcCtx, member.lease, member.service, inst, stopSet, leadershipLost, forceDelete); err != nil {
			return err
		}
	}
	member.activationWG.Wait()
	return nil
}

func (c *serviceElection) stopMemberWithRetry(member *serviceElectionMember) error {
	delay := 10 * time.Millisecond
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		err = c.stopMember(member)
		if err == nil {
			return nil
		}
		metrics.ServiceReconcileErrorsTotal.WithLabelValues(member.service.Namespace, member.service.Name, "delete_service").Inc()
		if attempt == 4 {
			break
		}
		timer := time.NewTimer(delay)
		select {
		case <-member.key.svcCtx.Parent().Done():
			timer.Stop()
		case <-timer.C:
		}
		delay *= 2
	}
	return err
}

func (c *serviceElection) memberCanActivate(member *serviceElectionMember, epoch uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.campaign != nil && c.campaign.epoch == epoch && c.elected && !c.stopping && !member.removing && member.active && member.activeEpoch == epoch
}

func (c *serviceElection) retryMemberCleanup(ctx context.Context, member *serviceElectionMember) error {
	delay := 200 * time.Millisecond
	for {
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			if err := c.stopMember(member); err != nil {
				return c.failMemberCleanup(member, err)
			}
			c.retireMember(member)
			return nil
		}

		if err := c.stopMember(member); err != nil {
			metrics.ServiceReconcileErrorsTotal.WithLabelValues(member.service.Namespace, member.service.Name, "delete_service").Inc()
			if delay < time.Second {
				delay *= 2
			}
			continue
		}

		c.retireMember(member)
		return nil
	}
}

func (c *serviceElection) failMemberCleanup(member *serviceElectionMember, cleanupErr error) error {
	err := fmt.Errorf("%w for service %s/%s: %w", errServiceCleanupShutdown, member.service.Namespace, member.service.Name, cleanupErr)
	member.key.svcCtx.SetLeaderError(err)
	var finalizers []func()
	failed := false
	c.mu.Lock()
	if c.members[member.key] == member && !member.finished {
		member.err = err
		member.active, member.cleaning, member.retrying, member.finished = false, false, false, true
		finalizers = append(finalizers, member.finalizers...)
		member.finalizers = nil
		delete(c.members, member.key)
		member.cleanupOnce.Do(func() { close(member.cleanupAttempted) })
		close(member.retired)
		c.signalLocked()
		failed = true
	}
	c.mu.Unlock()
	for _, finalize := range finalizers {
		finalize()
	}
	if failed {
		c.cancelCampaignIfEmpty()
	}
	log.Error("service cleanup abandoned during watcher shutdown", "service", member.service.Name, "namespace", member.service.Namespace, "err", err)
	return err
}

func (c *serviceElection) retireMember(member *serviceElectionMember) {
	member.key.svcCtx.SetLeaderError(nil)
	c.mu.Lock()
	if c.members[member.key] != member {
		c.mu.Unlock()
		return
	}
	member.active, member.cleaning, member.retrying, member.finished = false, false, false, true
	member.instance, member.err, member.activationCancel = nil, nil, nil
	member.stopSet, member.forceDelete, member.leadershipLost, member.stopPrepared = nil, nil, false, nil
	delete(c.members, member.key)
	finalizers := append([]func(){}, member.finalizers...)
	member.finalizers = nil
	c.signalLocked()
	c.mu.Unlock()
	c.p.leaseMgr.Delete(c.id, member.key.claim, member.lease)
	for _, finalize := range finalizers {
		finalize()
	}
	c.p.updateActiveServicesMetric()
	c.cancelCampaignIfEmpty()
	c.mu.Lock()
	close(member.retired)
	c.signalLocked()
	c.mu.Unlock()
}

func (c *serviceElection) waitForCampaignIfEmpty(member *serviceElectionMember) {
	c.mu.Lock()
	if _, exists := c.members[member.key]; exists || len(c.members) != 0 || c.campaign == nil {
		c.mu.Unlock()
		return
	}
	done := c.campaign.done
	c.mu.Unlock()
	select {
	case <-done:
	case <-member.key.svcCtx.Parent().Done():
	}
}

func (c *serviceElection) cancelCampaignIfEmpty() {
	c.p.electionsMu.Lock()
	c.mu.Lock()
	if c.p.elections[c.id.NamespacedName()] == c && len(c.members) == 0 {
		if c.campaign != nil {
			c.campaign.cancel()
		}
		if c.retryStop != nil {
			close(c.retryStop)
			c.retryStop = nil
			c.retrying = false
		}
		if c.campaign == nil {
			delete(c.p.elections, c.id.NamespacedName())
		}
	}
	c.mu.Unlock()
	c.p.electionsMu.Unlock()
}

func (c *serviceElection) startCampaignLocked() {
	if len(c.members) == 0 {
		return
	}
	keys := make([]electionMemberKey, 0, len(c.members))
	for key := range c.members {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].claim < keys[j].claim })
	var svcLease *lease.Lease
	var service *v1.Service
	for _, key := range keys {
		member := c.members[key]
		if member.cleaning || member.removing || member.finished || member.key.svcCtx.Ctx.Err() != nil || member.lease.Ctx.Err() != nil {
			continue
		}
		svcLease = member.lease
		service = member.service
		break
	}
	if svcLease == nil {
		return
	}
	c.epoch++
	ctx, cancel := context.WithCancel(svcLease.Ctx)
	campaign := &serviceCampaign{epoch: c.epoch, ctx: ctx, cancel: cancel, done: make(chan struct{}), lease: svcLease, service: service.DeepCopy(), owned: svcLease.BeginElection()}
	c.campaign = campaign
	if campaign.owned {
		go c.runCampaign(ctx, campaign)
	} else {
		go c.followCampaign(ctx, campaign)
	}
}

func (c *serviceElection) runCampaign(ctx context.Context, campaign *serviceCampaign) {
	metrics.ServiceElectionAttemptsTotal.WithLabelValues(campaign.service.Namespace, campaign.service.Name).Inc()
	run := election.RunConfig{
		Config: c.p.config, LeaseID: c.id, Mgr: c.p.electionMgr, LeaseAnnotations: map[string]string{},
		OnStartedLeading: func(context.Context) {
			if c.campaignStarted(campaign) {
				campaign.lease.ElectionStarted()
			}
		},
		OnStoppedLeading: func() { c.campaignStoppedAndWait(campaign.epoch) },
		OnNewLeader:      func(string) {},
	}
	runner := c.p.electionRun
	if runner == nil {
		runner = election.RunOrDie
	}
	if err := runner(ctx, &run, c.p.config); err != nil {
		log.Error("services election failed", "lease", c.id.NamespacedName(), "err", err)
	}
	c.campaignStoppedAndWait(campaign.epoch)
	c.finishCampaign(campaign)
}

func (c *serviceElection) followCampaign(ctx context.Context, campaign *serviceCampaign) {
	if campaign.lease.WaitForLeader(ctx) && c.campaignStarted(campaign) {
		campaign.lease.WaitForElectionEnd(ctx)
	}
	c.campaignStoppedAndWait(campaign.epoch)
	c.finishCampaign(campaign)
}

func (c *serviceElection) finishCampaign(campaign *serviceCampaign) {
	c.mu.Lock()
	for c.hasActiveLocked() {
		changed := c.changed
		c.mu.Unlock()
		<-changed
		c.mu.Lock()
	}
	if c.campaign == campaign {
		c.campaign, c.elected, c.stopping = nil, false, false
		if campaign.owned {
			campaign.lease.ElectionStopped()
		}
		close(campaign.done)
		c.signalLocked()
		if len(c.members) != 0 {
			c.scheduleCampaignLocked()
		}
	}
	empty := len(c.members) == 0
	c.mu.Unlock()
	if empty {
		c.p.removeServiceElection(c)
	}
}

func (c *serviceElection) campaignStarted(campaign *serviceCampaign) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.campaign != campaign || c.stopping || c.elected {
		return false
	}
	c.elected = true
	campaign.started = true
	c.backoff = 0
	if campaign.owned {
		metrics.LeaderTransitionsTotal.WithLabelValues(c.id.Name()).Inc()
		metrics.IsLeader.WithLabelValues(c.p.config.NodeName, c.id.Name()).Set(1)
	}
	c.signalLocked()
	return true
}

func (c *serviceElection) campaignStopped(epoch uint64) {
	c.mu.Lock()
	if c.campaign == nil || c.campaign.epoch != epoch {
		c.mu.Unlock()
		return
	}
	if c.stopping {
		c.mu.Unlock()
		return
	}
	c.elected, c.stopping = false, true
	type stoppingMember struct {
		member         *serviceElectionMember
		instance       *instance.Instance
		cancel         context.CancelFunc
		leadershipLost bool
	}
	stopping := make([]stoppingMember, 0, len(c.members))
	stopSet := make(map[*instance.Instance]struct{})
	stopPrepared := make(chan struct{})
	for _, member := range c.members {
		if member.activeEpoch == epoch && member.activationCancel != nil {
			leadershipLost := !member.removing && member.key.svcCtx.Ctx.Err() == nil
			if leadershipLost {
				select {
				case <-member.key.lost:
					leadershipLost = false
				default:
				}
			}
			stopping = append(stopping, stoppingMember{member: member, instance: member.instance, cancel: member.activationCancel, leadershipLost: leadershipLost})
			if member.instance != nil {
				stopSet[member.instance] = struct{}{}
			}
		}
	}
	for _, item := range stopping {
		item.member.stopSet = stopSet
		item.member.leadershipLost = item.leadershipLost
		item.member.stopPrepared = stopPrepared
	}
	if c.campaign.owned && c.campaign.started {
		metrics.IsLeader.WithLabelValues(c.p.config.NodeName, c.id.Name()).Set(0)
	}
	c.mu.Unlock()
	instances := make([]*instance.Instance, 0, len(stopSet))
	leadershipLost := make(map[*instance.Instance]bool, len(stopSet))
	for inst := range stopSet {
		instances = append(instances, inst)
	}
	for _, item := range stopping {
		if item.instance != nil && item.leadershipLost {
			leadershipLost[item.instance] = true
		}
	}
	forceDeletes := c.p.prepareServiceInstancesCampaignStop(instances, leadershipLost)
	c.mu.Lock()
	for _, item := range stopping {
		forceDelete := forceDeletes[item.instance]
		if forceDelete == nil {
			forceDelete = make(map[string]struct{})
		}
		item.member.forceDelete = forceDelete
	}
	close(stopPrepared)
	c.signalLocked()
	c.mu.Unlock()
	for _, item := range stopping {
		item.cancel()
	}
}

func (c *serviceElection) campaignStoppedAndWait(epoch uint64) {
	c.campaignStopped(epoch)
	c.mu.Lock()
	for c.campaign != nil && c.campaign.epoch == epoch && c.hasActiveLocked() {
		changed := c.changed
		c.mu.Unlock()
		<-changed
		c.mu.Lock()
	}
	c.mu.Unlock()
}

func (c *serviceElection) scheduleCampaignLocked() {
	if c.retrying || !c.hasCampaignMemberLocked() {
		return
	}
	if c.backoff == 0 {
		c.backoff = 50 * time.Millisecond
	} else if c.backoff < time.Second {
		c.backoff *= 2
	}
	delay := c.backoff
	c.retrying = true
	stop := make(chan struct{})
	c.retryStop = stop
	go func() {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-stop:
			return
		}
		c.mu.Lock()
		if c.retryStop != stop {
			c.mu.Unlock()
			return
		}
		c.retryStop = nil
		c.retrying = false
		if c.campaign == nil && !c.stopping && c.hasCampaignMemberLocked() {
			c.startCampaignLocked()
		}
		empty := len(c.members) == 0
		c.mu.Unlock()
		if empty {
			c.p.removeServiceElection(c)
		}
	}()
}

func (c *serviceElection) hasCampaignMemberLocked() bool {
	for _, member := range c.members {
		if !member.cleaning && !member.removing && !member.finished && member.key.svcCtx.Ctx.Err() == nil && member.lease.Ctx.Err() == nil {
			return true
		}
	}
	return false
}

func (c *serviceElection) prepareAndCancelMember(member *serviceElectionMember, cancel context.CancelFunc) {
	c.prepareMemberStop(member)
	cancel()
}

func (c *serviceElection) prepareMemberStop(member *serviceElectionMember) {
	c.mu.Lock()
	inst := member.instance
	c.mu.Unlock()
	if inst != nil {
		c.p.prepareServiceInstanceStop(inst)
	}
}

func (c *serviceElection) hasActiveLocked() bool {
	for _, member := range c.members {
		if !member.finished && (member.active || member.cleaning) {
			return true
		}
	}
	return false
}

func (c *serviceElection) signalLocked() {
	close(c.changed)
	c.changed = make(chan struct{})
}

func (p *Processor) removeServiceElection(c *serviceElection) {
	p.electionsMu.Lock()
	defer p.electionsMu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	if p.elections[c.id.NamespacedName()] == c && len(c.members) == 0 && c.campaign == nil {
		delete(p.elections, c.id.NamespacedName())
	}
}

func (p *Processor) waitAndRetryServiceElectionMember(svcCtx *servicecontext.Context) error {
	c, member := p.serviceElectionMemberForContext(svcCtx)
	if member == nil {
		return nil
	}
	select {
	case <-member.retired:
		c.mu.Lock()
		err := member.err
		c.mu.Unlock()
		if err == nil {
			c.waitForCampaignIfEmpty(member)
		}
		return err
	case <-member.cleanupAttempted:
		c.mu.Lock()
		err := member.err
		c.mu.Unlock()
		return err
	case <-svcCtx.Parent().Done():
		return svcCtx.Parent().Err()
	}
}

func (p *Processor) finalizeServiceElectionMember(c *serviceElection, member *serviceElectionMember, finalize func()) {
	c.mu.Lock()
	registered := c.members[member.key] == member
	if registered {
		member.removing = true
		if member.stopSet == nil {
			member.leadershipLost = false
			member.forceDelete = nil
		}
		member.finalizers = append(member.finalizers, finalize)
	}
	activationCancel := member.activationCancel
	if registered {
		c.signalLocked()
	}
	c.mu.Unlock()
	if registered && activationCancel != nil {
		c.prepareAndCancelMember(member, activationCancel)
	}
	if !registered {
		finalize()
	}
}

func (p *Processor) serviceElectionMemberExists(service *v1.Service, svcCtx *servicecontext.Context) bool {
	_, member := p.serviceElectionMember(service, svcCtx)
	return member != nil
}
