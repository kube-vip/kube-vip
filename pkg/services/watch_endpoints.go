package services

import (
	"context"
	"fmt"
	"sync"

	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/debouncer"
	"github.com/kube-vip/kube-vip/pkg/endpoints"
	"github.com/kube-vip/kube-vip/pkg/endpoints/providers"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	"github.com/kube-vip/kube-vip/pkg/utils"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/watch"
)

func (p *Processor) watchEndpoint(svcCtx *servicecontext.Context, id string, service *v1.Service,
	provider providers.Provider, cancelWatcher context.CancelCauseFunc) error {
	log.Info("watching", "provider", provider.GetLabel(), "service_name", service.Name, "namespace", service.Namespace)
	// Use a restartable watcher, as this should help in the event of etcd or timeout issues

	rw, err := provider.CreateRetryWatcher(svcCtx.Ctx, p.rwClientSet, service)
	if err != nil {
		if svcCtx.Ctx.Err() != nil {
			return nil
		}
		return utils.WrapPanicError(err, "[%s] error watching endpoints", provider.GetLabel())
	}

	d, err := debouncer.New(rw.ResultChan(), p.config.DebounceTime)
	if err != nil {
		rw.Stop()
		return utils.WrapPanicError(err, "failed to create debouncer for endpoints event")
	}

	var wg sync.WaitGroup
	stopChan := make(chan any)

	defer func() {
		close(stopChan)
		if d != nil {
			d.Stop()
		}
		wg.Wait()
	}()

	wg.Go(func() {
		if d != nil {
			if err := d.Start(svcCtx.Ctx); err != nil {
				log.Error("[endpoint watcher] debouncer, cancelling context", "error", err.Error())
				if svcCtx.Ctx.Err() == nil {
					cancelWatcher(utils.WrapPanicError(err, "[%s] endpoint debouncer failed", provider.GetLabel()))
				}
				svcCtx.Cancel()
			}
		}
		select {
		case <-svcCtx.Ctx.Done():
			log.Debug("[endpoint watcher] context cancelled", "provider", provider.GetLabel())
			rw.Stop()
		case <-stopChan:
			svcCtx.Cancel()
			log.Debug("[endpoint watcher] exiting endpoint watcher", "namespace", service.Namespace, "service", service.Name, "provider", provider.GetLabel())
		}
	})

	epProcessor := endpoints.NewEndpointProcessor(p.config, provider, p.bgpServer, &p.ServiceInstances, p.leaseMgr, p.TunnelMgr, p.routeMgr)

	ch := rw.ResultChan()
	if d != nil {
		ch = d.Output()
	}

	var lastKnownGoodEndpoint string
	for event := range ch {
		// We need to inspect the event and get ResourceVersion out of it
		switch event.Type {

		case watch.Added, watch.Modified, watch.Deleted:
			if event.Type == watch.Deleted {
				log.Info("[endpoint watcher] endpoint object deleted", "provider", provider.GetLabel(), "service name", service.Name, "namespace", service.Namespace)
			}

			restart, err := epProcessor.Reconcile(svcCtx, event, &lastKnownGoodEndpoint, service, id,
				p.StartServicesLeaderElection, &wg, p.clientSet, p.updateEgressConfiguration)
			if restart {
				continue
			} else if err != nil {
				return fmt.Errorf("[%s] error while processing %s event: %w", provider.GetLabel(), event.Type, err)
			}

		case watch.Error:
			if svcCtx.Ctx.Err() != nil {
				return nil
			}
			watchErr := utils.WatchError(event.Object)
			log.Error("watch error", "provider", provider.GetLabel(), "err", watchErr)
			return utils.WrapPanicError(watchErr, "[%s] endpoint watch failed", provider.GetLabel())
		}
	}
	if svcCtx.Ctx.Err() != nil {
		return nil
	}
	log.Info("[endpoint watcher] stopping watching", "provider", provider.GetLabel(), "service name", service.Name, "namespace", service.Namespace)
	return utils.NewPanicError("[%s] endpoint watch channel closed unexpectedly for service %s/%s", provider.GetLabel(), service.Namespace, service.Name)
}
