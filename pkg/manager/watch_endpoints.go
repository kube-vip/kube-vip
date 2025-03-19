package manager

import (
	"context"
	"errors"
	"fmt"
	"syscall"

	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/endpoints"
	"github.com/kube-vip/kube-vip/pkg/endpoints/providers"
	"github.com/kube-vip/kube-vip/pkg/services"
	svcs "github.com/kube-vip/kube-vip/pkg/services"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/watch"
)

func (sm *Manager) watchEndpoint(svcCtx *services.Context, id string, service *v1.Service, provider providers.Provider) error {
	log.Info("watching", "provider", provider.GetLabel(), "service_name", service.Name, "namespace", service.Namespace)
	// Use a restartable watcher, as this should help in the event of etcd or timeout issues

	leaderCtx, cancel := context.WithCancel(svcCtx.Ctx)
	defer cancel()

	var leaderElectionActive bool

	rw, err := provider.CreateRetryWatcher(leaderCtx, sm.rwClientSet, service)
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
			// Cancel the context, which will in turn cancel the leadership
			cancel()
			return
		case <-sm.shutdownChan:
			log.Debug("shutdown called", "provider", provider.GetLabel())
			// Stop the retry watcher
			rw.Stop()
			// Cancel the context, which will in turn cancel the leadership
			cancel()
			return
		case <-exitFunction:
			log.Debug("function ending", "provider", provider.GetLabel())
			// Stop the retry watcher
			rw.Stop()
			// Cancel the context, which will in turn cancel the leadership
			cancel()
			return
		}
	}()

	ch := rw.ResultChan()

	epProcessor := endpoints.NewEndpointProcessor(sm.config, provider, sm.bgpServer, &sm.serviceInstances)

	var lastKnownGoodEndpoint string
	for event := range ch {
		// We need to inspect the event and get ResourceVersion out of it
		switch event.Type {

		case watch.Added, watch.Modified:
			restart, err := epProcessor.AddOrModify(svcCtx, event, &lastKnownGoodEndpoint, service, id, &leaderElectionActive, sm.StartServicesLeaderElection, &leaderCtx, &cancel)
			if restart {
				continue
			} else if err != nil {
				return fmt.Errorf("[%s] error while processing add/modify event: %w", provider.GetLabel(), err)
			}

		case watch.Deleted:
			if err := epProcessor.Delete(service, id); err != nil {
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

func (sm *Manager) clearRoutes(service *v1.Service) []error {
	errs := []error{}
	if instance := sm.findServiceInstance(service); instance != nil {
		for _, cluster := range instance.Clusters {
			for i := range cluster.Network {
				route := cluster.Network[i].PrepareRoute()
				// check if route we are about to delete is not referenced by more than one service
				if sm.countRouteReferences(route) <= 1 {
					err := cluster.Network[i].DeleteRoute()
					if err != nil && !errors.Is(err, syscall.ESRCH) {
						log.Error("failed to delete route", "ip", cluster.Network[i].IP(), "err", err)
						errs = append(errs, err)
					}
					log.Debug("deleted route", "ip",
						cluster.Network[i].IP(), "service name", service.Name, "namespace", service.Namespace, "interface", cluster.Network[i].Interface(), "tableID", sm.config.RoutingTableID)
				}
			}
		}
	}
	return errs
}

func (sm *Manager) clearBGPHosts(service *v1.Service) {
	if instance := sm.findServiceInstance(service); instance != nil {
		sm.clearBGPHostsByInstance(instance)
	}
}

func (sm *Manager) clearBGPHostsByInstance(instance *svcs.Instance) {
	for _, cluster := range instance.Clusters {
		for i := range cluster.Network {
			network := cluster.Network[i]
			err := sm.bgpServer.DelHost(network.CIDR())
			if err != nil {
				log.Error("[endpoint] error deleting BGP host", "err", err)
			} else {
				log.Debug("[endpoint] deleted BGP host", "ip",
					network.CIDR(), "service name", instance.ServiceSnapshot.Name, "namespace", instance.ServiceSnapshot.Namespace)
			}
		}
	}
}
