package service

import (
	"fmt"

	"github.com/davecgh/go-spew/spew"
	"github.com/plunder-app/kube-vip/pkg/kubevip"
	log "github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"
	watchtools "k8s.io/client-go/tools/watch"
)

// This file handles the watching of a services endpoints and updates a load balancers endpoint configurations accordingly

func (sm *Manager) newWatcher(s *serviceInstance) error {
	// Build a options structure to defined what we're looking for
	listOptions := metav1.ListOptions{
		FieldSelector: fmt.Sprintf("metadata.name=%s", s.service.ServiceName),
	}
	ns, err := returnNameSpace()
	if err != nil {
		return err
	}
	// Watch function
	// Use a restartable watcher, as this should help in the event of etcd or timeout issues
	rw, err := watchtools.NewRetryWatcher("1", &cache.ListWatch{
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			return sm.clientSet.CoreV1().Endpoints(ns).Watch(listOptions)
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
			log.Debugf("Endpoints for service [%s] have  been Created or modified", s.service.ServiceName)
			ep, ok := event.Object.(*v1.Endpoints)
			if !ok {
				return fmt.Errorf("Unable to parse Endpoints from watcher")
			}
			s.vipConfig.LoadBalancers[0].Backends = rebuildEndpoints(*ep)

			log.Debugf("Load-Balancer updated with [%d] backends", len(s.vipConfig.LoadBalancers[0].Backends))
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

func rebuildEndpoints(eps v1.Endpoints) []kubevip.BackEnd {
	var addresses []string
	var ports []int32

	for x := range eps.Subsets {
		// Loop over addresses
		for y := range eps.Subsets[x].Addresses {
			addresses = append(addresses, eps.Subsets[x].Addresses[y].IP)
		}
		for y := range eps.Subsets[x].Ports {
			ports = append(ports, eps.Subsets[x].Ports[y].Port)
		}
	}
	var newBackend []kubevip.BackEnd
	// Build endpoints
	log.Debugf("Updating %d endpoints", len(addresses))

	for x := range addresses {
		for y := range ports {
			// Print out Backends if debug logging is enabled
			if log.GetLevel() == log.DebugLevel {
				fmt.Printf("-> Address: %s:%d \n", addresses[x], ports[y])
			}
			newBackend = append(newBackend, kubevip.BackEnd{
				Address: addresses[x],
				Port:    int(ports[y]),
			})
		}
	}
	return newBackend
}
