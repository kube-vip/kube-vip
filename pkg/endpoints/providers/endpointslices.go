package providers

import (
	"context"
	"fmt"

	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	watchtools "k8s.io/client-go/tools/watch"
	"k8s.io/client-go/util/retry"
)

type Endpointslices struct {
	label     string
	endpoints *discoveryv1.EndpointSlice
}

func NewEndpointslices() Provider {
	return &Endpointslices{
		label: "endpointslices",
	}
}

func (ep *Endpointslices) CreateRetryWatcher(ctx context.Context, clientSet *kubernetes.Clientset,
	service *v1.Service) (*watchtools.RetryWatcher, error) {
	labelSelector := metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/service-name": service.Name}}

	opts := metav1.ListOptions{
		LabelSelector: labels.Set(labelSelector.MatchLabels).String(),
	}

	rw, err := watchtools.NewRetryWatcherWithContext(ctx, "1", &cache.ListWatch{
		WatchFunc: func(_ metav1.ListOptions) (watch.Interface, error) {
			return clientSet.DiscoveryV1().EndpointSlices(service.Namespace).Watch(ctx, opts)
		},
	})
	if err != nil {
		return nil, fmt.Errorf("[%s] error creating endpointslices watcher: %s", ep.label, err.Error())
	}

	return rw, nil
}

func (ep *Endpointslices) LoadObject(endpoints runtime.Object, cancel context.CancelFunc) error {
	eps, ok := endpoints.(*discoveryv1.EndpointSlice)
	if !ok {
		cancel()
		return fmt.Errorf("[%s] error casting endpoints to v1.Endpoints struct", ep.label)
	}
	ep.endpoints = eps
	return nil
}

func (ep *Endpointslices) GetAllEndpoints() ([]string, error) {
	result := []string{}
	for _, ep := range ep.endpoints.Endpoints {
		result = append(result, ep.Addresses...)
	}
	return result, nil
}

func (ep *Endpointslices) GetLocalEndpoints(id string, _ *kubevip.Config) ([]string, error) {
	var localEndpoints []string
	for _, endpoint := range ep.endpoints.Endpoints {
		if endpoint.Conditions.Serving == nil || !*endpoint.Conditions.Serving {
			continue
		}
		for _, address := range endpoint.Addresses {
			log.Debug("processing endpoint", "provider", ep.label, "ip", address)

			// 1. Compare the Nodename
			if endpoint.NodeName != nil && id == *endpoint.NodeName {
				if endpoint.Hostname != nil {
					log.Debug("found endpoint", "provider", ep.label, "ip", address, "hostname", *endpoint.Hostname, "nodename", *endpoint.NodeName)
				} else {
					log.Debug("found endpoint", "provider", ep.label, "ip", address, "nodename", *endpoint.NodeName)
				}
				localEndpoints = append(localEndpoints, address)
				continue
			}

			// 2. Compare the Hostname (only useful if endpoint.NodeName is not available)
			if endpoint.Hostname != nil && id == *endpoint.Hostname {
				log.Debug("found endpoint", "provider", ep.label, "ip", address, "hostname", *endpoint.Hostname)
				localEndpoints = append(localEndpoints, address)
			}
		}
	}
	return localEndpoints, nil
}

func (ep *Endpointslices) UpdateServiceAnnotation(endpoint, endpointIPv6 string, service *v1.Service, clientSet *kubernetes.Clientset) error {
	retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Retrieve the latest version of Deployment before attempting update
		// RetryOnConflict uses exponential backoff to avoid exhausting the apiserver
		currentService, err := clientSet.CoreV1().Services(service.Namespace).Get(context.TODO(), service.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}

		currentServiceCopy := currentService.DeepCopy()
		if currentServiceCopy.Annotations == nil {
			currentServiceCopy.Annotations = make(map[string]string)
		}

		currentServiceCopy.Annotations[kubevip.ActiveEndpoint] = endpoint
		currentServiceCopy.Annotations[kubevip.ActiveEndpointIPv6] = endpointIPv6

		_, err = clientSet.CoreV1().Services(currentService.Namespace).Update(context.TODO(), currentServiceCopy, metav1.UpdateOptions{})
		if err != nil {
			log.Error("error updating Service Spec", "provider", ep.label, "service name", currentServiceCopy.Name, "err", err)
			return err
		}
		return nil
	})

	if retryErr != nil {
		log.Error("failed to set Services", "provider", ep.label, "err", retryErr)
		return retryErr
	}
	return nil
}

func (ep *Endpointslices) GetLabel() string {
	return ep.label
}

func (ep *Endpointslices) GetProtocol() string {
	return string(ep.endpoints.AddressType)
}
