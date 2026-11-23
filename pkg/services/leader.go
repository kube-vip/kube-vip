package services

import (
	"context"
	"fmt"
	"sync"

	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/metrics"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	v1 "k8s.io/api/core/v1"
)

// The StartServicesWatchForLeaderElection function will start a services watcher, the
func (p *Processor) StartServicesWatchForLeaderElection(ctx context.Context, forcedOnly bool) error {
	err := p.ServicesWatcher(ctx, p.StartServicesLeaderElection, forcedOnly)
	if err != nil {
		return err
	}

	if p.config.EnableRoutingTable {
		p.routeMgr.Clear()
	}

	log.Info("Shutting down kube-Vip")

	return nil
}

// StartServicesLeaderElection watches one Service's endpoint readiness while
// its per-lease coordinator owns campaign lifetime.
func (p *Processor) StartServicesLeaderElection(svcCtx *servicecontext.Context, service *v1.Service,
	_ *sync.WaitGroup) error {
	if service == nil {
		return fmt.Errorf("no service for leader election")
	}
	if svcCtx == nil {
		return fmt.Errorf("no context for service %q with UID %q", service.Name, service.UID)
	}
	currentContext, err := p.getServiceContext(service.UID)
	if err != nil {
		return fmt.Errorf("get current service context: %w", err)
	}
	if currentContext != svcCtx {
		return fmt.Errorf("service context is no longer current for service %q with UID %q", service.Name, service.UID)
	}
	if err := svcCtx.Ctx.Err(); err != nil {
		return fmt.Errorf("service context cancelled before election start: %w", err)
	}
	if _, loaded := p.electionLoops.LoadOrStore(svcCtx, struct{}{}); loaded {
		return nil
	}
	defer p.electionLoops.Delete(svcCtx)
	loops := metrics.ServiceElectionLoops.WithLabelValues(service.Namespace, service.Name)
	loops.Inc()
	defer loops.Dec()
	p.electionCoordinators.Watch(svcCtx, service)
	return nil
}
