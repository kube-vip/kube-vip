package manager

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/davecgh/go-spew/spew"
	"github.com/kube-vip/kube-vip/pkg/vip"
	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"
	watchtools "k8s.io/client-go/tools/watch"
)

// TODO: Fix the naming of these contexts

// activeServiceLoadBalancer keeps track of services that already have a leaderElection in place
var activeServiceLoadBalancer map[string]context.Context

// activeServiceLoadBalancer keeps track of services that already have a leaderElection in place
var activeServiceLoadBalancerCancel map[string]func()

// activeService keeps track of services that already have a leaderElection in place
var activeService map[string]bool

// watchedService keeps track of services that are already being watched
var watchedService map[string]bool

func init() {
	// Set up the caches for monitoring existing active or watched services
	activeServiceLoadBalancerCancel = make(map[string]func())
	activeServiceLoadBalancer = make(map[string]context.Context)
	activeService = make(map[string]bool)
	watchedService = make(map[string]bool)
}

// This function handles the watching of a services endpoints and updates a load balancers endpoint configurations accordingly
func (sm *Manager) servicesWatcher(ctx context.Context, serviceFunc func(context.Context, *v1.Service, *sync.WaitGroup) error) error {
	// Watch function
	var wg sync.WaitGroup

	id, err := os.Hostname()
	if err != nil {
		return err
	}
	if sm.config.ServiceNamespace == "" {
		// v1.NamespaceAll is actually "", but we'll stay with the const in case things change upstream
		sm.config.ServiceNamespace = v1.NamespaceAll
		log.Infof("(svcs) starting services watcher for all namespaces")
	} else {
		log.Infof("(svcs) starting services watcher for services in namespace [%s]", sm.config.ServiceNamespace)
	}

	// Use a restartable watcher, as this should help in the event of etcd or timeout issues
	rw, err := watchtools.NewRetryWatcher("1", &cache.ListWatch{
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			return sm.clientSet.CoreV1().Services(sm.config.ServiceNamespace).Watch(ctx, metav1.ListOptions{})
		},
	})
	if err != nil {
		return fmt.Errorf("error creating services watcher: %s", err.Error())
	}
	exitFunction := make(chan struct{})
	go func() {
		select {
		case <-sm.shutdownChan:
			log.Debug("(svcs) shutdown called")
			// Stop the retry watcher
			rw.Stop()
			return
		case <-exitFunction:
			log.Debug("(svcs) function ending")
			// Stop the retry watcher
			rw.Stop()
			return
		}
	}()
	ch := rw.ResultChan()

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
			if fetchServiceAddress(svc) == "" {
				break
			}

			// Check the loadBalancer class
			if svc.Spec.LoadBalancerClass != nil {
				// if this isn't nil then it has been configured, check if it the kube-vip loadBalancer class
				if *svc.Spec.LoadBalancerClass != sm.config.LoadBalancerClassName {
					log.Infof("(svcs) [%s] specified the loadBalancer class [%s], ignoring", svc.Name, *svc.Spec.LoadBalancerClass)
					break
				}
			} else if sm.config.LoadBalancerClassOnly {
				// if kube-vip is configured to only recognize services with kube-vip's lb class, then ignore the services without any lb class
				log.Infof("(svcs) kube-vip configured to only recognize services with kube-vip's lb class but the service [%s] didn't specify any loadBalancer class, ignoring", svc.Name)
				break
			}

			// Check if we ignore this service
			if svc.Annotations["kube-vip.io/ignore"] == "true" {
				log.Infof("(svcs) [%s] has an ignore annotation for kube-vip", svc.Name)
				break
			}

			// The modified event should only be triggered if the service has been modified (i.e. moved somewhere else)
			if event.Type == watch.Modified {
				//log.Debugf("(svcs) Retreiving local addresses, to ensure that this modified address doesn't exist")
				f, err := vip.GarbageCollect(sm.config.Interface, svc.Spec.LoadBalancerIP)
				if err != nil {
					log.Errorf("(svcs) cleaning existing address error: [%s]", err.Error())
				}
				if f {
					log.Warnf("(svcs) already found existing address [%s] on adapter [%s]", svc.Spec.LoadBalancerIP, sm.config.Interface)
				}
			}
			// Scenarios:
			// 1.
			if !activeService[string(svc.UID)] {
				log.Debugf("(svcs) [%s] has been added/modified with addresses [%s]", svc.Name, fetchServiceAddress(svc))

				wg.Add(1)
				activeServiceLoadBalancer[string(svc.UID)], activeServiceLoadBalancerCancel[string(svc.UID)] = context.WithCancel(context.TODO())
				// Background the services election
				if sm.config.EnableServicesElection || sm.config.EnableRoutingTable && !sm.config.EnableLeaderElection {
					if svc.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyTypeLocal {
						// Start an endpoint watcher if we're not watching it already
						if !watchedService[string(svc.UID)] {
							// background the endpoint watcher
							go func() {
								if svc.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyTypeLocal {
									// Add Endpoint watcher
									wg.Add(1)
									err = sm.watchEndpoint(activeServiceLoadBalancer[string(svc.UID)], id, svc, &wg)
									if err != nil {
										log.Error(err)
									}
									wg.Done()
								}
							}()
							// We're now watching this service
							watchedService[string(svc.UID)] = true
						}
					} else {
						// Increment the waitGroup before the service Func is called (Done is completed in there)
						wg.Add(1)
						go func() {
							err = serviceFunc(activeServiceLoadBalancer[string(svc.UID)], svc, &wg)
							if err != nil {
								log.Error(err)
							}
							wg.Done()
						}()
					}
					activeService[string(svc.UID)] = true
				} else {
					// Increment the waitGroup before the service Func is called (Done is completed in there)
					wg.Add(1)
					err = serviceFunc(activeServiceLoadBalancer[string(svc.UID)], svc, &wg)
					if err != nil {
						log.Error(err)
					}
					wg.Done()
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
					log.Infof("(svcs) [%s] has an ignore annotation for kube-vip", svc.Name)
					break
				}
				// If this is an active service then and additional leaderElection will handle stopping
				err := sm.deleteService(string(svc.UID))
				if err != nil {
					log.Error(err)
				}

				// Calls the cancel function of the context
				if activeServiceLoadBalancerCancel[string(svc.UID)] != nil {

					activeServiceLoadBalancerCancel[string(svc.UID)]()
				}
				activeService[string(svc.UID)] = false
				watchedService[string(svc.UID)] = false
			}
			log.Infof("(svcs) [%s/%s] has been deleted", svc.Namespace, svc.Name)
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
			log.Errorf("services -> %v", status)
		default:
		}
	}
	close(exitFunction)
	log.Warnln("Stopping watching services for type: LoadBalancer in all namespaces")
	return nil
}
