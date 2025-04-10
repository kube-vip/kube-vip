package manager

import (
	"context"
	"errors"
	"fmt"
	"syscall"

	log "log/slog"

	v1 "k8s.io/api/core/v1"
)

type RoutingTable struct {
	Generic
}

func NewRoutingTable(sm *Manager, provider epProvider) EndpointWorker {
	return &RoutingTable{
		Generic: Generic{
			sm:       sm,
			provider: provider,
		},
	}
}

func (rt *RoutingTable) ProcessInstance(ctx context.Context, service *v1.Service, leaderElectionActive *bool) error {
	instance, err := rt.sm.findServiceInstanceWithTimeout(ctx, service)
	if err != nil {
		log.Error("error finding instance", "service", service.UID, "provider", rt.provider.getLabel(), "err", err)
	}
	if instance != nil {
		for _, cluster := range instance.clusters {
			for i := range cluster.Network {
				err := cluster.Network[i].AddRoute(false)
				if err != nil {
					if errors.Is(err, syscall.EEXIST) {
						// If route exists try to update it if necessary
						isUpdated, err := cluster.Network[i].UpdateRoutes()
						if err != nil {
							return fmt.Errorf("[%s] error updating existing routes: %w", rt.provider.getLabel(), err)
						}
						if isUpdated {
							log.Debug("updated route", "provider", rt.provider.getLabel(), "ip", cluster.Network[i].IP())
						}
					} else {
						// If other error occurs, return error
						return fmt.Errorf("[%s] error adding route: %s", rt.provider.getLabel(), err.Error())
					}
				} else {
					log.Info("added route", "provider",
						rt.provider.getLabel(), "ip", cluster.Network[i].IP(), "service name", service.Name, "namespace", service.Namespace, "interface", cluster.Network[i].Interface(), "tableID", rt.sm.config.RoutingTableID)
					configuredLocalRoutes.Store(string(service.UID), true)
					*leaderElectionActive = true
				}
			}
		}
	}

	return nil
}

func (rt *RoutingTable) Clear(lastKnownGoodEndpoint *string, service *v1.Service, cancel context.CancelFunc, leaderElectionActive *bool) {
	if !rt.sm.config.EnableServicesElection && !rt.sm.config.EnableLeaderElection {
		// If routing table mode is enabled - routes should be deleted
		if errs := rt.sm.clearRoutes(service); len(errs) == 0 {
			configuredLocalRoutes.Store(string(service.UID), false)
		} else {
			for _, err := range errs {
				log.Error("error while clearing routes", "err", err)
			}
		}
	}

	rt.clearEgress(lastKnownGoodEndpoint, service, cancel, leaderElectionActive)
}

func (rt *RoutingTable) GetEndpoints(service *v1.Service, id string) ([]string, error) {
	return rt.getAllEndpoints(service, id)
}

func (rt *RoutingTable) RemoveEgress(service *v1.Service, lastKnownGoodEndpoint *string) {
	if err := rt.sm.TeardownEgress(*lastKnownGoodEndpoint, service.Spec.LoadBalancerIP,
		service.Namespace, service.Annotations); err != nil {
		log.Warn("removing redundant egress rules", "err", err)
	}
}

func (rt *RoutingTable) Delete(service *v1.Service, id string) error {
	// When no-leader-elecition mode
	if !rt.sm.config.EnableServicesElection && !rt.sm.config.EnableLeaderElection {
		// find all existing local endpoints
		endpoints, err := rt.GetEndpoints(service, id)
		if err != nil {
			return fmt.Errorf("[%s] error getting endpoints: %w", rt.provider.getLabel(), err)
		}

		// If there were local endpoints deleted
		if len(endpoints) > 0 {
			rt.deleteAction(service)
		}
	}

	return nil
}

func (rt *RoutingTable) deleteAction(service *v1.Service) {
	rt.sm.clearRoutes(service)
}

func (sm *Manager) clearRoutes(service *v1.Service) []error {
	errs := []error{}
	if instance := sm.findServiceInstance(service); instance != nil {
		for _, cluster := range instance.clusters {
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

func (rt *RoutingTable) SetInstanceEndpointsStatus(service *v1.Service, state bool) error {
	instance := rt.sm.findServiceInstance(service)
	if instance == nil {
		return fmt.Errorf("failed to find instance for service %s/%s", service.Namespace, service.Name)
	}
	instance.HasEndpoints = state
	return nil
}
