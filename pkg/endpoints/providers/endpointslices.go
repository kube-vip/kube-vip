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
	label       string
	endpointsv4 []discoveryv1.Endpoint
	endpointsv6 []discoveryv1.Endpoint
	ports       []discoveryv1.EndpointPort
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

	if eps.AddressType == discoveryv1.AddressTypeIPv6 {
		ep.endpointsv6 = eps.Endpoints
	} else {
		ep.endpointsv4 = eps.Endpoints
	}

	// Store ports for resolving named ports
	ep.ports = eps.Ports

	return nil
}

func (ep *Endpointslices) GetAllEndpoints() ([]string, error) {
	result := []string{}
	for _, e := range ep.endpointsv4 {
		result = append(result, e.Addresses...)
	}
	for _, e := range ep.endpointsv6 {
		result = append(result, e.Addresses...)
	}
	return result, nil
}

func (ep *Endpointslices) GetLocalEndpoints(id string, _ *kubevip.Config) ([]string, error) {
	var localEndpoints []string
	tmpEps := []discoveryv1.Endpoint{}

	tmpEps = append(tmpEps, ep.endpointsv4...)
	tmpEps = append(tmpEps, ep.endpointsv6...)

	for _, endpoint := range tmpEps {
		if endpoint.Conditions.Serving == nil || !*endpoint.Conditions.Serving {
			continue
		}
		for _, address := range endpoint.Addresses {
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

func (ep *Endpointslices) UpdateServiceAnnotation(ctx context.Context, endpoint, endpointIPv6 string, service *v1.Service, clientSet *kubernetes.Clientset) error {
	retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Retrieve the latest version of Deployment before attempting update
		// RetryOnConflict uses exponential backoff to avoid exhausting the apiserver
		currentService, err := clientSet.CoreV1().Services(service.Namespace).Get(ctx, service.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}

		currentServiceCopy := currentService.DeepCopy()
		if currentServiceCopy.Annotations == nil {
			currentServiceCopy.Annotations = make(map[string]string)
		}

		currentServiceCopy.Annotations[kubevip.ActiveEndpoint] = endpoint
		currentServiceCopy.Annotations[kubevip.ActiveEndpointIPv6] = endpointIPv6

		_, err = clientSet.CoreV1().Services(currentService.Namespace).Update(ctx, currentServiceCopy, metav1.UpdateOptions{})
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

func (ep *Endpointslices) ResolvePort(servicePort v1.ServicePort) int32 {
	return ResolvePortWithLookup(servicePort, func(name string) int32 {
		for _, p := range ep.ports {
			if p.Name != nil && *p.Name == name && p.Port != nil {
				return *p.Port
			}
		}
		return 0
	})
}
