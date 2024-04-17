package manager

import (
	"context"
	"fmt"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	log "github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"
	watchtools "k8s.io/client-go/tools/watch"
	"k8s.io/client-go/util/retry"
)

type endpointslicesProvider struct {
	label     string
	endpoints *discoveryv1.EndpointSlice
}

func (ep *endpointslicesProvider) createRetryWatcher(ctx context.Context, sm *Manager,
	service *v1.Service) (*watchtools.RetryWatcher, error) {
	labelSelector := metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/service-name": service.Name}}

	opts := metav1.ListOptions{
		LabelSelector: labels.Set(labelSelector.MatchLabels).String(),
	}

	rw, err := watchtools.NewRetryWatcher("1", &cache.ListWatch{
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			return sm.clientSet.DiscoveryV1().EndpointSlices(service.Namespace).Watch(ctx, opts)
		},
	})
	if err != nil {
		return nil, fmt.Errorf("[%s] error creating endpointslices watcher: %s", ep.label, err.Error())
	}

	return rw, nil
}

func (ep *endpointslicesProvider) loadObject(endpoints runtime.Object, cancel context.CancelFunc) error {
	eps, ok := endpoints.(*discoveryv1.EndpointSlice)
	if !ok {
		cancel()
		return fmt.Errorf("[%s] error casting endpoints to v1.Endpoints struct", ep.label)
	}
	ep.endpoints = eps
	return nil
}

func (ep *endpointslicesProvider) getAllEndpoints() ([]string, error) {
	result := []string{}
	for _, ep := range ep.endpoints.Endpoints {
		result = append(result, ep.Addresses...)
	}
	return result, nil
}

func (ep *endpointslicesProvider) getLocalEndpoints(id string, _ *kubevip.Config) ([]string, error) {
	var localEndpoints []string
	for _, endpoint := range ep.endpoints.Endpoints {
		if !*endpoint.Conditions.Serving {
			continue
		}
		for _, address := range endpoint.Addresses {
			log.Debugf("[%s] processing endpoint [%s]", ep.label, address)

			// 1. Compare the Nodename
			if endpoint.NodeName != nil && id == *endpoint.NodeName {
				if endpoint.Hostname != nil {
					log.Debugf("[%s] found endpoint - address: %s, hostname: %s, node: %s", ep.label, address, *endpoint.Hostname, *endpoint.NodeName)
				} else {
					log.Debugf("[%s] found endpoint - address: %s, node: %s", ep.label, address, *endpoint.NodeName)
				}
				localEndpoints = append(localEndpoints, address)
				continue
			}

			// 2. Compare the Hostname (only useful if endpoint.NodeName is not available)
			if endpoint.Hostname != nil && id == *endpoint.Hostname {
				log.Debugf("[%s] found endpoint - address: %s, hostname: %s", ep.label, address, *endpoint.Hostname)
				localEndpoints = append(localEndpoints, address)
			}
		}
	}
	return localEndpoints, nil
}

func (ep *endpointslicesProvider) updateServiceAnnotation(endpoint, endpointIPv6 string, service *v1.Service, sm *Manager) error {
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
		currentServiceCopy.Annotations[activeEndpointIPv6] = endpointIPv6

		_, err = sm.clientSet.CoreV1().Services(currentService.Namespace).Update(context.TODO(), currentServiceCopy, metav1.UpdateOptions{})
		if err != nil {
			log.Errorf("[%s] error updating Service Spec [%s] : %v", ep.label, currentServiceCopy.Name, err)
			return err
		}
		return nil
	})

	if retryErr != nil {
		log.Errorf("[%s] failed to set Services: %v", ep.label, retryErr)
		return retryErr
	}
	return nil
}

func (ep *endpointslicesProvider) getLabel() string {
	return ep.label
}

func (ep *endpointslicesProvider) getProtocol() string {
	return string(ep.endpoints.AddressType)
}
