package services

import (
	"context"
	"fmt"
	"sync"

	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	v1 "k8s.io/api/core/v1"
)

// The StartServicesWatchForLeaderElection function will start a services watcher, the
func (p *Processor) StartServicesWatchForLeaderElection(ctx context.Context, forcedOnly bool) error {
	err := p.ServicesWatcher(ctx, NewCallback(p.runServicesLeaderElectionLoop, true), forcedOnly)
	if err != nil {
		return err
	}

	if p.config.EnableRoutingTable {
		p.routeMgr.Clear()
	}

	log.Info("Shutting down kube-Vip")

	return nil
}

// The startServicesWatchForLeaderElection function will start a services watcher, the
func (p *Processor) StartServicesLeaderElection(svcCtx *servicecontext.Context, service *v1.Service, _ *sync.WaitGroup, _ bool) error {
	return p.startServicesLeaderElectionOnce(svcCtx, service)
}

func (p *Processor) runServicesLeaderElectionLoop(svcCtx *servicecontext.Context, service *v1.Service, _ *sync.WaitGroup, _ bool) error {
	if err := p.validateServiceElectionContext(svcCtx, service); err != nil {
		return err
	}
	if !svcCtx.StartLeaderLoop() {
		return nil
	}
	defer svcCtx.FinishLeaderLoop()
	return p.startLeaderElection(svcCtx.Parent(), svcCtx, service, nil)
}

func (p *Processor) startServicesLeaderElectionOnce(svcCtx *servicecontext.Context, service *v1.Service) error {
	if err := p.validateServiceElectionContext(svcCtx, service); err != nil {
		return err
	}
	return p.runServiceElectionMember(svcCtx, service)
}

func (p *Processor) validateServiceElectionContext(svcCtx *servicecontext.Context, service *v1.Service) error {
	if service == nil {
		return fmt.Errorf("no service for leader election")
	}
	if svcCtx == nil {
		return fmt.Errorf("no service context for service %q with UID %q", service.Name, service.UID)
	}
	current, err := p.getServiceContext(service.UID)
	if err != nil {
		return fmt.Errorf("get current service context: %w", err)
	}
	if current != svcCtx {
		return fmt.Errorf("service context is no longer current for service %q with UID %q", service.Name, service.UID)
	}
	if err := svcCtx.Ctx.Err(); err != nil {
		return fmt.Errorf("service context cancelled before election start: %w", err)
	}
	return nil
}

// releaseServiceLease releases a lease membership only while svcCtx remains
// the current context for the Service. A replacement can share the same lease,
// so an old cleanup goroutine must not remove the replacement's membership.
func (p *Processor) releaseServiceLease(svcCtx *servicecontext.Context, service *v1.Service, id lease.ID, claimID string, svcLease *lease.Lease) {
	unlockService := p.lockService(service.UID)
	defer unlockService()

	currentCtx, err := p.getServiceContext(service.UID)
	if err != nil || currentCtx != svcCtx {
		return
	}
	p.leaseMgr.Delete(id, claimID, svcLease)
}

func (p *Processor) onStartedLeading(ctx context.Context, svcCtx *servicecontext.Context, service *v1.Service, wg *sync.WaitGroup, expected ...*serviceExpectation) error {
	var expectation *serviceExpectation
	if len(expected) > 0 {
		expectation = expected[0]
	}
	err := p.syncServices(ctx, svcCtx, service, wg, true, expectation)
	if err != nil {
		log.Error("service sync", "uid", service.UID, "err", err)
		return err
	}
	return nil
}

func (p *Processor) onStoppedLeading(svcCtx *servicecontext.Context, svcLease *lease.Lease, service *v1.Service, expected ...*instance.Instance) error {
	return p.onStoppedLeadingExcluding(svcCtx, svcLease, service, firstInstance(expected), nil, true)
}

func firstInstance(instances []*instance.Instance) *instance.Instance {
	if len(instances) == 0 {
		return nil
	}
	return instances[0]
}

func (p *Processor) onStoppedLeadingExcluding(svcCtx *servicecontext.Context, svcLease *lease.Lease, service *v1.Service,
	expected *instance.Instance, stopping map[*instance.Instance]struct{}, leadershipLost bool, forceDelete ...map[string]struct{}) error {
	currentSvcCtx, err := p.getServiceContext(service.UID)
	if err != nil {
		return err
	}
	if currentSvcCtx != nil && currentSvcCtx != svcCtx {
		log.Debug("skipping cleanup from superseded service context", "service", service.Name, "uid", service.UID)
		return nil
	}

	log.Debug("deleting service due to lost leadership", "uid", service.UID)
	if expected != nil {
		return p.deleteServiceInstanceWithMode(context.WithoutCancel(svcLease.Ctx), expected, stopping, leadershipLost, forceDelete...)
	}
	err = p.deleteService(context.WithoutCancel(svcLease.Ctx), service.UID, svcCtx)
	if err != nil {
		log.Error("service deletion", "err", err)
		return err
	}
	return nil
}
