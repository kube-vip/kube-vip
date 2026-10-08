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
	endpointsReady      chan struct{}
	endpointsLost       chan struct{}
	readinessGeneration uint64
	readinessOperations int
	readinessChanged    *sync.Cond
}

// ReadinessGeneration identifies one endpoint-readiness lifecycle. Ready is
// closed when the generation becomes usable; Lost is closed when it is
// invalidated. A generation is only meaningful for the Context that created it.
type ReadinessGeneration struct {
	id    uint64
	ready <-chan struct{}
	lost  <-chan struct{}
}

func (g ReadinessGeneration) ID() uint64 { return g.id }

func (g ReadinessGeneration) Ready() <-chan struct{} { return g.ready }

func (g ReadinessGeneration) Lost() <-chan struct{} { return g.lost }

// ReadinessReservation prevents invalidation from completing while datapath
// work belonging to the generation is still running.
type ReadinessReservation struct {
	context *Context
	once    sync.Once
}

func (r *ReadinessReservation) Release() {
	if r == nil || r.context == nil {
		return
	}
	r.once.Do(r.context.releaseReadinessGeneration)
}

func New(ctx context.Context) *Context {
	// context and cancel stored for a future use, gosec linter disabled
	svcCtx, svcCancel := context.WithCancel(ctx) //nolint:gosec
	serviceContext := &Context{
		Ctx:                 svcCtx,
		Cancel:              svcCancel,
		endpointsReady:      make(chan struct{}),
		endpointsLost:       make(chan struct{}),
		readinessGeneration: 1,
	}
	serviceContext.readinessChanged = sync.NewCond(&serviceContext.stateMutex)
	return serviceContext
}

// CurrentReadiness returns one readiness lifecycle. The ready channel is closed
// when endpoints become usable and the lost channel is closed when that exact
// generation is reset.
func (ctx *Context) CurrentReadiness() ReadinessGeneration {
	ctx.stateMutex.Lock()
	defer ctx.stateMutex.Unlock()
	return ReadinessGeneration{id: ctx.readinessGeneration, ready: ctx.endpointsReady, lost: ctx.endpointsLost}
}

func (ctx *Context) IsReady() bool {
	ctx.stateMutex.Lock()
	defer ctx.stateMutex.Unlock()
	return ctx.ready
}

// ReadinessGenerationCurrent reports whether generation is the current usable
// endpoint generation for this Service context.
func (ctx *Context) ReadinessGenerationCurrent(generation ReadinessGeneration) bool {
	ctx.stateMutex.Lock()
	defer ctx.stateMutex.Unlock()
	return ctx.ready && ctx.readinessGeneration == generation.id
}

func (ctx *Context) ResetReadinessGeneration(generation ReadinessGeneration) bool {
	ctx.stateMutex.Lock()
	defer ctx.stateMutex.Unlock()
	if !ctx.ready || ctx.readinessGeneration != generation.id {
		return false
	}
	close(ctx.endpointsLost)
	ctx.readinessGeneration++
	ctx.endpointsReady = make(chan struct{})
	ctx.endpointsLost = make(chan struct{})
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
func (ctx *Context) WaitForReadiness() (*ReadinessReservation, bool) {
	for {
		if ctx.Ctx.Err() != nil {
			return nil, false
		}
		generation := ctx.CurrentReadiness()
		select {
		case <-ctx.Ctx.Done():
			return nil, false
		case <-generation.Ready():
		}
		if release, acquired := ctx.AcquireReadinessGeneration(generation); acquired {
			return release, true
		}
	}
}

// AcquireReadinessGeneration reserves a ready generation while a caller starts
// or stops datapath work. ResetReadinessGeneration waits for the returned
// release function, preventing that work from outliving its endpoint state.
func (ctx *Context) AcquireReadinessGeneration(generation ReadinessGeneration) (*ReadinessReservation, bool) {
	if !ctx.acquireReadinessGeneration(generation) {
		return nil, false
	}

	return &ReadinessReservation{context: ctx}, true
}

func (ctx *Context) acquireReadinessGeneration(generation ReadinessGeneration) bool {
	ctx.stateMutex.Lock()
	defer ctx.stateMutex.Unlock()
	if ctx.Ctx.Err() != nil || !ctx.ready || ctx.readinessGeneration != generation.id {
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
