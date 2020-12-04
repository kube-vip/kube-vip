package manager

import (
	"context"
	"fmt"

	log "github.com/sirupsen/logrus"

	"github.com/davecgh/go-spew/spew"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	watchtools "k8s.io/client-go/tools/watch"

	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"
)

// This file handles the watching of a services endpoints and updates a load balancers endpoint configurations accordingly
func (sm *Manager) servicesWatcher(ctx context.Context) error {
	// Watch function

	// Use a restartable watcher, as this should help in the event of etcd or timeout issues
	rw, err := watchtools.NewRetryWatcher("1", &cache.ListWatch{
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			return sm.clientSet.CoreV1().Services(v1.NamespaceAll).Watch(ctx, metav1.ListOptions{})
		},
	})
	if err != nil {
		return fmt.Errorf("error creating services watcher: %s", err.Error())
	}

	ch := rw.ResultChan()
	defer rw.Stop()
	log.Infoln("Beginning watching services for type: LoadBalancer in all namespaces")

	for event := range ch {

		// We need to inspect the event and get ResourceVersion out of it
		switch event.Type {
		case watch.Added, watch.Modified:
			// log.Debugf("Endpoints for service [%s] have been Created or modified", s.service.ServiceName)
			svc, ok := event.Object.(*v1.Service)
			if !ok {
				return fmt.Errorf("Unable to parse Kubernetes services from API watcher")
			}
			log.Infof("Found Service [%s], it has an assigned external addresses [%s]", svc.Name, len(svc.Spec.LoadBalancerIP))
			err = sm.syncServices(svc)
			if err != nil {
				log.Error(err)
			}

		case watch.Deleted:
			svc, ok := event.Object.(*v1.Service)
			if !ok {
				return fmt.Errorf("Unable to parse Kubernetes services from API watcher")
			}
			sm.deleteService(string(svc.UID))

		case watch.Bookmark:
			// Un-used
		case watch.Error:
			log.Error("Error attempting to watch Kubernetes services")

			// This round trip allows us to handle unstructured status
			errObject := apierrors.FromObject(event.Object)
			statusErr, ok := errObject.(*apierrors.StatusError)
			if !ok {
				log.Errorf(spew.Sprintf("Received an error which is not *metav1.Status but %#+v", event.Object))

			}

			status := statusErr.ErrStatus
			log.Errorf("%v", status)
		default:
		}
	}
	log.Warnln("Stopping watching services for type: LoadBalancer in all namespaces")
	return nil
}
