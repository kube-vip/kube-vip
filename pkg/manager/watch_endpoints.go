package manager

import (
	"context"
	"fmt"
	"net"
	"strings"
	"syscall"

	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/vip"
	"github.com/pkg/errors"
	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"
	watchtools "k8s.io/client-go/tools/watch"
	"k8s.io/client-go/util/retry"
)

type epProvider interface {
	createRetryWatcher(context.Context, *Manager,
		*v1.Service) (*watchtools.RetryWatcher, error)
	getAllEndpoints() ([]string, error)
	getLocalEndpoints(string, *kubevip.Config) ([]string, error)
	getLabel() string
	updateServiceAnnotation(string, string, *v1.Service, *Manager) error
	loadObject(runtime.Object, context.CancelFunc) error
	getProtocol() string
}

type endpointsProvider struct {
	label     string
	endpoints *v1.Endpoints
}

func (ep *endpointsProvider) createRetryWatcher(ctx context.Context, sm *Manager,
	service *v1.Service) (*watchtools.RetryWatcher, error) {
	opts := metav1.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("metadata.name", service.Name).String(),
	}

	rw, err := watchtools.NewRetryWatcher("1", &cache.ListWatch{
		WatchFunc: func(_ metav1.ListOptions) (watch.Interface, error) {
			return sm.rwClientSet.CoreV1().Endpoints(service.Namespace).Watch(ctx, opts)
		},
	})

	if err != nil {
		return nil, fmt.Errorf("error creating endpoint watcher: %s", err.Error())
	}

	return rw, nil
}

func (ep *endpointsProvider) loadObject(endpoints runtime.Object, cancel context.CancelFunc) error {
	eps, ok := endpoints.(*v1.Endpoints)
	if !ok {
		cancel()
		return fmt.Errorf("[%s] unable to parse Kubernetes services from API watcher", ep.getLabel())
	}
	ep.endpoints = eps
	return nil
}

func (ep *endpointsProvider) getAllEndpoints() ([]string, error) {
	result := []string{}
	for subset := range ep.endpoints.Subsets {
		for address := range ep.endpoints.Subsets[subset].Addresses {
			addr := strings.Split(ep.endpoints.Subsets[subset].Addresses[address].IP, "/")
			result = append(result, addr[0])
		}
	}

	return result, nil
}

func (ep *endpointsProvider) getLocalEndpoints(id string, _ *kubevip.Config) ([]string, error) {
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

func (ep *endpointsProvider) updateServiceAnnotation(endpoint string, _ string, service *v1.Service, sm *Manager) error {
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

		_, err = sm.clientSet.CoreV1().Services(currentService.Namespace).Update(context.TODO(), currentServiceCopy, metav1.UpdateOptions{})
		if err != nil {
			log.Error("error updating Service Spec", "label", ep.getLabel(), "name", currentServiceCopy.Name, "err", err)
			return err
		}
		return nil
	})

	if retryErr != nil {
		log.Error("failed to set Services", "label", ep.getLabel(), "err", retryErr)
		return retryErr
	}
	return nil
}

func (ep *endpointsProvider) getLabel() string {
	return ep.label
}

func (ep *endpointsProvider) getProtocol() string {
	return ""
}

