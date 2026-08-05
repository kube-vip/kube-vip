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
	Signalled          atomic.Bool
	LeaderCancel       context.CancelFunc
	// LeaderElectionStarted tracks whether the per-service leader-election
	// restart-loop goroutine has already been spawned for this context's
	// lifetime. That goroutine runs until Ctx is cancelled, re-attempting
	// election forever on its own, so it must be started at most once per
	// service - not once per AddOrModify call with non-zero endpoints.
	LeaderElectionStarted bool
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
