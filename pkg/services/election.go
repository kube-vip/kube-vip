package services

import (
	"context"
	log "log/slog"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/kube-vip/kube-vip/pkg/election"
	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/metrics"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

// serviceElection owns local membership and campaign lifetime for one lease.
// Service contexts remain responsible for endpoint readiness and datapath work.
type serviceElection struct {
	processor *Processor
	id        lease.ID

	mutex        sync.Mutex
	members      map[types.UID]*serviceElectionMember
	lease        *lease.Lease
	campaign     *serviceElectionCampaign
	retired      bool
	retiredDone  chan struct{}
	retiredCtx   context.Context
	retireCancel context.CancelFunc

	// restartFailures counts consecutive campaigns that ended via
	// cancelCampaign (an activation failure with no other ready member)
	// rather than a normal leadership change. It backs off campaign restarts
	// and resets on the next successful activation.
	restartFailures int
}

const (
	serviceElectionRestartBaseDelay = 200 * time.Millisecond
	serviceElectionRestartMaxDelay  = 30 * time.Second
)

type serviceElectionCampaign struct {
	done         chan struct{}
	ctx          context.Context
	cancel       context.CancelFunc
	leaderCtx    context.Context
	cancelLeader context.CancelFunc
	vips         []string
	external     bool
	stopped      bool
}

func (campaign *serviceElectionCampaign) cancelRunner() {
	if campaign != nil && campaign.cancel != nil {
		campaign.cancel()
	}
}

type serviceElectionMember struct {
	election            *serviceElection
	service             *v1.Service
	serviceContext      *servicecontext.Context
	readinessGeneration uint64
	claimToken          string
	operationMutex      sync.Mutex
	active              bool
}

func (p *Processor) serviceElectionFor(id lease.ID) *serviceElection {
	p.electionsMutex.Lock()
	defer p.electionsMutex.Unlock()
	if p.elections == nil {
		p.elections = make(map[string]*serviceElection)
	}
	key := id.NamespacedName()
	if election := p.elections[key]; election != nil {
		return election
	}
	retiredCtx, retire := context.WithCancel(context.Background())
	election := &serviceElection{
		processor:    p,
		id:           id,
		members:      make(map[types.UID]*serviceElectionMember),
		retiredDone:  make(chan struct{}),
		retiredCtx:   retiredCtx,
		retireCancel: retire,
	}
	p.elections[key] = election
	return election
}

// joinServiceElection registers the current ready generation of a Service. A
// caller that races coordinator retirement retries against its replacement.
func (p *Processor) joinServiceElection(svcCtx *servicecontext.Context, service *v1.Service,
	readinessGeneration uint64) (*serviceElectionMember, bool) {
	if svcCtx == nil || service == nil || p.leaseMgr == nil {
		return nil, false
	}

	namespace, name := lease.ServiceName(service)
	id := lease.NewID(p.config.LeaderElectionType, namespace, name)
	for {
		if !p.serviceElectionContextCurrent(svcCtx, service, readinessGeneration) {
			return nil, false
		}
		election := p.serviceElectionFor(id)
		member, joined := election.join(svcCtx, service, readinessGeneration)
		if joined {
			if p.serviceElectionContextCurrent(svcCtx, service, readinessGeneration) {
				return member, true
			}
			p.leaveServiceElection(member)
			return nil, false
		}
		if retiredDone, retired := election.retirement(); retired {
			select {
			case <-svcCtx.Ctx.Done():
				return nil, false
			case <-retiredDone:
				continue
			}
		}
		return nil, false
	}
}

// serviceElectionContextCurrent acquires the Service lock while comparing the
// current context. Callers must not already hold that lock.
func (p *Processor) serviceElectionContextCurrent(svcCtx *servicecontext.Context, service *v1.Service, readinessGeneration uint64) bool {
	currentContext, err := p.currentServiceContext(service.UID)
	if err != nil || currentContext != svcCtx || svcCtx.Ctx.Err() != nil {
		return false
	}
	return svcCtx.ReadinessGenerationCurrent(readinessGeneration)
}

// currentServiceContext acquires and releases the Service lock around one
// svcMap read.
func (p *Processor) currentServiceContext(uid types.UID) (*servicecontext.Context, error) {
	unlockService := p.lockService(uid)
	defer unlockService()
	return p.getServiceContext(uid)
}

func (e *serviceElection) join(svcCtx *servicecontext.Context, service *v1.Service,
	readinessGeneration uint64) (*serviceElectionMember, bool) {
	e.mutex.Lock()
	defer e.mutex.Unlock()
	if e.retired {
		return nil, false
	}
	if member := e.members[service.UID]; member != nil && member.serviceContext == svcCtx &&
		member.readinessGeneration == readinessGeneration {
		return member, true
	}

	if previous := e.members[service.UID]; previous != nil {
		e.processor.leaseMgr.Delete(e.id, previous.claimToken, e.lease)
	}
	member := &serviceElectionMember{
		election:            e,
		service:             service.DeepCopy(),
		serviceContext:      svcCtx,
		readinessGeneration: readinessGeneration,
		claimToken:          e.nextMemberToken(),
	}
	e.members[service.UID] = member

	// A member can become ready again while the old campaign is still stopping.
	// Keep its new generation until that runner finishes; finishCampaign will
	// rebuild the lease and launch the replacement campaign.
	if e.lease != nil && e.lease.Ctx.Err() != nil && e.campaign != nil {
		return member, true
	}
	if e.lease == nil || e.lease.Ctx.Err() != nil {
		if e.createLeaseLocked() == nil {
			delete(e.members, service.UID)
			return nil, false
		}
		return member, true
	}
	if claimed, _ := e.processor.leaseMgr.Claim(e.id, member.claimToken); claimed != nil {
		return member, true
	}

	// An external cleanup retired the manager entry. Rebuild it from the live
	// coordinator snapshot rather than admitting a member to a dead lease.
	e.lease = nil
	if e.createLeaseLocked() == nil {
		delete(e.members, service.UID)
		return nil, false
	}
	return member, true
}

func (e *serviceElection) createLeaseLocked() *lease.Lease {
	if e.lease != nil && e.lease.Ctx.Err() == nil {
		return e.lease
	}
	var first *serviceElectionMember
	for _, member := range e.members {
		first = member
		break
	}
	if first == nil {
		return nil
	}
	svcLease, _ := e.processor.leaseMgr.Acquire(context.Background(), e.id, first.claimToken)
	for _, member := range e.members {
		if member == first {
			continue
		}
		if claimed, _ := e.processor.leaseMgr.Claim(e.id, member.claimToken); claimed == nil {
			svcLease.Cancel()
			return nil
		}
	}
	e.lease = svcLease
	return svcLease
}

func (e *serviceElection) nextMemberToken() string {
	return strconv.FormatUint(e.processor.nextMemberToken.Add(1), 10)
}

// leaveServiceElection removes only the supplied member generation. A stale
// member cannot remove a replacement Service context or readiness generation.
func (p *Processor) leaveServiceElection(member *serviceElectionMember) {
	if member == nil {
		return
	}
	member.election.leave(member)
}

func (p *Processor) leaveServiceElectionForContext(svcCtx *servicecontext.Context, service *v1.Service) {
	if svcCtx == nil || service == nil {
		return
	}
	namespace, name := lease.ServiceName(service)
	id := lease.NewID(p.config.LeaderElectionType, namespace, name)
	election := p.currentServiceElection(id)
	if election == nil {
		return
	}
	member := election.currentMember(service.UID)
	if member != nil && member.serviceContext == svcCtx {
		p.leaveServiceElection(member)
	}
}

func (p *Processor) currentServiceElection(id lease.ID) *serviceElection {
	p.electionsMutex.Lock()
	defer p.electionsMutex.Unlock()
	return p.elections[id.NamespacedName()]
}

func (e *serviceElection) currentMember(uid types.UID) *serviceElectionMember {
	e.mutex.Lock()
	defer e.mutex.Unlock()
	return e.members[uid]
}

func (e *serviceElection) leave(member *serviceElectionMember) {
	campaign, leaseRetired, retired := e.removeMember(member)
	if !retired {
		return
	}
	e.retire()
	if campaign != nil && (campaign.external || leaseRetired) {
		campaign.cancelRunner()
	}
	// A Service-owned runner remains responsible for the shared election when
	// a non-Service member still holds the lease. Its lease-scoped context ends
	// when that final member leaves or the election itself stops.
}

// removeMember deletes member if it is still current and, once no members
// remain, marks the election retired and reports whether deleting its claim
// also retired the shared lease.
func (e *serviceElection) removeMember(member *serviceElectionMember) (campaign *serviceElectionCampaign, leaseRetired, retired bool) {
	e.mutex.Lock()
	defer e.mutex.Unlock()
	if e.members[member.service.UID] != member {
		return nil, false, false
	}
	delete(e.members, member.service.UID)
	leaseRetired = e.processor.leaseMgr.Delete(e.id, member.claimToken, e.lease)
	if len(e.members) != 0 {
		return nil, leaseRetired, false
	}
	e.retired = true
	campaign = e.campaign
	e.lease = nil
	return campaign, leaseRetired, true
}

func (e *serviceElection) retirement() (<-chan struct{}, bool) {
	e.mutex.Lock()
	defer e.mutex.Unlock()
	return e.retiredDone, e.retired
}

func (e *serviceElection) retire() {
	if e.retireCancel != nil {
		e.retireCancel()
	}
	e.processor.removeServiceElection(e)
	close(e.retiredDone)
}

func (p *Processor) removeServiceElection(election *serviceElection) {
	p.electionsMutex.Lock()
	defer p.electionsMutex.Unlock()
	if p.elections[election.id.NamespacedName()] == election {
		delete(p.elections, election.id.NamespacedName())
	}
}

func (p *Processor) watchServiceElection(svcCtx *servicecontext.Context, service *v1.Service,
	wg *sync.WaitGroup) {
	for {
		generation, ready, lost, isReady := svcCtx.ReadinessState()
		if !isReady {
			select {
			case <-svcCtx.Ctx.Done():
				return
			case <-ready:
				continue
			}
		}

		member, joined := p.joinServiceElection(svcCtx, service, generation)
		if !joined {
			if !p.serviceElectionContextCurrent(svcCtx, service, generation) {
				return
			}
			select {
			case <-svcCtx.Ctx.Done():
				return
			case <-time.After(serviceElectionRestartBaseDelay):
				continue
			}
		}
		member.election.startCampaign(wg)

		select {
		case <-svcCtx.Ctx.Done():
			member.election.deactivateMember(member)
			p.leaveServiceElection(member)
			return
		case <-lost:
			member.election.deactivateMember(member)
			p.leaveServiceElection(member)
		}
	}
}

// campaignStart carries the decision taken under the election mutex so the
// caller can act on it without holding the lock.
type campaignStart struct {
	lease        *lease.Lease
	campaign     *serviceElectionCampaign
	leaderCtx    context.Context
	members      []*serviceElectionMember
	joinExisting bool
	external     bool
}

func (e *serviceElection) prepareCampaign() campaignStart {
	e.mutex.Lock()
	defer e.mutex.Unlock()

	if e.retired || len(e.members) == 0 {
		return campaignStart{}
	}
	if e.campaign != nil {
		return campaignStart{
			lease:        e.lease,
			campaign:     e.campaign,
			leaderCtx:    e.campaign.leaderCtx,
			joinExisting: true,
		}
	}
	svcLease := e.createLeaseLocked()
	if svcLease == nil {
		return campaignStart{}
	}
	external := !svcLease.BeginElection()
	campaignCtx, campaignCancel := svcLease.NewElectionContext(context.Background())
	members := e.membersLocked()
	campaign := &serviceElectionCampaign{
		done:     make(chan struct{}),
		ctx:      campaignCtx,
		cancel:   campaignCancel,
		vips:     memberVIPs(members),
		external: external,
	}
	e.campaign = campaign
	return campaignStart{lease: svcLease, campaign: campaign, members: members, external: external}
}

func (e *serviceElection) startCampaign(wg *sync.WaitGroup) {
	start := e.prepareCampaign()
	if start.campaign == nil {
		return
	}
	if start.joinExisting {
		if start.leaderCtx != nil {
			e.activateMembers(start.leaderCtx, start.lease, start.campaign, wg)
		}
		return
	}
	if start.external {
		wg.Go(func() {
			e.followCampaign(start.lease, start.campaign, wg)
		})
		return
	}
	for _, member := range start.members {
		metrics.ServiceElectionAttemptsTotal.WithLabelValues(member.service.Namespace, member.service.Name).Inc()
	}

	wg.Go(func() {
		e.runCampaign(start.lease, start.campaign, wg)
	})
}

// adoptLeaderContext publishes the leader context for a campaign that just won
// an externally driven election.
func (e *serviceElection) adoptLeaderContext(svcLease *lease.Lease, campaign *serviceElectionCampaign,
	leaderCtx context.Context, cancelLeader context.CancelFunc) bool {
	e.mutex.Lock()
	defer e.mutex.Unlock()

	if e.retired || e.lease != svcLease || e.campaign != campaign || campaign.stopped {
		return false
	}
	campaign.leaderCtx = leaderCtx
	campaign.cancelLeader = cancelLeader
	return true
}

func (e *serviceElection) followCampaign(svcLease *lease.Lease, campaign *serviceElectionCampaign, wg *sync.WaitGroup) {
	defer close(campaign.done)
	defer campaign.cancelRunner()
	leaderGeneration, elected := svcLease.WaitForLeaderGeneration(campaign.ctx)
	if !elected {
		e.stopCampaign(svcLease, campaign)
		e.finishCampaign(svcLease, campaign, wg)
		return
	}
	leaderCtx, cancelLeader := context.WithCancel(campaign.ctx)
	if !e.adoptLeaderContext(svcLease, campaign, leaderCtx, cancelLeader) {
		cancelLeader()
		return
	}
	e.activateMembers(leaderCtx, svcLease, campaign, wg)
	svcLease.WaitForElectionEndAfter(campaign.ctx, leaderGeneration)
	cancelLeader()
	e.stopCampaign(svcLease, campaign)
	e.finishCampaign(svcLease, campaign, wg)
}

func (e *serviceElection) runCampaign(svcLease *lease.Lease, campaign *serviceElectionCampaign, wg *sync.WaitGroup) {
	defer close(campaign.done)
	defer campaign.cancelRunner()
	run := election.RunConfig{
		Config:           e.processor.config,
		LeaseID:          e.id,
		Mgr:              e.processor.electionMgr,
		LeaseAnnotations: map[string]string{},
		VIPs:             campaign.vips,
		OnStartedLeading: func(ctx context.Context) {
			e.startedLeading(ctx, svcLease, campaign, wg)
		},
		OnStoppedLeading: func() {
			e.stopCampaign(svcLease, campaign)
			metrics.IsLeader.WithLabelValues(e.processor.config.NodeName, e.id.Name()).Set(0)
		},
		OnNewLeader: func(identity string) {
			if identity != e.processor.config.NodeName {
				log.Info("new leader", "leader", identity, "lease", e.id.NamespacedName())
			}
		},
	}
	if err := e.processor.runElection(campaign.ctx, &run); err != nil {
		log.Error("services election failed", "lease", e.id.NamespacedName(), "error", err)
	}
	e.stopCampaign(svcLease, campaign)
	svcLease.ElectionStopped()
	e.finishCampaign(svcLease, campaign, wg)
}

func memberVIPs(members []*serviceElectionMember) []string {
	services := make([]*v1.Service, 0, len(members))
	for _, member := range members {
		if member != nil && member.service != nil {
			services = append(services, member.service)
		}
	}
	return orderedServiceVIPs(services)
}

func orderedServiceVIPs(services []*v1.Service) []string {
	services = append([]*v1.Service(nil), services...)
	sort.SliceStable(services, func(first, second int) bool {
		firstService, secondService := services[first], services[second]
		if !firstService.CreationTimestamp.Equal(&secondService.CreationTimestamp) {
			return firstService.CreationTimestamp.Before(&secondService.CreationTimestamp)
		}
		if firstService.Namespace != secondService.Namespace {
			return firstService.Namespace < secondService.Namespace
		}
		if firstService.Name != secondService.Name {
			return firstService.Name < secondService.Name
		}
		return firstService.UID < secondService.UID
	})

	vips := make([]string, 0)
	for _, service := range services {
		addresses, _ := instance.FetchServiceAddresses(service)
		vips = append(vips, addresses...)
	}
	return vips
}

func (p *Processor) runElection(ctx context.Context, run *election.RunConfig) error {
	if p.electionRun != nil {
		return p.electionRun(ctx, run, p.config)
	}
	return election.RunOrDie(ctx, run, p.config)
}

func (e *serviceElection) membersLocked() []*serviceElectionMember {
	members := make([]*serviceElectionMember, 0, len(e.members))
	for _, member := range e.members {
		members = append(members, member)
	}
	return members
}

// beginLeading records the leader context for a campaign this process won.
func (e *serviceElection) beginLeading(ctx context.Context, svcLease *lease.Lease,
	campaign *serviceElectionCampaign) bool {
	e.mutex.Lock()
	defer e.mutex.Unlock()

	if e.retired || e.lease != svcLease || e.campaign != campaign || campaign.stopped || len(e.members) == 0 {
		return false
	}
	campaign.leaderCtx = ctx
	svcLease.ElectionStarted()
	return true
}

func (e *serviceElection) startedLeading(ctx context.Context, svcLease *lease.Lease,
	campaign *serviceElectionCampaign, wg *sync.WaitGroup) {
	if !e.beginLeading(ctx, svcLease, campaign) {
		return
	}
	metrics.LeaderTransitionsTotal.WithLabelValues(e.id.Name()).Inc()
	metrics.IsLeader.WithLabelValues(e.processor.config.NodeName, e.id.Name()).Set(1)
	e.activateMembers(ctx, svcLease, campaign, wg)
}

// activatableMembers snapshots the members eligible for activation, or nil when
// the campaign is no longer current.
func (e *serviceElection) activatableMembers(svcLease *lease.Lease,
	campaign *serviceElectionCampaign) []*serviceElectionMember {
	e.mutex.Lock()
	defer e.mutex.Unlock()

	if e.retired || e.lease != svcLease || e.campaign != campaign || (campaign != nil && campaign.stopped) || !svcLease.Elected.Load() {
		return nil
	}
	return e.membersLocked()
}

func (e *serviceElection) activateMembers(ctx context.Context, svcLease *lease.Lease,
	campaign *serviceElectionCampaign, wg *sync.WaitGroup) {
	if svcLease == nil || !svcLease.Elected.Load() {
		return
	}
	for _, member := range e.activatableMembers(svcLease, campaign) {
		e.activateMember(ctx, member, svcLease, campaign, wg)
	}
}

func (e *serviceElection) activateMember(ctx context.Context, member *serviceElectionMember, svcLease *lease.Lease,
	campaign *serviceElectionCampaign, wg *sync.WaitGroup) {
	releaseReadiness, ready := member.serviceContext.AcquireReadinessGeneration(member.readinessGeneration)
	if !ready {
		return
	}
	defer releaseReadiness()

	member.operationMutex.Lock()
	defer member.operationMutex.Unlock()

	if !e.processor.serviceElectionMemberCurrent(member) || !e.markMemberActive(member, svcLease, campaign) {
		return
	}
	if err := e.processor.syncServices(ctx, member.serviceContext, member.service, wg, true); err != nil {
		metrics.ServiceElectionErrorsTotal.WithLabelValues(member.service.Namespace, member.service.Name, "service_sync").Inc()
		log.Error("start service after election", "service", member.service.Name, "namespace", member.service.Namespace, "error", err)
		e.deactivateMemberOperationHeld(member)
		if !e.hasOtherReadyMember(member) {
			e.cancelCampaign(svcLease, campaign)
		}
		return
	}
	e.resetRestartFailures()
	if !e.processor.serviceElectionMemberCurrent(member) || !e.memberActivationCurrent(member, svcLease, campaign) {
		e.deactivateMemberOperationHeld(member)
	}
}

func (e *serviceElection) resetRestartFailures() {
	e.mutex.Lock()
	defer e.mutex.Unlock()
	e.restartFailures = 0
}

func (p *Processor) syncServices(operationCtx context.Context, svcCtx *servicecontext.Context,
	service *v1.Service, wg *sync.WaitGroup, usesLeaderElection bool) error {
	if p.serviceSync != nil {
		return p.serviceSync(operationCtx, svcCtx, service, wg, usesLeaderElection)
	}
	return p.syncServicesWithContext(operationCtx, svcCtx, service, wg, usesLeaderElection)
}

func (e *serviceElection) memberActivationCurrent(member *serviceElectionMember, svcLease *lease.Lease,
	campaign *serviceElectionCampaign) bool {
	e.mutex.Lock()
	defer e.mutex.Unlock()
	return e.memberActivationCurrentLocked(member, svcLease, campaign) && member.active
}

func (e *serviceElection) memberActivationCurrentLocked(member *serviceElectionMember, svcLease *lease.Lease,
	campaign *serviceElectionCampaign) bool {
	return !e.retired && e.lease == svcLease && e.campaign == campaign &&
		(campaign == nil || !campaign.stopped) && svcLease.Elected.Load() &&
		e.members[member.service.UID] == member
}

func (e *serviceElection) markMemberActive(member *serviceElectionMember, svcLease *lease.Lease, campaign *serviceElectionCampaign) bool {
	e.mutex.Lock()
	defer e.mutex.Unlock()
	if !e.memberActivationCurrentLocked(member, svcLease, campaign) || member.active {
		return false
	}
	member.active = true
	return true
}

func (e *serviceElection) hasOtherReadyMember(member *serviceElectionMember) bool {
	for _, candidate := range e.otherMembers(member) {
		if candidate.serviceContext.Ctx.Err() == nil && candidate.serviceContext.ReadinessGenerationCurrent(candidate.readinessGeneration) {
			return true
		}
	}
	return false
}

// otherMembers returns every member except the supplied one.
func (e *serviceElection) otherMembers(member *serviceElectionMember) []*serviceElectionMember {
	e.mutex.Lock()
	defer e.mutex.Unlock()
	others := make([]*serviceElectionMember, 0, len(e.members))
	for _, candidate := range e.members {
		if candidate != member {
			others = append(others, candidate)
		}
	}
	return others
}

func (e *serviceElection) deactivateMember(member *serviceElectionMember) {
	member.operationMutex.Lock()
	defer member.operationMutex.Unlock()
	e.deactivateMemberOperationHeld(member)
}

// markMemberInactive clears the active flag and reports the lease that the
// caller must run cleanup against.
func (e *serviceElection) markMemberInactive(member *serviceElectionMember) (*lease.Lease, bool) {
	e.mutex.Lock()
	defer e.mutex.Unlock()

	if e.members[member.service.UID] != member || !member.active {
		return nil, false
	}
	member.active = false
	return e.lease, true
}

func (e *serviceElection) deactivateMemberOperationHeld(member *serviceElectionMember) {
	svcLease, deactivated := e.markMemberInactive(member)
	if !deactivated {
		return
	}
	e.cleanupMember(member, svcLease)
}

// serviceElectionMemberCurrent acquires and releases the Service lock before
// acquiring the election mutex. Callers must not already hold the Service lock.
func (p *Processor) serviceElectionMemberCurrent(member *serviceElectionMember) bool {
	currentCtx, err := p.currentServiceContext(member.service.UID)
	current := err == nil && currentCtx == member.serviceContext && member.serviceContext.Ctx.Err() == nil &&
		member.serviceContext.ReadinessGenerationCurrent(member.readinessGeneration)
	if !current {
		return false
	}

	member.election.mutex.Lock()
	defer member.election.mutex.Unlock()
	return !member.election.retired && member.election.members[member.service.UID] == member
}

func (e *serviceElection) cleanupMember(member *serviceElectionMember, svcLease *lease.Lease) {
	if svcLease == nil {
		return
	}
	if err := e.processor.onStoppedLeadingMember(member, svcLease); err != nil {
		log.Error("stop service after election", "service", member.service.Name, "namespace", member.service.Namespace, "error", err)
	}
}

// markCampaignStopped retires the campaign and returns the members whose
// datapath the caller must tear down outside the lock.
func (e *serviceElection) markCampaignStopped(svcLease *lease.Lease,
	campaign *serviceElectionCampaign) []*serviceElectionMember {
	e.mutex.Lock()
	defer e.mutex.Unlock()

	if e.retired || e.lease != svcLease || e.campaign != campaign || campaign.stopped {
		return nil
	}
	campaign.stopped = true
	if campaign.cancelLeader != nil {
		campaign.cancelLeader()
	}
	if !campaign.external {
		svcLease.ElectionStopped()
	}
	return e.membersLocked()
}

func (e *serviceElection) stopCampaign(svcLease *lease.Lease, campaign *serviceElectionCampaign) {
	for _, member := range e.markCampaignStopped(svcLease, campaign) {
		e.deactivateMember(member)
	}
}

// recordCampaignFailure counts an activation failure for the restart backoff.
func (e *serviceElection) recordCampaignFailure(svcLease *lease.Lease, campaign *serviceElectionCampaign) bool {
	e.mutex.Lock()
	defer e.mutex.Unlock()

	if e.retired || e.lease != svcLease || e.campaign != campaign {
		return false
	}
	e.restartFailures++
	return true
}

func (e *serviceElection) cancelCampaign(svcLease *lease.Lease, campaign *serviceElectionCampaign) {
	if !e.recordCampaignFailure(svcLease, campaign) {
		return
	}
	campaign.cancelRunner()
}

// completeCampaign clears the finished campaign and reports whether a restart
// is still needed, along with its backoff delay.
func (e *serviceElection) completeCampaign(svcLease *lease.Lease,
	campaign *serviceElectionCampaign) (bool, time.Duration) {
	e.mutex.Lock()
	defer e.mutex.Unlock()

	if e.retired || e.lease != svcLease || e.campaign != campaign {
		return false, 0
	}
	e.campaign = nil
	if svcLease.Ctx.Err() != nil {
		e.lease = nil
	}
	return len(e.members) != 0, e.restartDelayLocked()
}

func (e *serviceElection) finishCampaign(svcLease *lease.Lease, campaign *serviceElectionCampaign, wg *sync.WaitGroup) {
	restart, delay := e.completeCampaign(svcLease, campaign)
	if !restart {
		return
	}

	e.processor.scheduleServiceElectionRestart(e.retiredCtx, delay, wg, func() {
		e.startCampaign(wg)
	})
}

// restartDelayLocked doubles the restart delay for each consecutive
// activation failure, capped at serviceElectionRestartMaxDelay, so a
// persistently broken Service does not spin the Lease and VIP in a tight
// add/delete loop. The caller must hold e.mutex.
func (e *serviceElection) restartDelayLocked() time.Duration {
	delay := serviceElectionRestartBaseDelay
	for i := 0; i < e.restartFailures && delay < serviceElectionRestartMaxDelay; i++ {
		delay *= 2
	}
	if delay > serviceElectionRestartMaxDelay {
		delay = serviceElectionRestartMaxDelay
	}
	return delay
}

func (p *Processor) scheduleServiceElectionRestart(ctx context.Context, delay time.Duration, wg *sync.WaitGroup, restart func()) {
	if p.scheduleElectionRestart != nil {
		p.scheduleElectionRestart(restart)
		return
	}
	wg.Go(func() {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			restart()
		}
	})
}
