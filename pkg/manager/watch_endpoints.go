package manager

import (
	"context"
	"fmt"
	"strings"
	"sync"

	log "github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"
	watchtools "k8s.io/client-go/tools/watch"
	"k8s.io/client-go/util/retry"
)

func (sm *Manager) watchEndpoint(ctx context.Context, id string, service *v1.Service, wg *sync.WaitGroup) error {
	log.Infof("[endpoint] watching for service [%s] in namespace [%s]", service.Name, service.Namespace)
	// Use a restartable watcher, as this should help in the event of etcd or timeout issues
	leaderContext, cancel := context.WithCancel(context.Background())
	var leaderElectionActive bool
	defer cancel()

	opts := metav1.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("metadata.name", service.Name).String(),
	}
	rw, err := watchtools.NewRetryWatcher("1", &cache.ListWatch{
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			return sm.clientSet.CoreV1().Endpoints(service.Namespace).Watch(ctx, opts)
		},
	})
	if err != nil {
		cancel()
		return fmt.Errorf("error creating endpoint watcher: %s", err.Error())
	}

	exitFunction := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			log.Debug("[endpoint] context cancelled")
			// Stop the retry watcher
			rw.Stop()
			// Cancel the context, which will in turn cancel the leadership
			cancel()
			return
		case <-sm.shutdownChan:
			log.Debug("[endpoint] shutdown called")
			// Stop the retry watcher
			rw.Stop()
			// Cancel the context, which will in turn cancel the leadership
			cancel()
			return
		case <-exitFunction:
			log.Debug("[endpoint] function ending")
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

		// We need to inspect the event and get ResourceVersion out of it
		switch event.Type {
		case watch.Added, watch.Modified:
			ep, ok := event.Object.(*v1.Endpoints)
			if !ok {
				cancel()
				return fmt.Errorf("unable to parse Kubernetes services from API watcher")
			}

			// Build endpoints
			var localendpoints []string
			for subset := range ep.Subsets {
				for address := range ep.Subsets[subset].Addresses {
					// 1. Compare the hostname on the endpoint to the hostname
					// 2. Compare the nodename on the endpoint to the hostname
					// 3. Drop the FQDN to a shortname and compare to the nodename on the endpoint

					// 1. Compare the Hostname first (should be FQDN)
					if id == ep.Subsets[subset].Addresses[address].Hostname {
						log.Debugf("[endpoint] address: %s, hostname: %s", ep.Subsets[subset].Addresses[address].IP, ep.Subsets[subset].Addresses[address].Hostname)
						localendpoints = append(localendpoints, ep.Subsets[subset].Addresses[address].IP)
					} else {
						// 2. Compare the Nodename (from testing could be FQDN or short)
						if ep.Subsets[subset].Addresses[address].NodeName != nil {
							log.Debugf("[endpoint] address: %s, hostname: %s, node: %s", ep.Subsets[subset].Addresses[address].IP, ep.Subsets[subset].Addresses[address].Hostname, *ep.Subsets[subset].Addresses[address].NodeName)
							if id == *ep.Subsets[subset].Addresses[address].NodeName {
								localendpoints = append(localendpoints, ep.Subsets[subset].Addresses[address].IP)
							} else {
								// 3. Compare to shortname
								shortname, err := getShortname(id)
								if err != nil {
									log.Errorf("[endpoint] %v", err)
								} else {
									log.Debugf("[endpoint] address: %s, shortname: %s, node: %s", ep.Subsets[subset].Addresses[address].IP, shortname, *ep.Subsets[subset].Addresses[address].NodeName)

									if shortname == *ep.Subsets[subset].Addresses[address].NodeName {
										localendpoints = append(localendpoints, ep.Subsets[subset].Addresses[address].IP)
									}
								}
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
			if len(localendpoints) != 0 {
				// if we haven't populated one, then do so
				if lastKnownGoodEndpoint != "" {

					// check out previous endpoint exists
					stillExists := false

					for x := range localendpoints {
						if localendpoints[x] == lastKnownGoodEndpoint {
							stillExists = true
						}
					}
					// If the last endpoint no longer exists, we cancel our leader Election
					if !stillExists && leaderElectionActive {
						log.Warnf("[endpoint] existing [%s] has been removed, restarting leaderElection", lastKnownGoodEndpoint)
						// Stop the existing leaderElection
						cancel()
						// Set our active endpoint to an existing one
						lastKnownGoodEndpoint = localendpoints[0]
						// disable last leaderElection flag
						leaderElectionActive = false
					}

				} else {
					lastKnownGoodEndpoint = localendpoints[0]
				}

				// Set the service accordingly
				if service.Annotations["kube-vip.io/egress"] == "true" {
					service.Annotations["kube-vip.io/active-endpoint"] = lastKnownGoodEndpoint
				}

				if !leaderElectionActive && sm.config.EnableServicesElection {
					go func() {
						leaderContext, cancel = context.WithCancel(context.Background())

						// This is a blocking function, that will restart (in the event of failure)
						for {
							// if the context isn't cancelled restart
							if leaderContext.Err() != context.Canceled {
								leaderElectionActive = true
								err = sm.StartServicesLeaderElection(leaderContext, service, wg)
								if err != nil {
									log.Error(err)
								}
								leaderElectionActive = false
							} else {
								leaderElectionActive = false
								break
							}
						}
					}()
				}
			} else {
				// If there are no local endpoints, and we had one then remove it and stop the leaderElection
				if lastKnownGoodEndpoint != "" {
					log.Warnf("[endpoint] existing [%s] has been removed, no remaining endpoints for leaderElection", lastKnownGoodEndpoint)
					lastKnownGoodEndpoint = "" // reset endpoint
					cancel()                   // stop services watcher
					leaderElectionActive = false
				}
			}
			log.Debugf("[endpoint watcher] local endpoint(s) [%d], known good [%s], active election [%t]", len(localendpoints), lastKnownGoodEndpoint, leaderElectionActive)

		case watch.Deleted:
			// Close the goroutine that will end the retry watcher, then exit the endpoint watcher function
			close(exitFunction)
			log.Infof("[endpoints] deleted stopping watching for [%s] in namespace [%s]", service.Name, service.Namespace)
			return nil
		case watch.Error:
			errObject := apierrors.FromObject(event.Object)
			statusErr, _ := errObject.(*apierrors.StatusError)
			log.Errorf("[endpoint] -> %v", statusErr)
		}
	}
	close(exitFunction)
	log.Infof("[endpoints] stopping watching for [%s] in namespace [%s]", service.Name, service.Namespace)
	return nil //nolint:govet
}

func (sm *Manager) updateServiceEndpointAnnotation(endpoint string, service *v1.Service) error {
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

		currentServiceCopy.Annotations["kube-vip.io/active-endpoint"] = endpoint

		_, err = sm.clientSet.CoreV1().Services(currentService.Namespace).Update(context.TODO(), currentServiceCopy, metav1.UpdateOptions{})
		if err != nil {
			log.Errorf("Error updating Service Spec [%s] : %v", currentServiceCopy.Name, err)
			return err
		}
		return nil
	})

	if retryErr != nil {
		log.Errorf("Failed to set Services: %v", retryErr)
		return retryErr
	}
	return nil
}

// returns just the shortname (or first bit) of a FQDN
func getShortname(hostname string) (string, error) {
	if len(hostname) == 0 {
		return "", fmt.Errorf("unable to find shortname from %s", hostname)
	}
	hostParts := strings.Split(hostname, ".")
	if len(hostParts) > 1 {
		return hostParts[0], nil
	}
	return "", fmt.Errorf("unable to find shortname from %s", hostname)
}
