package endpoints

import (
	"context"
	"errors"
	"fmt"
	"syscall"

	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/egress"
	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/vishvananda/netlink"
	v1 "k8s.io/api/core/v1"
)

type RoutingTable struct {
	generic
}

func newRoutingTable(generic generic) endpointWorker {
	return &RoutingTable{
		generic: generic,
	}
}

func (rt *RoutingTable) processInstance(ctx context.Context, service *v1.Service, leaderElectionActive *bool) error {
	instance, err := instance.FindServiceInstanceWithTimeout(ctx, service, *rt.instances)
	if err != nil {
		log.Error("error finding instance", "service", service.UID, "provider", rt.provider.GetLabel(), "err", err)
	}
	if instance != nil {
		for _, cluster := range instance.Clusters {
			for i := range cluster.Network {
				err := cluster.Network[i].AddRoute(false)
				if err != nil {
					if errors.Is(err, syscall.EEXIST) {
						// If route exists try to update it if necessary
						isUpdated, err := cluster.Network[i].UpdateRoutes()
						if err != nil {
							return fmt.Errorf("[%s] error updating existing routes: %w", rt.provider.GetLabel(), err)
						}
						if isUpdated {
							log.Debug("updated route", "provider", rt.provider.GetLabel(), "ip", cluster.Network[i].IP())
						}
					} else {
						// If other error occurs, return error
						return fmt.Errorf("[%s] error adding route: %s", rt.provider.GetLabel(), err.Error())
					}
				} else {
					log.Info("added route", "provider",
						rt.provider.GetLabel(), "ip", cluster.Network[i].IP(), "service name", service.Name, "namespace", service.Namespace, "interface", cluster.Network[i].Interface(), "tableID", rt.config.RoutingTableID)
					rt.configuredLocalRoutes.Store(string(service.UID), true)
					*leaderElectionActive = true
				}
			}
		}
	}

	return nil
}

func (rt *RoutingTable) clear(lastKnownGoodEndpoint *string, service *v1.Service, cancel context.CancelFunc, leaderElectionActive *bool) {
	if !rt.config.EnableServicesElection && !rt.config.EnableLeaderElection {
		// If routing table mode is enabled - routes should be deleted
		if errs := ClearRoutes(service, rt.instances); len(errs) == 0 {
			rt.configuredLocalRoutes.Store(string(service.UID), false)
		} else {
			for _, err := range errs {
				log.Error("error while clearing routes", "err", err)
			}
		}
	}

	rt.clearEgress(lastKnownGoodEndpoint, service, cancel, leaderElectionActive)
}

func (rt *RoutingTable) getEndpoints(service *v1.Service, id string) ([]string, error) {
	return rt.getAllEndpoints(service, id)
}

func (rt *RoutingTable) removeEgress(service *v1.Service, lastKnownGoodEndpoint *string) {
	if err := egress.Teardown(*lastKnownGoodEndpoint, service.Spec.LoadBalancerIP,
		service.Namespace, service.Annotations, rt.config.EgressWithNftables); err != nil {
		log.Warn("removing redundant egress rules", "err", err)
	}
}

func (rt *RoutingTable) delete(service *v1.Service, id string) error {
	// When no-leader-elecition mode
	if !rt.config.EnableServicesElection && !rt.config.EnableLeaderElection {
		// find all existing local endpoints
		endpoints, err := rt.getEndpoints(service, id)
		if err != nil {
			return fmt.Errorf("[%s] error getting endpoints: %w", rt.provider.GetLabel(), err)
		}

		// If there were local endpoints deleted
		if len(endpoints) > 0 {
			rt.deleteAction(service)
		}
	}

	return nil
}

func (rt *RoutingTable) deleteAction(service *v1.Service) {
	ClearRoutes(service, rt.instances)
}

func ClearRoutes(service *v1.Service, instances *[]*instance.Instance) []error {
	errs := []error{}
	if instance := instance.FindServiceInstance(service, *instances); instance != nil {
		for _, cluster := range instance.Clusters {
			for i := range cluster.Network {
				route := cluster.Network[i].PrepareRoute()
				// check if route we are about to delete is not referenced by more than one service
				if CountRouteReferences(route, instances) <= 1 {
					err := cluster.Network[i].DeleteRoute()
					if err != nil && !errors.Is(err, syscall.ESRCH) {
						log.Error("failed to delete route", "ip", cluster.Network[i].IP(), "err", err)
						errs = append(errs, err)
					}
					log.Debug("deleted route", "ip",
						cluster.Network[i].IP(), "service name", service.Name, "namespace", service.Namespace, "interface", cluster.Network[i].Interface())
				}
			}
		}
	}
	return errs
}

func CountRouteReferences(route *netlink.Route, instances *[]*instance.Instance) int {
	cnt := 0
	for _, instance := range *instances {
		if instance.HasEndpoints {
			for _, cluster := range instance.Clusters {
				for n := range cluster.Network {
					r := cluster.Network[n].PrepareRoute()
					if r.Dst.String() == route.Dst.String() {
						cnt++
					}
				}
			}
		}
	}
	return cnt
}
