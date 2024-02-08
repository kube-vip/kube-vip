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

func (ep *endpointslicesProvider) getLocalEndpoints(id string, config *kubevip.Config) ([]string, error) {
	shortname, shortnameErr := getShortname(id)
	if shortnameErr != nil {
		if config.EnableRoutingTable && (!config.EnableLeaderElection && !config.EnableServicesElection) {
			log.Debugf("[%s] %v, shortname will not be used", ep.label, shortnameErr)
		} else {
			log.Errorf("[%s] %v", ep.label, shortnameErr)
		}
	}

	var localEndpoints []string
	for i := range ep.endpoints.Endpoints {
		for j := range ep.endpoints.Endpoints[i].Addresses {
			// 1. Compare the hostname on the endpoint to the hostname
			// 2. Compare the nodename on the endpoint to the hostname
			// 3. Drop the FQDN to a shortname and compare to the nodename on the endpoint

			// 1. Compare the Hostname first (should be FQDN)
			log.Debugf("[%s] processing endpoint [%s]", ep.label, ep.endpoints.Endpoints[i].Addresses[j])
			if ep.endpoints.Endpoints[i].Hostname != nil && id == *ep.endpoints.Endpoints[i].Hostname {
				if *ep.endpoints.Endpoints[i].Conditions.Serving {
					log.Debugf("[%s] found endpoint - address: %s, hostname: %s", ep.label, ep.endpoints.Endpoints[i].Addresses[j], *ep.endpoints.Endpoints[i].Hostname)
					localEndpoints = append(localEndpoints, ep.endpoints.Endpoints[i].Addresses[j])
				}
			} else {
				// 2. Compare the Nodename (from testing could be FQDN or short)
				if ep.endpoints.Endpoints[i].NodeName != nil {
					if id == *ep.endpoints.Endpoints[i].NodeName && *ep.endpoints.Endpoints[i].Conditions.Serving {
						if ep.endpoints.Endpoints[i].Hostname != nil {
							log.Debugf("[%s] found endpoint - address: %s, hostname: %s, node: %s", ep.label, ep.endpoints.Endpoints[i].Addresses[j], *ep.endpoints.Endpoints[i].Hostname, *ep.endpoints.Endpoints[i].NodeName)
						} else {
							log.Debugf("[%s] found endpoint - address: %s, node: %s", ep.label, ep.endpoints.Endpoints[i].Addresses[j], *ep.endpoints.Endpoints[i].NodeName)
						}
						localEndpoints = append(localEndpoints, ep.endpoints.Endpoints[i].Addresses[j])
						// 3. Compare to shortname
					} else if shortnameErr != nil && shortname == *ep.endpoints.Endpoints[i].NodeName && *ep.endpoints.Endpoints[i].Conditions.Serving {
						log.Debugf("[%s] found endpoint - address: %s, shortname: %s, node: %s", ep.label, ep.endpoints.Endpoints[i].Addresses[j], shortname, *ep.endpoints.Endpoints[i].NodeName)
						localEndpoints = append(localEndpoints, ep.endpoints.Endpoints[i].Addresses[j])
					}
				}
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
