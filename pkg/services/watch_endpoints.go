package services

import (
	"fmt"

	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/endpoints"
	"github.com/kube-vip/kube-vip/pkg/endpoints/providers"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/watch"
)

func (p *Processor) watchEndpoint(svcCtx *servicecontext.Context, id string, service *v1.Service, provider providers.Provider) error {
	log.Info("watching", "provider", provider.GetLabel(), "service_name", service.Name, "namespace", service.Namespace)
	// Use a restartable watcher, as this should help in the event of etcd or timeout issues

	rw, err := provider.CreateRetryWatcher(svcCtx.Ctx, p.rwClientSet, service)
	if err != nil {
		return fmt.Errorf("[%s] error watching endpoints: %w", provider.GetLabel(), err)
	}

	exitFunction := make(chan struct{})
	go func() {
		select {
		case <-svcCtx.Ctx.Done():
			log.Debug("context cancelled", "provider", provider.GetLabel())
			// Stop the retry watcher
			rw.Stop()
			return
		case <-p.shutdownChan:
			log.Debug("shutdown called", "provider", provider.GetLabel())
			// Stop the retry watcher
			rw.Stop()
			// Cancel the context, which will in turn cancel the leadership
			svcCtx.Cancel()
			return
		case <-exitFunction:
			log.Debug("function ending", "provider", provider.GetLabel())
			// Stop the retry watcher
			rw.Stop()
			// Cancel the context, which will in turn cancel the leadership
			svcCtx.Cancel()
			return
		}
	}()

	ch := rw.ResultChan()

	epProcessor := endpoints.NewEndpointProcessor(p.config, provider, p.bgpServer, &p.ServiceInstances, p.leaseMgr)

	var lastKnownGoodEndpoint string
	for event := range ch {
		// We need to inspect the event and get ResourceVersion out of it
		switch event.Type {

		case watch.Added, watch.Modified:
			restart, err := epProcessor.AddOrModify(svcCtx, event, &lastKnownGoodEndpoint, service, id, p.StartServicesLeaderElection)
			if restart {
				continue
			} else if err != nil {
				return fmt.Errorf("[%s] error while processing add/modify event: %w", provider.GetLabel(), err)
			}

		case watch.Deleted:
			if err := epProcessor.Delete(svcCtx.Ctx, service, id); err != nil {
				return fmt.Errorf("[%s] error while processing delete event: %w", provider.GetLabel(), err)
			}

			// Close the goroutine that will end the retry watcher, then exit the endpoint watcher function
			close(exitFunction)
			log.Info("stopping watching", "provider", provider.GetLabel(), "service name", service.Name, "namespace", service.Namespace)

			return nil
		case watch.Error:
			errObject := apierrors.FromObject(event.Object)
			statusErr, _ := errObject.(*apierrors.StatusError)
			log.Error("watch error", "provider", provider.GetLabel(), "err", statusErr)
		}
	}
	close(exitFunction)
	log.Info("stopping watching", "provider", provider.GetLabel(), "service name", service.Name, "namespace", service.Namespace)
	return nil //nolint:govet
}
