package manager

import (
	"context"
	"encoding/json"
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

// The watcher will watch config maps for new services being created
func (sm *Manager) configmapWatcher(ctx context.Context, ns string) {
	// Build a options structure to defined what we're looking for
	listOptions := metav1.ListOptions{
		FieldSelector: fmt.Sprintf("metadata.name=%s", sm.configMap),
	}

	// Watch function
	// Use a restartable watcher, as this should help in the event of etcd or timeout issues
	rw, err := watchtools.NewRetryWatcher("1", &cache.ListWatch{
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			return sm.clientSet.CoreV1().ConfigMaps(ns).Watch(context.TODO(), listOptions)
		},
	})

	if err != nil {
		log.Errorf("error creating watcher: %s", err.Error())
		ctx.Done()
	}

	ch := rw.ResultChan()
	defer rw.Stop()
	log.Infof("Beginning watching Kubernetes configMap [%s]", sm.configMap)

	var services kubernetesServices

	go func() {
		for event := range ch {

			// We need to inspect the event and get ResourceVersion out of it
			switch event.Type {
			case watch.Added, watch.Modified:
				log.Debugf("ConfigMap [%s] has been Created or modified", sm.configMap)
				cm, ok := event.Object.(*v1.ConfigMap)
				if !ok {
					log.Errorf("Unable to parse ConfigMap from watcher")
					break
				}
				data := cm.Data["plndr-services"]
				json.Unmarshal([]byte(data), &services)
				log.Debugf("Found %d services defined in ConfigMap", len(services.Services))

				err = sm.syncServices(&services)
				if err != nil {
					log.Errorf("%v", err)
				}
			case watch.Deleted:
				log.Debugf("ConfigMap [%s] has been Deleted", sm.configMap)

			case watch.Bookmark:
				// Un-used
			case watch.Error:
				log.Infoln("err")

				// This round trip allows us to handle unstructured status
				errObject := apierrors.FromObject(event.Object)
				statusErr, ok := errObject.(*apierrors.StatusError)
				if !ok {
					log.Fatalf(spew.Sprintf("Received an error which is not *metav1.Status but %#+v", event.Object))
					// Retry unknown errors
					//return false, 0
				}

				status := statusErr.ErrStatus
				log.Errorf("%v", status)

			default:
			}
		}
	}()

	<-signalChan
}

// This file handles the watching of a services endpoints and updates a load balancers endpoint configurations accordingly
func (sm *Manager) serviceWatcher(s *Instance, ns string) error {
	// Build a options structure to defined what we're looking for
	listOptions := metav1.ListOptions{
		FieldSelector: fmt.Sprintf("metadata.name=%s", s.service.ServiceName),
	}

	// Watch function
	// Use a restartable watcher, as this should help in the event of etcd or timeout issues
	rw, err := watchtools.NewRetryWatcher("1", &cache.ListWatch{
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			return sm.clientSet.CoreV1().Endpoints(ns).Watch(context.TODO(), listOptions)
		},
	})
	if err != nil {
		return fmt.Errorf("error creating watcher: %s", err.Error())
	}

	ch := rw.ResultChan()
	defer rw.Stop()
	log.Infof("Beginning watching Kubernetes Endpoints for service [%s]", s.service.ServiceName)

	for event := range ch {

		// We need to inspect the event and get ResourceVersion out of it
		switch event.Type {
		case watch.Added, watch.Modified:
			// log.Debugf("Endpoints for service [%s] have been Created or modified", s.service.ServiceName)
			// ep, ok := event.Object.(*v1.Endpoints)
			// if !ok {
			// 	return fmt.Errorf("Unable to parse Endpoints from watcher")
			// }
			// s.vipConfig.LoadBalancers[0].Backends = rebuildEndpoints(*ep)

			// log.Debugf("Load-Balancer updated with [%d] backends", len(s.vipConfig.LoadBalancers[0].Backends))
		case watch.Deleted:
			log.Debugf("Endpoints for service [%s] have been Deleted", s.service.ServiceName)
			log.Infof("Service [%s] has been deleted, stopping VIP", s.service.ServiceName)
			// Stopping the service from running
			sm.stopService(s.service.UID)
			// Remove this service from the list of services we manage
			sm.deleteService(s.service.UID)
			return nil
		case watch.Bookmark:
			// Un-used
		case watch.Error:
			log.Infoln("err")

			// This round trip allows us to handle unstructured status
			errObject := apierrors.FromObject(event.Object)
			statusErr, ok := errObject.(*apierrors.StatusError)
			if !ok {
				log.Fatalf(spew.Sprintf("Received an error which is not *metav1.Status but %#+v", event.Object))
				// Retry unknown errors
				//return false, 0
			}

			status := statusErr.ErrStatus
			log.Errorf("%v", status)
		default:
		}
	}
	return nil
}

// This file handles the watching of a services endpoints and updates a load balancers endpoint configurations accordingly
func (sm *Manager) servicesWatcher(ctx context.Context) error {
	// Build a options structure to defined what we're looking for
	// listOptions := metav1.ListOptions{
	// 	FieldSelector: fields.S,
	// }

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
			log.Infof("Found Service [%s], it has [%d] external addresses", svc.Name, len(svc.Status.LoadBalancer.Ingress))
			// s.vipConfig.LoadBalancers[0].Backends = rebuildEndpoints(*ep)

			// log.Debugf("Load-Balancer updated with [%d] backends", len(s.vipConfig.LoadBalancers[0].Backends))
		case watch.Deleted:
			svc, ok := event.Object.(*v1.Service)
			if !ok {
				return fmt.Errorf("Unable to parse Endpoints from watcher")
			}
			log.Debugf("Endpoints for service [%s] have been Deleted", svc.Name)
			log.Infof("Service [%s] has been deleted, stopping VIP", svc.Name)

			return nil
		case watch.Bookmark:
			// Un-used
		case watch.Error:
			log.Infoln("err")

			// This round trip allows us to handle unstructured status
			errObject := apierrors.FromObject(event.Object)
			statusErr, ok := errObject.(*apierrors.StatusError)
			if !ok {
				log.Fatalf(spew.Sprintf("Received an error which is not *metav1.Status but %#+v", event.Object))
				// Retry unknown errors
				//return false, 0
			}

			status := statusErr.ErrStatus
			log.Errorf("%v", status)
		default:
		}
	}
	return nil
}
