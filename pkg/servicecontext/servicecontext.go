package servicecontext

import (
	"context"
	"sync"
)

type Context struct {
	Ctx                 context.Context
	Cancel              context.CancelFunc
	ConfiguredNetworks  sync.Map
	stateMutex          sync.Mutex
	ready               bool
	isWatched           bool
	watchingStopped     chan struct{}
	endpointsReady      chan any
	endpointsLost       chan any
	readinessGeneration uint64
	readinessOperations int
	readinessChanged    *sync.Cond
}

func New(ctx context.Context) *Context {
	// context and cancel stored for a future use, gosec linter disabled
	svcCtx, svcCancel := context.WithCancel(ctx) //nolint:gosec
	serviceContext := &Context{
		Ctx:                 svcCtx,
		Cancel:              svcCancel,
		endpointsReady:      make(chan any),
		endpointsLost:       make(chan any),
		readinessGeneration: 1,
	}
	serviceContext.readinessChanged = sync.NewCond(&serviceContext.stateMutex)
	return serviceContext
}

// ReadinessState returns one readiness lifecycle. The ready channel is closed
// when endpoints become usable and the lost channel is closed when that exact
// generation is reset.
func (ctx *Context) ReadinessState() (uint64, <-chan any, <-chan any, bool) {
	ctx.stateMutex.Lock()
	defer ctx.stateMutex.Unlock()
	return ctx.readinessGeneration, ctx.endpointsReady, ctx.endpointsLost, ctx.ready
}

func (ctx *Context) IsReady() bool {
	ctx.stateMutex.Lock()
	defer ctx.stateMutex.Unlock()
	return ctx.ready
}

// ReadinessGenerationCurrent reports whether generation is the current usable
// endpoint generation for this Service context.
func (ctx *Context) ReadinessGenerationCurrent(generation uint64) bool {
	ctx.stateMutex.Lock()
	defer ctx.stateMutex.Unlock()
	return ctx.ready && ctx.readinessGeneration == generation
}

func (ctx *Context) ResetReadinessGeneration(generation uint64) bool {
	ctx.stateMutex.Lock()
	defer ctx.stateMutex.Unlock()
	if !ctx.ready || ctx.readinessGeneration != generation {
		return false
	}
	close(ctx.endpointsLost)
	ctx.readinessGeneration++
	ctx.endpointsReady = make(chan any)
	ctx.endpointsLost = make(chan any)
	ctx.ready = false
	for ctx.readinessOperations > 0 {
		ctx.readinessChanged.Wait()
	}
	return true
}

func (ctx *Context) SignalReadiness() {
	ctx.stateMutex.Lock()
	defer ctx.stateMutex.Unlock()
	if ctx.ready {
		return
	}
	close(ctx.endpointsReady)
	ctx.ready = true
}

// WaitForReadiness reserves the first ready generation that remains current.
// The caller must release the returned reservation after its datapath operation.
func (ctx *Context) WaitForReadiness() (func(), bool) {
	for {
		generation, ready, _, isReady := ctx.ReadinessState()
		if !isReady {
			select {
			case <-ctx.Ctx.Done():
				return nil, false
			case <-ready:
			}
		}
		if release, acquired := ctx.AcquireReadinessGeneration(generation); acquired {
			return release, true
		}
	}
}

// AcquireReadinessGeneration reserves a ready generation while a caller starts
// or stops datapath work. ResetReadinessGeneration waits for the returned
// release function, preventing that work from outliving its endpoint state.
func (ctx *Context) AcquireReadinessGeneration(generation uint64) (func(), bool) {
	if !ctx.acquireReadinessGeneration(generation) {
		return nil, false
	}

	var releaseOnce sync.Once
	return func() {
		releaseOnce.Do(ctx.releaseReadinessGeneration)
	}, true
}

func (ctx *Context) acquireReadinessGeneration(generation uint64) bool {
	ctx.stateMutex.Lock()
	defer ctx.stateMutex.Unlock()
	if ctx.Ctx.Err() != nil || !ctx.ready || ctx.readinessGeneration != generation {
		return false
	}
	ctx.readinessOperations++
	return true
}

func (ctx *Context) releaseReadinessGeneration() {
	ctx.stateMutex.Lock()
	defer ctx.stateMutex.Unlock()
	ctx.readinessOperations--
	if ctx.readinessOperations == 0 {
		ctx.readinessChanged.Broadcast()
	}
}

func (ctx *Context) StartWatching() bool {
	ctx.stateMutex.Lock()
	defer ctx.stateMutex.Unlock()
	if ctx.Ctx.Err() != nil || ctx.isWatched {
		return false
	}
	ctx.isWatched = true
	ctx.watchingStopped = make(chan struct{})
	return true
}

func (ctx *Context) StopWatching() {
	ctx.stateMutex.Lock()
	defer ctx.stateMutex.Unlock()
	if !ctx.isWatched {
		return
	}
	ctx.isWatched = false
	close(ctx.watchingStopped)
}

func (ctx *Context) WaitForWatchingStopped(waitCtx context.Context) error {
	stopped := ctx.watchingStoppedSignal()
	if stopped == nil {
		return nil
	}

	select {
	case <-waitCtx.Done():
		return waitCtx.Err()
	case <-stopped:
		return nil
	}
}

func (ctx *Context) watchingStoppedSignal() <-chan struct{} {
	ctx.stateMutex.Lock()
	defer ctx.stateMutex.Unlock()
	if !ctx.isWatched {
		return nil
	}
	return ctx.watchingStopped
}
