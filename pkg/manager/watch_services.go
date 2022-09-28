package manager

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/davecgh/go-spew/spew"
	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"
	watchtools "k8s.io/client-go/tools/watch"
)

// activeServiceLoadBalancer keeps track of services that already have a leaderElection in place
var activeServiceLoadBalancer map[string]context.Context

// activeService keeps track of services that already have a leaderElection in place
var activeService map[string]bool

// watchedService keeps track of services that are already being watched
var watchedService map[string]bool

// This function handles the watching of a services endpoints and updates a load balancers endpoint configurations accordingly
func (sm *Manager) servicesWatcher(ctx context.Context, serviceFunc func(context.Context, *v1.Service, *sync.WaitGroup) error) error {
	// Watch function
	var wg sync.WaitGroup

	// Set up the activeServiceLoadBalancer
	activeServiceLoadBalancer = make(map[string]context.Context)
	activeService = make(map[string]bool)
	watchedService = make(map[string]bool)

	id, err := os.Hostname()
	if err != nil {
		return err
	}

	// Use a restartable watcher, as this should help in the event of etcd or timeout issues
	rw, err := watchtools.NewRetryWatcher("1", &cache.ListWatch{
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			return sm.clientSet.CoreV1().Services(v1.NamespaceAll).Watch(ctx, metav1.ListOptions{})
		},
	})
	if err != nil {
		return fmt.Errorf("error creating services watcher: %s", err.Error())
	}
	go func() {
		<-sm.signalChan
		// Cancel the context
		rw.Stop()
	}()
	ch := rw.ResultChan()
	//defer rw.Stop()

	// Used for tracking an active endpoint / pod
	for event := range ch {
		sm.countServiceWatchEvent.With(prometheus.Labels{"type": string(event.Type)}).Add(1)

		// We need to inspect the event and get ResourceVersion out of it
		switch event.Type {
		case watch.Added, watch.Modified:
			// log.Debugf("Endpoints for service [%s] have been Created or modified", s.service.ServiceName)
			svc, ok := event.Object.(*v1.Service)
			if !ok {
				return fmt.Errorf("unable to parse Kubernetes services from API watcher")
			}

			// We only care about LoadBalancer services
			if svc.Spec.Type != v1.ServiceTypeLoadBalancer {
				break
			}

			// We only care about LoadBalancer services that have been allocated an address
			if svc.Spec.LoadBalancerIP == "" {
				break
			}

			// Check the loadBalancer class
			if svc.Spec.LoadBalancerClass != nil {
				// if this isn't nil then it has been configured, check if it the kube-vip loadBalancer class
				if *svc.Spec.LoadBalancerClass != "kube-vip.io/kube-vip-class" {
					log.Infof("service [%s] specified the loadBalancer class [%s], ignoring", svc.Name, *svc.Spec.LoadBalancerClass)
					break
				}
			} else if sm.config.LoadBalancerClassOnly {
				// if kube-vip is configured to only recognize services with kube-vip's lb class, then ignore the services without any lb class
				log.Infof("kube-vip configured to only recognize services with kube-vip's lb class but the service [%s] didn't specify any loadBalancer class, ignoring", svc.Name)
				break
			}

			// Check if we ignore this service
			if svc.Annotations["kube-vip.io/ignore"] == "true" {
				log.Infof("service [%s] has an ignore annotation for kube-vip", svc.Name)
				break
			}

			log.Infof("service [%s] has been added/modified it has an assigned external addresses [%s]", svc.Name, svc.Spec.LoadBalancerIP)

			// Scenarios:
			// 1.

			if !activeService[string(svc.UID)] {
				wg.Add(1)
				activeServiceLoadBalancer[string(svc.UID)] = context.TODO()
				// Background the services election
				if sm.config.EnableServicesElection {
					if svc.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyTypeLocal {
						// Start an endpoint watcher if we're not watching it alredy
						if !watchedService[string(svc.UID)] {
							watchedService[string(svc.UID)] = true

							// background the endpoint watcher
							go func() {
								if svc.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyTypeLocal {
									// Add Endpoint watcher
									err = sm.watchEndpoint(activeServiceLoadBalancer[string(svc.UID)], id, svc, &wg)
									if err != nil {
										log.Errorf("%v", err)
									}
									watchedService[string(svc.UID)] = false
									wg.Wait()
								}
							}()
						}
					} else {
						go func() {
							err = serviceFunc(activeServiceLoadBalancer[string(svc.UID)], svc, &wg)
							if err != nil {
								log.Error(err)
							}
							wg.Wait()
						}()
					}
				} else {
					err = serviceFunc(activeServiceLoadBalancer[string(svc.UID)], svc, &wg)
					if err != nil {
						log.Error(err)
					}
					wg.Wait()
				}

			}
		case watch.Deleted:
			svc, ok := event.Object.(*v1.Service)
			if !ok {
				return fmt.Errorf("unable to parse Kubernetes services from API watcher")
			}
			if activeService[string(svc.UID)] {
				// We only care about LoadBalancer services
				if svc.Spec.Type != v1.ServiceTypeLoadBalancer {
					break
				}

				// We can ignore this service
				if svc.Annotations["kube-vip.io/ignore"] == "true" {
					log.Infof("service [%s] has an ignore annotation for kube-vip", svc.Name)
					break
				}
				// If this is a watched service then and additional leaderElection will handle stopping
				if !watchedService[string(svc.UID)] {
					err := sm.deleteService(string(svc.UID))
					if err != nil {
						log.Error(err)
					}
				}
				activeServiceLoadBalancer[string(svc.UID)].Done()
				activeService[string(svc.UID)] = false
				watchedService[string(svc.UID)] = false

				log.Infof("service [%s/%s] has been deleted", svc.Namespace, svc.Name)
			}
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
