package servicecontext

import (
	"context"
	"sync"
	"sync/atomic"
)

type Context struct {
	Ctx                context.Context
	Cancel             context.CancelFunc
	ConfiguredNetworks sync.Map
	leaderElection     sync.Once
	Signalled          atomic.Bool
	stateMutex         sync.Mutex
	isWatched          bool
	endpointsReady     chan any
	leaderCancel       context.CancelFunc
}

func New(ctx context.Context) *Context {
	// context and cancel stored for a future use, gosec linter disabled
	svcCtx, svcCancel := context.WithCancel(ctx) //nolint:gosec
	return &Context{
		Ctx:            svcCtx,
		Cancel:         svcCancel,
		endpointsReady: make(chan any),
	}
}

func (ctx *Context) HasConfiguredNetworks() bool {
	cnt := 0
	ctx.ConfiguredNetworks.Range(func(_ any, _ any) bool {
		cnt++
		return cnt < 1
	})
	return cnt > 0
}

func (ctx *Context) IsNetworkConfigured(ip string) bool {
	_, exists := ctx.ConfiguredNetworks.Load(ip)
	return exists
}

// StartLeaderElectionOnce runs f only on its first call for this service context.
// The leader-election loop restarts itself internally until the context is
// cancelled, so it must be started exactly once per service lifetime. Unlike
// readiness, this is never reset: the loop outlives individual endpoint events.
func (ctx *Context) StartLeaderElectionOnce(f func()) {
	ctx.leaderElection.Do(f)
}

func (ctx *Context) Readiness() <-chan any {
	ctx.stateMutex.Lock()
	defer ctx.stateMutex.Unlock()
	return ctx.endpointsReady
}

func (ctx *Context) SignalReadiness() {
	ctx.stateMutex.Lock()
	defer ctx.stateMutex.Unlock()
	if !ctx.Signalled.Load() {
		close(ctx.endpointsReady)
		ctx.Signalled.Store(true)
	}
}

func (ctx *Context) ResetReadiness() {
	ctx.stateMutex.Lock()
	defer ctx.stateMutex.Unlock()
	if ctx.Signalled.Load() {
		ctx.endpointsReady = make(chan any)
		ctx.Signalled.Store(false)
	}
}

func (ctx *Context) StartWatching() bool {
	ctx.stateMutex.Lock()
	defer ctx.stateMutex.Unlock()
	if ctx.isWatched {
		return false
	}
	ctx.isWatched = true
	return true
}

func (ctx *Context) StopWatching() {
	ctx.stateMutex.Lock()
	defer ctx.stateMutex.Unlock()
	ctx.isWatched = false
}

func (ctx *Context) SetLeaderCancel(cancel context.CancelFunc) {
	ctx.stateMutex.Lock()
	defer ctx.stateMutex.Unlock()
	ctx.leaderCancel = cancel
}

func (ctx *Context) CancelLeader() {
	ctx.stateMutex.Lock()
	cancel := ctx.leaderCancel
	ctx.stateMutex.Unlock()
	if cancel != nil {
		cancel()
	}
}
