package servicecontext

import (
	"context"
	"sync"
)

type Context struct {
	Ctx                context.Context
	Cancel             context.CancelFunc
	IsWatched          bool
	ConfiguredNetworks sync.Map
	EndpointsReady     chan any
	epReady            sync.Once
	signalled          bool
	LeaderCancel       context.CancelFunc
}

func New(ctx context.Context) *Context {
	svcCtx, svcCancel := context.WithCancel(ctx)
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
		ctx.signalled = true
	})
}

func (ctx *Context) ResetReadiness() {
	if ctx.signalled {
		ctx.EndpointsReady = make(chan any)
		ctx.epReady = sync.Once{}
		ctx.signalled = false
	}
}