func (sm *Manager) watchEndpoint(ctx context.Context, id string, service *v1.Service, provider epProvider) error {
	log.Info("watching", "provider", provider.getLabel(), "service_name", service.Name, "namespace", service.Namespace)
	// Use a restartable watcher, as this should help in the event of etcd or timeout issues
	leaderContext, cancel := context.WithCancel(ctx)
	defer cancel()

	var leaderElectionActive bool

	rw, err := provider.createRetryWatcher(leaderContext, sm, service)
	if err != nil {
		cancel()
		return fmt.Errorf("[%s] error watching endpoints: %w", provider.getLabel(), err)
	}

	exitFunction := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			log.Debug("context cancelled", "provider", provider.getLabel())
			// Stop the retry watcher
			rw.Stop()
			// Cancel the context, which will in turn cancel the leadership
			cancel()
			return
		case <-sm.shutdownChan:
			log.Debug("shutdown called", "provider", provider.getLabel())
			// Stop the retry watcher
			rw.Stop()
			// Cancel the context, which will in turn cancel the leadership
			cancel()
			return
		case <-exitFunction:
			log.Debug("function ending", "provider", provider.getLabel())
			// Stop the retry watcher
			rw.Stop()
			// Cancel the context, which will in turn cancel the leadership
			cancel()
			return
		}
	}()

	ch := rw.ResultChan()

	var lastKnownGoodEndpoint string
	for event := range ch {
		activeEndpointAnnotation := activeEndpoint
		// We need to inspect the event and get ResourceVersion out of it
		switch event.Type {

		case watch.Added, watch.Modified:

			if err = provider.loadObject(event.Object, cancel); err != nil {
				return fmt.Errorf("[%s] error loading k8s object: %w", provider.getLabel(), err)
			}

			if sm.config.EnableEndpointSlices && provider.getProtocol() == string(discoveryv1.AddressTypeIPv6) {
				activeEndpointAnnotation = activeEndpointIPv6
			}

			// Build endpoints
			var endpoints []string
			if (sm.config.EnableBGP || sm.config.EnableRoutingTable) && !sm.config.EnableLeaderElection && !sm.config.EnableServicesElection &&
				service.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyTypeCluster {
				if endpoints, err = provider.getAllEndpoints(); err != nil {
					return fmt.Errorf("[%s] error getting all endpoints: %w", provider.getLabel(), err)
				}
			} else {
				if endpoints, err = provider.getLocalEndpoints(id, sm.config); err != nil {
					return fmt.Errorf("[%s] error getting local endpoints: %w", provider.getLabel(), err)
				}
			}

			if sm.config.EnableRoutingTable {
				instance := sm.findServiceInstance(service)
				if err != nil {
					log.Error("failed to find the instance", "service", service.UID, "provider", provider.getLabel(), "err", err)
				}
				if instance == nil {
					log.Error("failed to find the instance", "service", service.UID, "provider", provider.getLabel())
				} else {
					for _, c := range instance.Clusters {
						for n := range c.Network {
							// if there are no endpoints set HasEndpoints false just in case
							if len(endpoints) < 1 {
								c.Network[n].SetHasEndpoints(false)
							}
							// check if endpoint are available and are of same IP family as service
							if len(endpoints) > 0 && ((net.ParseIP(c.Network[n].IP()).To4() == nil) == (net.ParseIP(endpoints[0]).To4() == nil)) {
								c.Network[n].SetHasEndpoints(true)
							}
						}
					}
				}
			}

			// Find out if we have any local endpoints
			// if out endpoint is empty then populate it
			// if not, go through the endpoints and see if ours still exists
			// If we have a local endpoint then begin the leader Election, unless it's already running
			//

			// Check that we have local endpoints
			if len(endpoints) != 0 {
				// Ignore IPv4
				if service.Annotations[egressIPv6] == "true" && net.ParseIP(endpoints[0]).To4() != nil {
					continue
				}

				// if we haven't populated one, then do so
				if lastKnownGoodEndpoint != "" {

					// check out previous endpoint exists
					stillExists := false

					for x := range endpoints {
						if endpoints[x] == lastKnownGoodEndpoint {
							stillExists = true
						}
					}
					// If the last endpoint no longer exists, we cancel our leader Election, and set another endpoint as last known good
					if !stillExists {
						if sm.config.EnableRoutingTable {
							if err := sm.TeardownEgress(lastKnownGoodEndpoint, service.Spec.LoadBalancerIP,
								service.Namespace, service.Annotations); err != nil {
								log.Warn("removing redundant egress rules", "err", err)
							}
						}
						if leaderElectionActive && (sm.config.EnableServicesElection || sm.config.EnableLeaderElection) {
							log.Warn(" existing endpoint has been removed, restarting leaderElection", "provider", provider.getLabel(), "endpoint", lastKnownGoodEndpoint)
							// Stop the existing leaderElection
							cancel()
							// disable last leaderElection flag
							leaderElectionActive = false
						}
						// Set our active endpoint to an existing one
						lastKnownGoodEndpoint = endpoints[0]
					}
				} else {
					lastKnownGoodEndpoint = endpoints[0]
				}

				if !leaderElectionActive && sm.config.EnableServicesElection {
					go func() {
						leaderContext, cancel = context.WithCancel(ctx)

						// This is a blocking function, that will restart (in the event of failure)
						for {
							// if the context isn't cancelled restart
							if leaderContext.Err() != context.Canceled {
								leaderElectionActive = true
								err := sm.StartServicesLeaderElection(leaderContext, service)
								if err != nil {
									log.Error(err.Error())
								}
								leaderElectionActive = false
							} else {
								leaderElectionActive = false
								break
							}
						}
					}()
				}

				// There are local endpoints available on the node
				if !sm.config.EnableServicesElection && !sm.config.EnableLeaderElection {
					// If routing table mode is enabled - routes should be added per node
					if sm.config.EnableRoutingTable {
						instance := sm.findServiceInstance(service)
						if instance != nil {
							for _, cluster := range instance.Clusters {
								for i := range cluster.Network {
									if !isNetworkConfigured(service.UID, cluster.Network[i]) && cluster.Network[i].HasEndpoints() {
										err := cluster.Network[i].AddRoute(false)
										if err != nil {
											if errors.Is(err, syscall.EEXIST) {
												// If route exists, but protocol is not set (e.g. the route was created by the older version
												// of kube-vip) try to update it if necessary
												isUpdated, err := cluster.Network[i].UpdateRoutes()
												if err != nil {
													return fmt.Errorf("[%s] error updating existing routes: %w", provider.getLabel(), err)
												}
												if isUpdated {
													log.Info("updated route", "provider",
														provider.getLabel(), "ip", cluster.Network[i].IP(), "service name", service.Name, "namespace", service.Namespace, "interface", cluster.Network[i].Interface(), "tableID", sm.config.RoutingTableID)
												} else {
													log.Info("route already present", "provider",
														provider.getLabel(), "ip", cluster.Network[i].IP(), "service name", service.Name, "namespace", service.Namespace, "interface", cluster.Network[i].Interface(), "tableID", sm.config.RoutingTableID)
												}
											} else {
												// If other error occurs, return error
												return fmt.Errorf("[%s] error adding route: %s", provider.getLabel(), err.Error())
											}
										} else {
											log.Info("added route", "provider",
												provider.getLabel(), "ip", cluster.Network[i].IP(), "service name", service.Name, "namespace", service.Namespace, "interface", cluster.Network[i].Interface(), "tableID", sm.config.RoutingTableID)
											storeConfiguredNetwork(string(service.UID), cluster.Network[i])
											leaderElectionActive = true
										}
									}
								}
							}
						}
					}

					// If BGP mode is enabled - hosts should be added per node
					if sm.config.EnableBGP {
						if instance := sm.findServiceInstance(service); instance != nil {
							for _, cluster := range instance.Clusters {
								for i := range cluster.Network {
									if !isNetworkConfigured(service.UID, cluster.Network[i]) {
										network := cluster.Network[i]
										if err != nil {
											log.Error("error formatting address with subnet mask", "err", err)
										}
										log.Debug("attempting to advertise BGP service", "provider", provider.getLabel(), "ip", network.CIDR())
										err = sm.bgpServer.AddHost(network.CIDR())
										if err != nil {
											log.Error("error adding BGP host", "provider", provider.getLabel(), "err", err)
										} else {
											log.Info("added BGP host", "provider",
												provider.getLabel(), "ip", network.CIDR(), "service name", service.Name, "namespace", service.Namespace)
											storeConfiguredNetwork(string(service.UID), cluster.Network[i])
											leaderElectionActive = true
										}
									}
								}
							}
						}
					}
				}
			} else {
				// There are no local endpoints
				if !sm.config.EnableServicesElection && !sm.config.EnableLeaderElection {
					// If routing table mode is enabled - routes should be deleted
					if sm.config.EnableRoutingTable {
						if errs := sm.clearRoutes(service); len(errs) == 0 {
							delete(configuredNetworks, string(service.UID))
						} else {
							for _, err := range errs {
								log.Error("error while clearing routes", "err", err)
							}
						}
					}

					// If BGP mode is enabled - routes should be deleted
					if sm.config.EnableBGP {
						if instance := sm.findServiceInstance(service); instance != nil {
							for _, cluster := range instance.Clusters {
								for i := range cluster.Network {
									network := cluster.Network[i]
									err = sm.bgpServer.DelHost(network.CIDR())
									if err != nil {
										log.Error("[endpoint] deleting BGP host", "provider", provider.getLabel(), "ip", network.CIDR(), "err", err)
									} else {
										log.Info("[endpoint] deleted BGP host", "provider",
											provider.getLabel(), "ip", network.CIDR(), "service name", service.Name, "namespace", service.Namespace)

										delete(configuredNetworks[string(service.UID)], cluster.Network[i].IP())
										leaderElectionActive = false
									}
								}
							}
						}
					}
				}

				// If there are no local endpoints, and we had one then remove it and stop the leaderElection
				if lastKnownGoodEndpoint != "" {
					log.Warn("existing endpoint has been removed, no remaining endpoints for leaderElection", "provider", provider.getLabel(), "endpoint", lastKnownGoodEndpoint)
					if err := sm.TeardownEgress(lastKnownGoodEndpoint, service.Spec.LoadBalancerIP, service.Namespace, service.Annotations); err != nil {
						log.Error("error removing redundant egress rules", "err", err)
					}

					lastKnownGoodEndpoint = "" // reset endpoint
					if sm.config.EnableServicesElection || sm.config.EnableLeaderElection {
						cancel() // stop services watcher
					}
					leaderElectionActive = false
				}
			}
			// Set the service accordingly
			if service.Annotations[egress] == "true" {
				service.Annotations[activeEndpointAnnotation] = lastKnownGoodEndpoint
			}

			log.Debug("watcher", "provider",
				provider.getLabel(), "service name", service.Name, "namespace", service.Namespace, "endpoints", len(endpoints), "last endpoint", lastKnownGoodEndpoint, "active leader election", leaderElectionActive)

		case watch.Deleted:
			// When no-leader-elecition mode
			if !sm.config.EnableServicesElection && !sm.config.EnableLeaderElection {
				// find all existing local endpoints
				var endpoints []string
				if (sm.config.EnableBGP || sm.config.EnableRoutingTable) && !sm.config.EnableLeaderElection && !sm.config.EnableServicesElection &&
					service.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyTypeCluster {
					if endpoints, err = provider.getAllEndpoints(); err != nil {
						return fmt.Errorf("[%s] error getting all endpoints: %w", provider.getLabel(), err)
					}
				} else {
					if endpoints, err = provider.getLocalEndpoints(id, sm.config); err != nil {
						return fmt.Errorf("[%s] error getting all endpoints: %w", provider.getLabel(), err)
					}
				}

				// If there were local endpoints deleted
				if len(endpoints) > 0 {
					// Delete all routes in routing table mode
					if sm.config.EnableRoutingTable {
						sm.clearRoutes(service)
					}

					// Delete all hosts in BGP mode
					if sm.config.EnableBGP {
						sm.clearBGPHosts(service)
					}
				}
			}

			// Close the goroutine that will end the retry watcher, then exit the endpoint watcher function
			close(exitFunction)
			log.Info("stopping watching", "provider", provider.getLabel(), "service name", service.Name, "namespace", service.Namespace)

			return nil
		case watch.Error:
			errObject := apierrors.FromObject(event.Object)
			statusErr, _ := errObject.(*apierrors.StatusError)
			log.Error("watch error", "provider", provider.getLabel(), "err", statusErr)
		}
	}
	close(exitFunction)
	log.Info("stopping watching", "provider", provider.getLabel(), "service name", service.Name, "namespace", service.Namespace)
	return nil //nolint:govet
}

