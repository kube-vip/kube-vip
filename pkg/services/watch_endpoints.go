package services

import (
	"context"
	"fmt"
	"sync"

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

	wg := sync.WaitGroup{}

	defer func() {
		rw.Stop()
		wg.Wait()
	}()

	wg.Go(func() {
		<-svcCtx.Ctx.Done()
		log.Debug("context cancelled", "provider", provider.GetLabel())
	})

	ch := rw.ResultChan()

	epProcessor := endpoints.NewEndpointProcessor(p.config, provider, p.bgpServer, &p.ServiceInstances, p.leaseMgr, p.TunnelMgr)

	var lastKnownGoodEndpoint string
	for event := range ch {
		// We need to inspect the event and get ResourceVersion out of it
		switch event.Type {

		case watch.Added, watch.Modified:
			updateAnnotationFunc := func(ctx context.Context, endpoint, endpointIPv6 string, svc *v1.Service) error {
				return provider.UpdateServiceAnnotation(ctx, endpoint, endpointIPv6, svc, p.clientSet)
			}
			restart, err := epProcessor.AddOrModify(svcCtx, event, &lastKnownGoodEndpoint, service, id, p.StartServicesLeaderElection, &wg, updateAnnotationFunc, p.updateEgressConfiguration)
			if restart {
				continue
			} else if err != nil {
				return fmt.Errorf("[%s] error while processing add/modify event: %w", provider.GetLabel(), err)
			}

		case watch.Deleted:
			if err := epProcessor.Delete(svcCtx.Ctx, service, id); err != nil {
				return fmt.Errorf("[%s] error while processing delete event: %w", provider.GetLabel(), err)
			}

			log.Info("stopping watching", "provider", provider.GetLabel(), "service name", service.Name, "namespace", service.Namespace)
			return nil
		case watch.Error:
			errObject := apierrors.FromObject(event.Object)
			statusErr, _ := errObject.(*apierrors.StatusError)
			log.Error("watch error", "provider", provider.GetLabel(), "err", statusErr)
		}
	}
	log.Info("stopping watching", "provider", provider.GetLabel(), "service name", service.Name, "namespace", service.Namespace)
	return nil //nolint:govet
}
