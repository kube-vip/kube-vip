package services

import (
	"context"
	"errors"
	log "log/slog"
	"sync"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
)

type pendingReconcile struct {
	member        *serviceElectionMember
	version       uint64
	ctx           context.Context
	event         watch.Event
	serviceFunc   *Callback
	forcedOnly    bool
	group         *cleanupGroup
	cancelWatcher context.CancelCauseFunc
	beforeReplay  func()
}

type cleanupGroup struct {
	ctx     context.Context
	wg      sync.WaitGroup
	mu      sync.Mutex
	err     error
	watchWG *sync.WaitGroup
	active  bool
}

func (p *Processor) queuePendingReconcile(c *serviceElection, member *serviceElectionMember, ctx context.Context,
	event watch.Event, serviceFunc *Callback, forcedOnly bool, wg *sync.WaitGroup, cancelWatcher context.CancelCauseFunc, version uint64) {
	service, ok := event.Object.(*v1.Service)
	if !ok || service == nil || ctx.Err() != nil {
		return
	}

	p.pendingMu.Lock()
	if p.pending == nil {
		p.pending = make(map[types.UID]*pendingReconcile)
	}
	pending := p.pending[service.UID]
	if pending != nil && pending.member == member {
		pending.version = version
		pending.ctx = ctx
		pending.event = watch.Event{Type: event.Type, Object: service.DeepCopy()}
		pending.serviceFunc = serviceFunc
		pending.forcedOnly = forcedOnly
		pending.cancelWatcher = cancelWatcher
		p.pendingMu.Unlock()
		return
	}
	if pending != nil {
		p.pendingMu.Unlock()
		return
	}

	group := p.cleanupGroup(ctx)
	group.mu.Lock()
	if group.watchWG == nil {
		group.watchWG = wg
	}
	group.mu.Unlock()
	pending = &pendingReconcile{
		member: member, version: version, ctx: ctx, event: watch.Event{Type: event.Type, Object: service.DeepCopy()},
		serviceFunc: serviceFunc, forcedOnly: forcedOnly, group: group, cancelWatcher: cancelWatcher,
	}
	p.pending[service.UID] = pending
	p.pendingMu.Unlock()

	p.finalizeServiceElectionMember(c, member, func() {
		p.svcMap.CompareAndDelete(service.UID, member.key.svcCtx)
		p.replayPendingReconcile(service.UID, pending)
	})
}

func (p *Processor) replayPendingReconcile(uid types.UID, pending *pendingReconcile) {
	p.pendingMu.Lock()
	if p.pending[uid] != pending {
		p.pendingMu.Unlock()
		return
	}
	if !p.desiredEventCurrent(uid, pending.version) {
		delete(p.pending, uid)
		p.pendingMu.Unlock()
		return
	}
	delete(p.pending, uid)
	ctx, event, serviceFunc, version := pending.ctx, pending.event, pending.serviceFunc, pending.version
	forcedOnly, cancelWatcher := pending.forcedOnly, pending.cancelWatcher
	p.pendingMu.Unlock()

	wg, ok := pending.group.beginReplay()
	if !ok || event.Object == nil {
		return
	}
	defer wg.Done()
	if pending.beforeReplay != nil {
		pending.beforeReplay()
	}
	if !p.desiredEventCurrent(uid, version) {
		return
	}
	if err := p.reconcileDesired(ctx, event, serviceFunc, forcedOnly, wg, cancelWatcher, version); err != nil && ctx.Err() == nil {
		log.Error("reconcile service after cleanup", "uid", uid, "err", err)
	}
}

func (p *Processor) discardPendingReconcile(uid types.UID) {
	p.pendingMu.Lock()
	delete(p.pending, uid)
	p.pendingMu.Unlock()
}

func (p *Processor) pendingReconcileCount() int {
	p.pendingMu.Lock()
	defer p.pendingMu.Unlock()
	return len(p.pending)
}

func (p *Processor) registerCleanupGroup(ctx context.Context, wg ...*sync.WaitGroup) *cleanupGroup {
	group := &cleanupGroup{ctx: ctx, active: true}
	if len(wg) != 0 {
		group.watchWG = wg[0]
	}
	p.cleanupMu.Lock()
	if p.cleanup == nil {
		p.cleanup = make(map[<-chan struct{}]*cleanupGroup)
	}
	p.cleanup[ctx.Done()] = group
	p.cleanupMu.Unlock()
	return group
}

func (p *Processor) cleanupGroup(ctx context.Context) *cleanupGroup {
	p.cleanupMu.Lock()
	defer p.cleanupMu.Unlock()
	if p.cleanup == nil {
		p.cleanup = make(map[<-chan struct{}]*cleanupGroup)
	}
	group := p.cleanup[ctx.Done()]
	if group == nil {
		group = &cleanupGroup{ctx: ctx, active: true}
		p.cleanup[ctx.Done()] = group
	}
	return group
}

func (group *cleanupGroup) beginReplay() (*sync.WaitGroup, bool) {
	group.mu.Lock()
	defer group.mu.Unlock()
	if !group.active || group.ctx.Err() != nil || group.watchWG == nil {
		return nil, false
	}
	group.watchWG.Add(1)
	return group.watchWG, true
}

func (group *cleanupGroup) stopReplays() {
	group.mu.Lock()
	group.active = false
	group.watchWG = nil
	group.mu.Unlock()
}

func (p *Processor) queueMemberCleanup(c *serviceElection, member *serviceElectionMember) {
	group := p.cleanupGroup(member.key.svcCtx.Parent())
	group.wg.Add(1)
	go func() {
		defer group.wg.Done()
		if err := c.retryMemberCleanup(group.ctx, member); err != nil {
			group.mu.Lock()
			group.err = errors.Join(group.err, err)
			group.mu.Unlock()
		}
	}()
}

func (p *Processor) finishCleanupGroup(group *cleanupGroup) error {
	group.wg.Wait()
	p.cleanupMu.Lock()
	if p.cleanup[group.ctx.Done()] == group {
		delete(p.cleanup, group.ctx.Done())
	}
	p.cleanupMu.Unlock()
	group.mu.Lock()
	defer group.mu.Unlock()
	return group.err
}
