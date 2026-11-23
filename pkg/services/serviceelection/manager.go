package serviceelection

import (
	"fmt"
	"sync"
	"time"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	v1 "k8s.io/api/core/v1"
)

// Manager coordinates Service readiness with per-lease election state.
type Manager struct {
	config         *kubevip.Config
	state          ServiceState
	coordinatorMgr *coordinatorManager
	campaignWG     sync.WaitGroup
}

// NewManager creates a Service election manager.
func NewManager(dependencies *Dependencies) (*Manager, error) {
	if dependencies == nil {
		return nil, fmt.Errorf("create service election manager: dependencies are required")
	}
	if err := dependencies.validate(); err != nil {
		return nil, fmt.Errorf("create service election manager: %w", err)
	}

	return &Manager{config: dependencies.Config, state: dependencies.State, coordinatorMgr: newCoordinatorManager(*dependencies)}, nil
}

// join registers the current ready generation of a Service. A caller racing
// coordinator retirement retries against its replacement.
func (m *Manager) join(svcCtx *servicecontext.Context, service *v1.Service,
	readinessGeneration servicecontext.ReadinessGeneration) (*member, bool) {
	if svcCtx == nil || service == nil {
		return nil, false
	}

	namespace, name := lease.ServiceName(service)
	id := lease.NewID(m.config.LeaderElectionType, namespace, name)
	for {
		if !m.state.IsCurrent(service, svcCtx, readinessGeneration) {
			return nil, false
		}
		coordinator := m.coordinatorMgr.getOrCreate(id)
		member, joined := coordinator.join(svcCtx, service, readinessGeneration)
		if joined {
			if m.state.IsCurrent(member.service, member.serviceContext, member.readinessGeneration) {
				return member, true
			}
			coordinator.withdrawMember(member)
			return nil, false
		}
		if retiredDone, retired := coordinator.retirement(); retired {
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

// DetachForContext withdraws only the member belonging to svcCtx. The caller
// owns datapath cleanup, so this method deliberately does not deactivate it.
func (m *Manager) DetachForContext(svcCtx *servicecontext.Context, service *v1.Service) {
	if svcCtx == nil || service == nil {
		return
	}
	namespace, name := lease.ServiceName(service)
	id := lease.NewID(m.config.LeaderElectionType, namespace, name)
	coordinator := m.coordinatorMgr.current(id)
	if coordinator == nil {
		return
	}
	member := coordinator.currentMember(service.UID)
	if member != nil && member.serviceContext == svcCtx {
		// The caller already owns Service cleanup. Withdrawing here must not
		// reacquire the Service lock through datapath cleanup.
		coordinator.withdrawMember(member)
	}
}

// Watch follows readiness generations for one Service until its context ends.
func (m *Manager) Watch(svcCtx *servicecontext.Context, service *v1.Service) {
	for {
		if svcCtx.Ctx.Err() != nil {
			return
		}
		generation := svcCtx.CurrentReadiness()
		select {
		case <-svcCtx.Ctx.Done():
			return
		case <-generation.Ready():
		}
		if !svcCtx.ReadinessGenerationCurrent(generation) {
			continue
		}

		member, joined := m.join(svcCtx, service, generation)
		if !joined {
			if !m.state.IsCurrent(service, svcCtx, generation) {
				if svcCtx.Ctx.Err() != nil {
					return
				}
				if !svcCtx.ReadinessGenerationCurrent(generation) {
					continue
				}
				return
			}
			select {
			case <-svcCtx.Ctx.Done():
				return
			case <-time.After(restartBaseDelay):
				continue
			}
		}
		member.coordinator.admitToCampaign(&m.campaignWG, member)

		select {
		case <-svcCtx.Ctx.Done():
			member.coordinator.closeMember(member)
			return
		case <-generation.Lost():
			member.coordinator.closeMember(member)
		}
	}
}

// Wait blocks until all election campaigns owned by the Manager have stopped.
// Callers must stop every source that can start a new campaign before calling
// Wait.
func (m *Manager) Wait() {
	m.campaignWG.Wait()
}