func (sm *Manager) clearRoutes(service *v1.Service) []error {
	errs := []error{}
	if instance := sm.findServiceInstance(service); instance != nil {
		for _, cluster := range instance.Clusters {
			for i := range cluster.Network {
				route := cluster.Network[i].PrepareRoute()
				// check if route we are about to delete is not referenced by more than one service
				if sm.countRouteReferences(route) <= 1 {
					err := cluster.Network[i].DeleteRoute()
					if err != nil && !errors.Is(err, syscall.ESRCH) {
						log.Error("failed to delete route", "ip", cluster.Network[i].IP(), "err", err)
						errs = append(errs, err)
					}
					log.Debug("deleted route", "ip",
						cluster.Network[i].IP(), "service name", service.Name, "namespace", service.Namespace, "interface", cluster.Network[i].Interface(), "tableID", sm.config.RoutingTableID)
				}
			}
		}
	}
	return errs
}

func (sm *Manager) clearBGPHosts(service *v1.Service) {
	if instance := sm.findServiceInstance(service); instance != nil {
		for _, cluster := range instance.Clusters {
			for i := range cluster.Network {
				network := cluster.Network[i]
				err := sm.bgpServer.DelHost(network.CIDR())
				if err != nil {
					log.Error("[endpoint] error deleting BGP host", "err", err)
				} else {
					log.Debug("[endpoint] deleted BGP host", "ip",
						network.CIDR(), "service name", service.Name, "namespace", service.Namespace)
				}
			}
		}
	}
}

func storeConfiguredNetwork(svcUID string, network vip.Network) {
	if _, exists := configuredNetworks[svcUID]; !exists {
		configuredNetworks[svcUID] = make(map[string]vip.Network)
	}

	configuredNetworks[svcUID][network.IP()] = network
}
