package providers

import (
	"context"
	"fmt"
	"strings"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	watchtools "k8s.io/client-go/tools/watch"
	"k8s.io/client-go/util/retry"

	log "log/slog"
)

type Endpoints struct {
	label string
	//nolint:staticcheck // SA1019 endpoints are moving to an opt-in only
	endpoints *v1.Endpoints
}

func NewEndpoints() Provider {
	return &Endpoints{
		label: "endpoints",
	}
}

func (ep *Endpoints) CreateRetryWatcher(ctx context.Context, clientSet *kubernetes.Clientset,
	service *v1.Service) (*watchtools.RetryWatcher, error) {
	opts := metav1.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("metadata.name", service.Name).String(),
	}

	rw, err := watchtools.NewRetryWatcherWithContext(ctx, "1", &cache.ListWatch{
		WatchFunc: func(_ metav1.ListOptions) (watch.Interface, error) {
			return clientSet.CoreV1().Endpoints(service.Namespace).Watch(ctx, opts)
		},
	})

	if err != nil {
		return nil, fmt.Errorf("error creating endpoint watcher: %s", err.Error())
	}

	return rw, nil
}

func (ep *Endpoints) LoadObject(endpoints runtime.Object, cancel context.CancelFunc) error {
	//nolint:staticcheck // SA1019 endpoints have to be explicitly requested now
	eps, ok := endpoints.(*v1.Endpoints)
	if !ok {
		cancel()
		return fmt.Errorf("[%s] unable to parse Kubernetes services from API watcher", ep.GetLabel())
	}
	ep.endpoints = eps
	return nil
}

func (ep *Endpoints) GetAllEndpoints() ([]string, error) {
	result := []string{}
	for subset := range ep.endpoints.Subsets {
		for address := range ep.endpoints.Subsets[subset].Addresses {
			addr := strings.Split(ep.endpoints.Subsets[subset].Addresses[address].IP, "/")
			result = append(result, addr[0])
		}
	}

	return result, nil
}

func (ep *Endpoints) GetLocalEndpoints(id string, _ *kubevip.Config) ([]string, error) {
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

func (ep *Endpoints) UpdateServiceAnnotation(endpoint string, _ string, service *v1.Service, clientSet *kubernetes.Clientset) error {
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

		_, err = clientSet.CoreV1().Services(currentService.Namespace).Update(context.TODO(), currentServiceCopy, metav1.UpdateOptions{})
		if err != nil {
			log.Error("error updating Service Spec", "label", ep.GetLabel(), "name", currentServiceCopy.Name, "err", err)
			return err
		}
		return nil
	})

	if retryErr != nil {
		log.Error("failed to set Services", "label", ep.GetLabel(), "err", retryErr)
		return retryErr
	}
	return nil
}

func (ep *Endpoints) GetLabel() string {
	return ep.label
}

func (ep *Endpoints) GetProtocol() string {
	return ""
}
