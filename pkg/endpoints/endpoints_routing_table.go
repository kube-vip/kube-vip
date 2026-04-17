package endpoints

import (
	"context"
	"fmt"
	"net"

	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/egress"
	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/route"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	v1 "k8s.io/api/core/v1"
)

type RoutingTable struct {
	generic
	routeMgr *route.Manager
}

func newRoutingTable(generic generic, routeMgr *route.Manager) endpointWorker {
	return &RoutingTable{
		generic:  generic,
		routeMgr: routeMgr,
	}
}

func (rt *RoutingTable) processInstance(ctx *servicecontext.Context, service *v1.Service) error {
	instance := instance.FindServiceInstance(service, *rt.instances)
	if instance != nil {
		for _, cluster := range instance.Clusters {
			for i := range cluster.Network {
				if !ctx.IsNetworkConfigured(cluster.Network[i].IP()) && cluster.Network[i].HasEndpoints() {
					if err := rt.routeMgr.Add(lease.ServiceNamespacedName(service), cluster.Network[i], false, true); err != nil {
						return fmt.Errorf("[%s] error adding route: %s", rt.provider.GetLabel(), err.Error())
					} else {
						log.Info("added route", "provider",
							rt.provider.GetLabel(), "ip", cluster.Network[i].IP(), "service name", service.Name, "namespace",
							service.Namespace, "interface", cluster.Network[i].Interface(), "tableID", rt.config.RoutingTableID)
						ctx.ConfiguredNetworks.Store(cluster.Network[i].IP(), true)
					}
				}
			}
		}
	}

	return nil
}

func (rt *RoutingTable) clear(svcCtx *servicecontext.Context, lastKnownGoodEndpoint *string, service *v1.Service) {
	if !rt.config.EnableServicesElection && !rt.config.EnableLeaderElection {
		if errs := ClearRoutes(service, rt.instances, rt.routeMgr); len(errs) == 0 {
			svcCtx.ConfiguredNetworks.Clear()
		} else {
			for _, err := range errs {
				log.Error("error while clearing routes", "err", err)
			}
		}
	}

	rt.clearEgress(lastKnownGoodEndpoint, service)

	if svcCtx.LeaderCancel != nil {
		svcCtx.LeaderCancel()
	}
}

func (rt *RoutingTable) getEndpoints(service *v1.Service, id string) ([]string, error) {
	return rt.getAllEndpoints(service, id)
}

func (rt *RoutingTable) removeEgress(service *v1.Service, lastKnownGoodEndpoint *string) {
	if err := egress.Teardown(*lastKnownGoodEndpoint, service.Spec.LoadBalancerIP,
		service.Namespace, string(service.UID), service.Annotations, rt.config.EgressWithNftables); err != nil {
		log.Warn("removing redundant egress rules", "err", err)
	}
}

func (rt *RoutingTable) delete(_ context.Context, service *v1.Service, id string) error {
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
	ClearRoutes(service, rt.instances, rt.routeMgr)
}

func (rt *RoutingTable) setInstanceEndpointsStatus(service *v1.Service, endpoints []string) error {
	instance := instance.FindServiceInstance(service, *rt.instances)
	if instance == nil {
		log.Error("failed to find the instance", "namespace", service.Namespace, "name", service.Name, "uid", service.UID, "provider", rt.provider.GetLabel())
	} else {
		for _, c := range instance.Clusters {
			for n := range c.Network {
				// if there are no endpoints set HasEndpoints false just in case
				if len(endpoints) < 1 {
					c.Network[n].SetHasEndpoints(false)
				} else {
					// check if endpoint are available and are of same IP family as service
					for _, ep := range endpoints {
						if (net.ParseIP(c.Network[n].IP()).To4() == nil) == (net.ParseIP(ep).To4() == nil) {
							c.Network[n].SetHasEndpoints(true)
							break
						}
					}
				}
			}
		}
	}

	return nil
}

func ClearRoutes(service *v1.Service, instances *[]*instance.Instance, routeMgr *route.Manager) []error {
	errs := []error{}
	if svcInst := instance.FindServiceInstance(service, *instances); svcInst != nil {
		clearErrs := ClearRoutesByInstance(service, svcInst, instances, routeMgr)
		errs = append(errs, clearErrs...)
	}
	return errs
}

func ClearRoutesByInstance(service *v1.Service, svcInst *instance.Instance, instances *[]*instance.Instance, routeMgr *route.Manager) []error {
	if svcInst == nil {
		return []error{fmt.Errorf("failed to remove routes for nil instance of service %s/%s, uid: %s", service.Namespace, service.Name, service.UID)}
	}
	errs := []error{}
	for _, cluster := range svcInst.Clusters {
		for i := range cluster.Network {
			err := routeMgr.Delete(lease.ServiceNamespacedName(service), cluster.Network[i])
			if err != nil {
				log.Error("failed to delete route", "ip", cluster.Network[i].IP(), "err", err)
				errs = append(errs, err)
			}
			log.Debug("deleted route", "ip",
				cluster.Network[i].IP(), "service name", service.Name, "namespace", service.Namespace, "interface", cluster.Network[i].Interface())

		}
	}

	return errs
}
