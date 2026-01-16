package servicecontext

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/kube-vip/kube-vip/pkg/lease"
)

type Context struct {
	Ctx                context.Context
	Cancel             context.CancelFunc
	IsActive           bool
	IsWatched          bool
	ConfiguredNetworks sync.Map
	Lease              *lease.Lease
	HasEndpoints       atomic.Bool
}

func New(ctx context.Context) *Context {
	svcCtx, svcCancel := context.WithCancel(ctx)
	return &Context{
		Ctx:    svcCtx,
		Cancel: svcCancel,
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
