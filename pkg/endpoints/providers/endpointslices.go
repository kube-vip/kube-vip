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
	label  string
	slices map[string]*discoveryv1.EndpointSlice
}

func NewEndpointslices() Provider {
	return &Endpointslices{
		label:  "endpointslices",
		slices: make(map[string]*discoveryv1.EndpointSlice),
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

	if ep.slices == nil {
		ep.slices = make(map[string]*discoveryv1.EndpointSlice)
	}
	ep.slices[eps.Name] = eps.DeepCopy()

	return nil
}

func (ep *Endpointslices) DeleteObject(endpoints runtime.Object) error {
	eps, ok := endpoints.(*discoveryv1.EndpointSlice)
	if !ok {
		return fmt.Errorf("[%s] unable to parse Kubernetes object", ep.GetLabel())
	}
	delete(ep.slices, eps.Name)
	return nil
}

// isServing reports whether an endpoint should receive traffic. Per the
// EndpointConditions godoc a nil Serving defers to Ready, and a nil Ready is an
// unknown state that consumers should interpret as ready.
func isServing(conditions discoveryv1.EndpointConditions) bool {
	serving := conditions.Serving
	if serving == nil {
		serving = conditions.Ready
	}
	return serving == nil || *serving
}

func (ep *Endpointslices) GetAllEndpoints() ([]string, error) {
	result := []string{}
	seen := map[string]struct{}{}
	for _, eps := range ep.slices {
		for _, e := range eps.Endpoints {
			if !isServing(e.Conditions) {
				continue
			}
			for _, address := range e.Addresses {
				if _, ok := seen[address]; ok {
					continue
				}
				seen[address] = struct{}{}
				result = append(result, address)
			}
		}
	}
	return result, nil
}

func (ep *Endpointslices) GetLocalEndpoints(id string, _ *kubevip.Config) ([]string, error) {
	var localEndpoints []string
	seen := map[string]struct{}{}
	for _, eps := range ep.slices {
		for _, endpoint := range eps.Endpoints {
			if !isServing(endpoint.Conditions) {
				continue
			}
			for _, address := range endpoint.Addresses {
				if _, ok := seen[address]; ok {
					continue
				}
				// 1. Compare the Nodename
				if endpoint.NodeName != nil && id == *endpoint.NodeName {
					if endpoint.Hostname != nil {
						log.Debug("found endpoint", "provider", ep.label, "ip", address, "hostname", *endpoint.Hostname, "nodename", *endpoint.NodeName)
					} else {
						log.Debug("found endpoint", "provider", ep.label, "ip", address, "nodename", *endpoint.NodeName)
					}
					localEndpoints = append(localEndpoints, address)
					seen[address] = struct{}{}
					continue
				}

				// 2. Compare the Hostname (only useful if endpoint.NodeName is not available)
				if endpoint.NodeName == nil && endpoint.Hostname != nil && id == *endpoint.Hostname {
					log.Debug("found endpoint", "provider", ep.label, "ip", address, "hostname", *endpoint.Hostname)
					localEndpoints = append(localEndpoints, address)
					seen[address] = struct{}{}
				}
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
		for _, eps := range ep.slices {
			for _, p := range eps.Ports {
				if p.Name != nil && *p.Name == name && p.Port != nil {
					return *p.Port
				}
			}
		}
		return 0
	})
}

func (ep *Endpointslices) GetBackends(servicePort v1.ServicePort, nodeName string, local bool) ([]Backend, error) {
	seen := map[Backend]struct{}{}
	backends := []Backend{}
	for _, eps := range ep.slices {
		targetPort := ResolvePortWithLookup(servicePort, func(name string) int32 {
			for _, port := range eps.Ports {
				if port.Name != nil && *port.Name == name && port.Port != nil && endpointPortProtocol(port) == servicePort.Protocol {
					return *port.Port
				}
			}
			return 0
		})
		for _, endpoint := range eps.Endpoints {
			if !isServing(endpoint.Conditions) || (local && !endpointIsLocal(endpoint, nodeName)) {
				continue
			}
			for _, address := range endpoint.Addresses {
				backend := Backend{Address: address, Port: targetPort}
				if _, ok := seen[backend]; ok {
					continue
				}
				seen[backend] = struct{}{}
				backends = append(backends, backend)
			}
		}
	}
	return backends, nil
}

func endpointIsLocal(endpoint discoveryv1.Endpoint, nodeName string) bool {
	if endpoint.NodeName != nil {
		return *endpoint.NodeName == nodeName
	}
	return endpoint.Hostname != nil && *endpoint.Hostname == nodeName
}

func endpointPortProtocol(port discoveryv1.EndpointPort) v1.Protocol {
	if port.Protocol == nil {
		return v1.ProtocolTCP
	}
	return *port.Protocol
}
