package manager

import (
	"context"
	"errors"
	"fmt"
	"net"
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

func (rt *RoutingTable) ProcessInstance(svcCtx *serviceContext, service *v1.Service, leaderElectionActive *bool) error {
	instance := rt.sm.findServiceInstance(service)
	if instance != nil {
		for _, cluster := range instance.Clusters {
			for i := range cluster.Network {
				if !svcCtx.isNetworkConfigured(cluster.Network[i].IP()) && cluster.Network[i].HasEndpoints() {
					err := cluster.Network[i].AddRoute(false)
					if err != nil {
						if errors.Is(err, syscall.EEXIST) {
							// If route exists, but protocol is not set (e.g. the route was created by the older version
							// of kube-vip) try to update it if necessary
							isUpdated, err := cluster.Network[i].UpdateRoutes()
							if err != nil {
								return fmt.Errorf("[%s] error updating existing routes: %w", rt.provider.getLabel(), err)
							}
							if isUpdated {
								log.Info("updated route", "provider",
									rt.provider.getLabel(), "ip", cluster.Network[i].IP(), "service name", service.Name, "namespace",
									service.Namespace, "interface", cluster.Network[i].Interface(), "tableID", rt.sm.config.RoutingTableID)
							} else {
								log.Info("route already present", "provider",
									rt.provider.getLabel(), "ip", cluster.Network[i].IP(), "service name", service.Name, "namespace",
									service.Namespace, "interface", cluster.Network[i].Interface(), "tableID", rt.sm.config.RoutingTableID)
							}
						} else {
							// If other error occurs, return error
							return fmt.Errorf("[%s] error adding route: %s", rt.provider.getLabel(), err.Error())
						}
					} else {
						log.Info("added route", "provider",
							rt.provider.getLabel(), "ip", cluster.Network[i].IP(), "service name", service.Name, "namespace",
							service.Namespace, "interface", cluster.Network[i].Interface(), "tableID", rt.sm.config.RoutingTableID)
						svcCtx.configuredNetworks.Store(cluster.Network[i].IP(), cluster.Network[i])
						*leaderElectionActive = true
					}
				}
			}
		}
	}

	return nil
}

func (rt *RoutingTable) Clear(svcCtx *serviceContext, lastKnownGoodEndpoint *string, service *v1.Service, cancel context.CancelFunc, leaderElectionActive *bool) {
	if !rt.sm.config.EnableServicesElection && !rt.sm.config.EnableLeaderElection {
		if errs := rt.sm.clearRoutes(service); len(errs) == 0 {
			svcCtx.configuredNetworks.Clear()
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

func (rt *RoutingTable) SetInstanceEndpointsStatus(service *v1.Service, endpoints []string) error {
	instance := rt.sm.findServiceInstance(service)
	if instance == nil {
		return fmt.Errorf("failed to find instance for service %s/%s", service.Namespace, service.Name)
	}
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
	return nil
}
