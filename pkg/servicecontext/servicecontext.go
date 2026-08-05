package servicecontext

import (
	"context"
	"sync"
	"sync/atomic"
)

type Context struct {
	Ctx                context.Context
	Cancel             context.CancelFunc
	IsWatched          bool
	ConfiguredNetworks sync.Map
	EndpointsReady     chan any
	epReady            sync.Once
	leaderElection     sync.Once
	Signalled          atomic.Bool
	LeaderCancel       context.CancelFunc
}

func New(ctx context.Context) *Context {
	// context and cancel stored for a future use, gosec linter disabled
	svcCtx, svcCancel := context.WithCancel(ctx) //nolint:gosec
	return &Context{
		Ctx:            svcCtx,
		Cancel:         svcCancel,
		EndpointsReady: make(chan any),
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

func (ctx *Context) SignalReadiness() {
	ctx.epReady.Do(func() {
		close(ctx.EndpointsReady)
		ctx.Signalled.Store(true)
	})
}

func (ctx *Context) ResetReadiness() {
	if ctx.Signalled.Load() {
		ctx.EndpointsReady = make(chan any)
		ctx.epReady = sync.Once{}
		ctx.Signalled.Store(false)
	}
}
