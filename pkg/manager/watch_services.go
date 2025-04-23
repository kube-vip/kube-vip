package manager

import (
	"context"
	"fmt"
	"reflect"

	log "log/slog"

	"github.com/davecgh/go-spew/spew"
	"github.com/kube-vip/kube-vip/pkg/cluster"
	"github.com/kube-vip/kube-vip/pkg/vip"
	"github.com/prometheus/client_golang/prometheus"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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

// configuredNetworks keeps track of networks that has been configured on the node
var configuredNetworks map[string]map[string]vip.Network

func init() {
	// Set up the caches for monitoring existing active or watched services
	activeServiceLoadBalancerCancel = make(map[string]func())
	activeServiceLoadBalancer = make(map[string]context.Context)
	activeService = make(map[string]bool)
	watchedService = make(map[string]bool)
	configuredNetworks = make(map[string]map[string]vip.Network)
}

// This function handles the watching of a services endpoints and updates a load balancers endpoint configurations accordingly
func (sm *Manager) servicesWatcher(ctx context.Context, serviceFunc func(context.Context, *v1.Service) error) error {
	// first start port mirroring if enabled
	if err := sm.startTrafficMirroringIfEnabled(); err != nil {
		return err
	}
	defer func() {
		// clean up traffic mirror related config
		err := sm.stopTrafficMirroringIfEnabled()
		if err != nil {
			log.Error("Stopping traffic mirroring", "err", err)
		}
	}()

	if sm.config.ServiceNamespace == "" {
		// v1.NamespaceAll is actually "", but we'll stay with the const in case things change upstream
		sm.config.ServiceNamespace = v1.NamespaceAll
		log.Info("(svcs) starting services watcher for all namespaces")
	} else {
		log.Info("(svcs) starting services watcher", "namespace", sm.config.ServiceNamespace)
	}

	// Use a restartable watcher, as this should help in the event of etcd or timeout issues
	rw, err := watchtools.NewRetryWatcher("1", &cache.ListWatch{
		WatchFunc: func(_ metav1.ListOptions) (watch.Interface, error) {
			return sm.rwClientSet.CoreV1().Services(sm.config.ServiceNamespace).Watch(ctx, metav1.ListOptions{})
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

			// Check if we ignore this service
			if svc.Annotations["kube-vip.io/ignore"] == "true" {
				log.Info("ignore annotation for kube-vip", "service name", svc.Name)
				break
			}

			// Select loadbalancer class filtering function
			lbClassFilterFunc := sm.lbClassFilter
			if sm.config.LoadBalancerClassLegacyHandling {
				lbClassFilterFunc = sm.lbClassFilterLegacy
			}

			// Check the loadBalancer class
			if lbClassFilterFunc(svc) {
				break
			}

			svcAddresses := cluster.FetchServiceAddresses(svc)

			// We only care about LoadBalancer services that have been allocated an address
			if len(svcAddresses) <= 0 {
				break
			}

			// The modified event should only be triggered if the service has been modified (i.e. moved somewhere else)
			if event.Type == watch.Modified {
				i := sm.findServiceInstance(svc)
				originalService := []string{}
				shouldGarbageCollect := true
				if i != nil {
					originalService = cluster.FetchServiceAddresses(i.ServiceSnapshot)
					shouldGarbageCollect = !reflect.DeepEqual(originalService, svcAddresses)
				}
				if shouldGarbageCollect {
					for _, addr := range svcAddresses {
						// log.Debugf("(svcs) Retreiving local addresses, to ensure that this modified address doesn't exist: %s", addr)
						f, err := vip.GarbageCollect(sm.config.Interface, addr, sm.intfMgr)
						if err != nil {
							log.Error("(svcs) cleaning existing address error", "err", err)
						}
						if f {
							log.Warn("(svcs) already found existing config", "address", addr, "adapter", sm.config.Interface)
						}
					}
				}
				// This service has been modified, but it was also active..
				if activeService[string(svc.UID)] {
					if i != nil {
						if !reflect.DeepEqual(originalService, svcAddresses) {
							// Calls the cancel function of the context
							if activeServiceLoadBalancerCancel[string(svc.UID)] != nil {
								log.Warn("(svcs) The load balancer has changed, cancelling original load balancer")
								activeServiceLoadBalancerCancel[string(svc.UID)]()
								<-activeServiceLoadBalancer[string(svc.UID)].Done()
								log.Warn("(svcs) waiting for load balancer to finish")
							}
							err = sm.deleteService(svc.UID)
							if err != nil {
								log.Error("(svc) unable to remove", "service", svc.UID)
							}
							activeService[string(svc.UID)] = false
							watchedService[string(svc.UID)] = false
							delete(activeServiceLoadBalancer, string(svc.UID))
							delete(configuredNetworks, string(svc.UID))
						}
						// in theory this should never fail
					}
				}
			}

			// Architecture walkthrough: (Had to do this as this code path is making my head hurt)

			// Is the service active (bool), if not then process this new service
			// Does this service use an election per service?
			//

			if !activeService[string(svc.UID)] {
				log.Debug("(svcs) has been added/modified with addresses", "service name", svc.Name, "ip", cluster.FetchServiceAddresses(svc))

				activeServiceLoadBalancer[string(svc.UID)], activeServiceLoadBalancerCancel[string(svc.UID)] = context.WithCancel(ctx)

				if sm.config.EnableServicesElection || // Service Election
					((sm.config.EnableRoutingTable || sm.config.EnableBGP) && // Routing table mode or BGP
						(!sm.config.EnableLeaderElection && !sm.config.EnableServicesElection)) { // No leaderelection or services election

					// If this load balancer Traffic Policy is "local"
					if svc.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyTypeLocal {

						// Start an endpoint watcher if we're not watching it already
						if !watchedService[string(svc.UID)] {
							// background the endpoint watcher
							if (sm.config.EnableRoutingTable || sm.config.EnableBGP) && (!sm.config.EnableLeaderElection && !sm.config.EnableServicesElection) {
								err = serviceFunc(activeServiceLoadBalancer[string(svc.UID)], svc)
								if err != nil {
									log.Error(err.Error())
								}
							}

							go func() {
								if svc.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyTypeLocal {
									// Add Endpoint or EndpointSlices watcher
									var provider epProvider
									if !sm.config.EnableEndpointSlices {
										provider = &endpointsProvider{label: "endpoints"}
									} else {
										provider = &endpointslicesProvider{label: "endpointslices"}
									}
									if err = sm.watchEndpoint(activeServiceLoadBalancer[string(svc.UID)], sm.config.NodeName, svc, provider); err != nil {
										log.Error(err.Error())
									}
								}
							}()

							// We're now watching this service
							watchedService[string(svc.UID)] = true
						}
					} else if (sm.config.EnableBGP || sm.config.EnableRoutingTable) && (!sm.config.EnableLeaderElection && !sm.config.EnableServicesElection) {
						err = serviceFunc(activeServiceLoadBalancer[string(svc.UID)], svc)
						if err != nil {
							log.Error(err.Error())
						}

						go func() {
							if svc.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyTypeCluster {
								// Add Endpoint watcher
								var provider epProvider
								if !sm.config.EnableEndpointSlices {
									provider = &endpointsProvider{label: "endpoints"}
								} else {
									provider = &endpointslicesProvider{label: "endpointslices"}
								}
								if err = sm.watchEndpoint(activeServiceLoadBalancer[string(svc.UID)], sm.config.NodeName, svc, provider); err != nil {
									log.Error(err.Error())
								}
							}
						}()
						// We're now watching this service
						watchedService[string(svc.UID)] = true
					} else {

						go func() {
							for {
								select {
								case <-activeServiceLoadBalancer[string(svc.UID)].Done():
									log.Warn("(svcs) restartable service watcher ending", "uid", svc.UID)
									return
								default:
									log.Info("(svcs) restartable service watcher starting", "uid", svc.UID)
									err = serviceFunc(activeServiceLoadBalancer[string(svc.UID)], svc)

									if err != nil {
										log.Error(err.Error())
									}
								}
							}

						}()
					}
				} else {
					// Increment the waitGroup before the service Func is called (Done is completed in there)
					err = serviceFunc(activeServiceLoadBalancer[string(svc.UID)], svc)
					if err != nil {
						log.Error(err.Error())
					}
				}
				activeService[string(svc.UID)] = true
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
					log.Info("(svcs)ignore annotation for kube-vip", "service name", svc.Name)
					break
				}

				// If no leader election is enabled, delete routes here
				if !sm.config.EnableLeaderElection && !sm.config.EnableServicesElection &&
					sm.config.EnableRoutingTable && hasConfiguredNetworks(svc.UID) {
					if errs := sm.clearRoutes(svc); len(errs) == 0 {
						delete(configuredNetworks, string(svc.UID))
					}
				}

				// If this is an active service then and additional leaderElection will handle stopping
				err = sm.deleteService(svc.UID)
				if err != nil {
					log.Error(err.Error())
				}

				// Calls the cancel function of the context
				if activeServiceLoadBalancerCancel[string(svc.UID)] != nil {
					activeServiceLoadBalancerCancel[string(svc.UID)]()
				}
				activeService[string(svc.UID)] = false
				watchedService[string(svc.UID)] = false
			}

			if sm.config.EnableLeaderElection && !sm.config.EnableServicesElection {
				if sm.config.EnableBGP {
					sm.clearBGPHosts(svc)
				} else if sm.config.EnableRoutingTable {
					sm.clearRoutes(svc)
				}
			}

			log.Info("(svcs) deleted", "service name", svc.Name, "namespace", svc.Namespace)
		case watch.Bookmark:
			// Un-used
		case watch.Error:
			log.Error("Error attempting to watch Kubernetes services")

			// This round trip allows us to handle unstructured status
			errObject := apierrors.FromObject(event.Object)
			statusErr, ok := errObject.(*apierrors.StatusError)
			if !ok {
				log.Error(spew.Sprintf("Received an error which is not *metav1.Status but %#+v", event.Object))
			}

			status := statusErr.ErrStatus
			log.Error("services", "err", status)
		default:
		}
	}
	close(exitFunction)
	log.Warn("Stopping watching services for type: LoadBalancer in all namespaces")
	return nil
}

func (sm *Manager) lbClassFilterLegacy(svc *v1.Service) bool {
	if svc == nil {
		log.Info("(svcs) service is nil, ignoring")
		return true
	}
	if svc.Spec.LoadBalancerClass != nil {
		// if this isn't nil then it has been configured, check if it the kube-vip loadBalancer class
		if *svc.Spec.LoadBalancerClass != sm.config.LoadBalancerClassName {
			log.Info("(svcs) specified the wrong loadBalancer class", "service name", svc.Name, "lbClass", *svc.Spec.LoadBalancerClass)
			return true
		}
	} else if sm.config.LoadBalancerClassOnly {
		// if kube-vip is configured to only recognize services with kube-vip's lb class, then ignore the services without any lb class
		log.Info("(svcs) kube-vip configured to only recognize services with kube-vip's lb class but the service didn't specify any loadBalancer class, ignoring", "service name", svc.Name)
		return true
	}
	return false
}

func (sm *Manager) lbClassFilter(svc *v1.Service) bool {
	if svc == nil {
		log.Info("(svcs) service is nil, ignoring")
		return true
	}
	if svc.Spec.LoadBalancerClass == nil && sm.config.LoadBalancerClassName != "" {
		log.Info("(svcs) no loadBalancer class, ignoring", "service name", svc.Name, "expected lbClass", sm.config.LoadBalancerClassName)
		return true
	}
	if svc.Spec.LoadBalancerClass == nil && sm.config.LoadBalancerClassName == "" {
		return false
	}
	if *svc.Spec.LoadBalancerClass != sm.config.LoadBalancerClassName {
		log.Info("(svcs) specified wrong loadBalancer class, ignoring", "service name", svc.Name, "wrong lbClass", *svc.Spec.LoadBalancerClass, "expected lbClass", sm.config.LoadBalancerClassName)
		return true
	}
	return false
}

func hasConfiguredNetworks(serviceUID types.UID) bool {
	_, ok := configuredNetworks[string(serviceUID)]
	if !ok {
		return false
	}

	return len(configuredNetworks[string(serviceUID)]) > 0
}

func isNetworkConfigured(serviceUID types.UID, network vip.Network) bool {
	_, exists := configuredNetworks[string(serviceUID)]
	if !exists {
		return false
	}

	_, exists = configuredNetworks[string(serviceUID)][network.IP()]
	return exists
}
