package manager

import (
	"context"
	"fmt"
	"strings"

	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"
	watchtools "k8s.io/client-go/tools/watch"
	"k8s.io/client-go/util/retry"
)

type epProvider interface {
	createRetryWatcher(context.Context, *Manager,
		*v1.Service) (*watchtools.RetryWatcher, error)
	getAllEndpoints() ([]string, error)
	getLocalEndpoints(string, *kubevip.Config) ([]string, error)
	getLabel() string
	updateServiceAnnotation(string, string, *v1.Service, *Manager) error
	loadObject(runtime.Object, context.CancelFunc) error
	getProtocol() string
}

type endpointsProvider struct {
	label     string
	endpoints *v1.Endpoints
}

func (ep *endpointsProvider) createRetryWatcher(ctx context.Context, sm *Manager,
	service *v1.Service) (*watchtools.RetryWatcher, error) {
	opts := metav1.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("metadata.name", service.Name).String(),
	}

	rw, err := watchtools.NewRetryWatcher("1", &cache.ListWatch{
		WatchFunc: func(_ metav1.ListOptions) (watch.Interface, error) {
			return sm.rwClientSet.CoreV1().Endpoints(service.Namespace).Watch(ctx, opts)
		},
	})

	if err != nil {
		return nil, fmt.Errorf("error creating endpoint watcher: %s", err.Error())
	}

	return rw, nil
}

func (ep *endpointsProvider) loadObject(endpoints runtime.Object, cancel context.CancelFunc) error {
	eps, ok := endpoints.(*v1.Endpoints)
	if !ok {
		cancel()
		return fmt.Errorf("[%s] unable to parse Kubernetes services from API watcher", ep.getLabel())
	}
	ep.endpoints = eps
	return nil
}

func (ep *endpointsProvider) getAllEndpoints() ([]string, error) {
	result := []string{}
	for subset := range ep.endpoints.Subsets {
		for address := range ep.endpoints.Subsets[subset].Addresses {
			addr := strings.Split(ep.endpoints.Subsets[subset].Addresses[address].IP, "/")
			result = append(result, addr[0])
		}
	}

	return result, nil
}

func (ep *endpointsProvider) getLocalEndpoints(id string, _ *kubevip.Config) ([]string, error) {
	var localEndpoints []string

	for _, subset := range ep.endpoints.Subsets {
		for _, address := range subset.Addresses {
			log.Debug("processing endpoint", "label", ep.label, "ip", address.IP)

			// 1. Compare the Nodename
			if address.NodeName != nil && id == *address.NodeName {
				log.Debug("found local endpoint", "label", ep.label, "ip", address.IP, "hostname", address.Hostname, "nodename", *address.NodeName)
				localEndpoints = append(localEndpoints, address.IP)
				continue
			}
			// 2. Compare the Hostname (only useful if address.NodeName is not available)
			if id == address.Hostname {
				log.Debug("found local endpoint", "label", ep.label, "ip", address.IP, "hostname", address.Hostname)
				localEndpoints = append(localEndpoints, address.IP)
				continue
			}
		}
	}
	return localEndpoints, nil
}

func (ep *endpointsProvider) updateServiceAnnotation(endpoint string, _ string, service *v1.Service, sm *Manager) error {
	retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Retrieve the latest version of Deployment before attempting update
		// RetryOnConflict uses exponential backoff to avoid exhausting the apiserver
		currentService, err := sm.clientSet.CoreV1().Services(service.Namespace).Get(context.TODO(), service.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}

		currentServiceCopy := currentService.DeepCopy()
		if currentServiceCopy.Annotations == nil {
			currentServiceCopy.Annotations = make(map[string]string)
		}

		currentServiceCopy.Annotations[activeEndpoint] = endpoint

		_, err = sm.clientSet.CoreV1().Services(currentService.Namespace).Update(context.TODO(), currentServiceCopy, metav1.UpdateOptions{})
		if err != nil {
			log.Error("error updating Service Spec", "label", ep.getLabel(), "name", currentServiceCopy.Name, "err", err)
			return err
		}
		return nil
	})

	if retryErr != nil {
		log.Error("failed to set Services", "label", ep.getLabel(), "err", retryErr)
		return retryErr
	}
	return nil
}

func (ep *endpointsProvider) getLabel() string {
	return ep.label
}

func (ep *endpointsProvider) getProtocol() string {
	return ""
}

func (sm *Manager) watchEndpoint(svcCtx *serviceContext, id string, service *v1.Service, provider epProvider) error {
	log.Info("watching", "provider", provider.getLabel(), "service_name", service.Name, "namespace", service.Namespace)
	// Use a restartable watcher, as this should help in the event of etcd or timeout issues

	leaderCtx, cancel := context.WithCancel(svcCtx.ctx)
	defer cancel()

	var leaderElectionActive bool

	rw, err := provider.createRetryWatcher(leaderCtx, sm, service)
	if err != nil {
		return fmt.Errorf("[%s] error watching endpoints: %w", provider.getLabel(), err)
	}

	exitFunction := make(chan struct{})
	go func() {
		select {
		case <-svcCtx.ctx.Done():
			log.Debug("context cancelled", "provider", provider.getLabel())
			// Stop the retry watcher
			rw.Stop()
			// Cancel the context, which will in turn cancel the leadership
			cancel()
			return
		case <-sm.shutdownChan:
			log.Debug("shutdown called", "provider", provider.getLabel())
			// Stop the retry watcher
			rw.Stop()
			// Cancel the context, which will in turn cancel the leadership
			cancel()
			return
		case <-exitFunction:
			log.Debug("function ending", "provider", provider.getLabel())
			// Stop the retry watcher
			rw.Stop()
			// Cancel the context, which will in turn cancel the leadership
			cancel()
			return
		}
	}()

	ch := rw.ResultChan()

	epProcessor := NewEndpointProcessor(sm, provider)

	var lastKnownGoodEndpoint string
	for event := range ch {
		// We need to inspect the event and get ResourceVersion out of it
		switch event.Type {

		case watch.Added, watch.Modified:
			restart, err := epProcessor.AddModify(svcCtx, event, &lastKnownGoodEndpoint, service, id, &leaderElectionActive, &leaderCtx, &cancel)
			if restart {
				continue
			} else if err != nil {
				return fmt.Errorf("[%s] error while processing add/modify event: %w", provider.getLabel(), err)
			}

		case watch.Deleted:
			if err := epProcessor.Delete(service, id); err != nil {
				return fmt.Errorf("[%s] error while processing delete event: %w", provider.getLabel(), err)
			}

			// Close the goroutine that will end the retry watcher, then exit the endpoint watcher function
			close(exitFunction)
			log.Info("stopping watching", "provider", provider.getLabel(), "service name", service.Name, "namespace", service.Namespace)

			return nil
		case watch.Error:
			errObject := apierrors.FromObject(event.Object)
			statusErr, _ := errObject.(*apierrors.StatusError)
			log.Error("watch error", "provider", provider.getLabel(), "err", statusErr)
		}
	}
	close(exitFunction)
	log.Info("stopping watching", "provider", provider.getLabel(), "service name", service.Name, "namespace", service.Namespace)
	return nil //nolint:govet
}

func (svcCtx *serviceContext) isNetworkConfigured(ip string) bool {
	_, exists := svcCtx.configuredNetworks.Load(ip)
	return exists
}
