package endpoints

import (
	"fmt"
	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/bgp"
	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	v1 "k8s.io/api/core/v1"
)

type BGP struct {
	generic
	bgpServer *bgp.Server
}

func newBGP(generic generic, bgpServer *bgp.Server) endpointWorker {
	return &BGP{
		generic:   generic,
		bgpServer: bgpServer,
	}
}

func (b *BGP) processInstance(svcCtx *servicecontext.Context, service *v1.Service) error {
	if instance := instance.FindServiceInstance(service, *b.instances); instance != nil {
		for _, cluster := range instance.Clusters {
			for i := range cluster.Network {
				if !svcCtx.IsNetworkConfigured(cluster.Network[i].IP()) {
					log.Debug("attempting to advertise BGP service", "provider", b.provider.GetLabel(), "ip", cluster.Network[i].IP())
					err := b.bgpServer.AddHost(cluster.Network[i].CIDR())
					if err != nil {
						log.Error("error adding BGP host", "provider", b.provider.GetLabel(), "err", err)
					} else {
						log.Info("added BGP host", "provider",
							b.provider.GetLabel(), "ip", cluster.Network[i].CIDR(), "service name", service.Name, "namespace", service.Namespace)
						svcCtx.ConfiguredNetworks.Store(cluster.Network[i].IP(), true)
					}
				}
			}
		}
	}
	return nil
}

func (b *BGP) clear(svcCtx *servicecontext.Context, lastKnownGoodEndpoint *string, service *v1.Service) {
	if !b.config.EnableServicesElection && !b.config.EnableLeaderElection {
		// If BGP mode is enabled - routes should be deleted
		if instance := instance.FindServiceInstance(service, *b.instances); instance != nil {
			for _, cluster := range instance.Clusters {
				for i := range cluster.Network {
					err := b.bgpServer.DelHost(cluster.Network[i].CIDR())
					if err != nil {
						log.Error("deleting BGP host", "provider", b.provider.GetLabel(), "ip", cluster.Network[i].IP(), "err", err)
					} else {
						log.Info("deleted BGP host", "provider",
							b.provider.GetLabel(), "ip", cluster.Network[i].IP(), "service name", service.Name, "namespace", service.Namespace)
						svcCtx.ConfiguredNetworks.Delete(cluster.Network[i].IP())
					}
				}
			}

		}
	}

	b.clearEgress(lastKnownGoodEndpoint, service, svcCtx.Cancel)
}

func (b *BGP) getEndpoints(service *v1.Service, id string) ([]string, error) {
	return b.getAllEndpoints(service, id)
}

func (b *BGP) delete(service *v1.Service, id string) error {
	// When no-leader-elecition mode
	if !b.config.EnableServicesElection && !b.config.EnableLeaderElection {
		// find all existing local endpoints
		endpoints, err := b.getEndpoints(service, id)
		if err != nil {
			return fmt.Errorf("[%s] error getting endpoints: %w", b.provider.GetLabel(), err)
		}

		// If there were local endpoints deleted
		if len(endpoints) > 0 {
			b.deleteAction(service)
		}
	}

	return nil
}

func (b *BGP) deleteAction(service *v1.Service) {
	b.clearBGPHosts(service)
}

func (b *BGP) clearBGPHosts(service *v1.Service) {
	ClearBGPHosts(service, b.instances, b.bgpServer)
}

func (b *BGP) setInstanceEndpointsStatus(_ *v1.Service, _ []string) error {
	return nil
}

func ClearBGPHosts(service *v1.Service, instances *[]*instance.Instance, bgpServer *bgp.Server) {
	if instance := instance.FindServiceInstance(service, *instances); instance != nil {
		ClearBGPHostsByInstance(instance, bgpServer)
	}
}

func ClearBGPHostsByInstance(instance *instance.Instance, bgpServer *bgp.Server) {
	if instance == nil {
		log.Error("failed to clear BGP host for nil instance")
		return
	}
	for _, cluster := range instance.Clusters {
		for i := range cluster.Network {
			network := cluster.Network[i]
			err := bgpServer.DelHost(network.CIDR())
			if err != nil {
				log.Error("[endpoint] error deleting BGP host", "err", err)
			} else {
				log.Debug("[endpoint] deleted BGP host", "ip",
					network.CIDR(), "service name", instance.ServiceSnapshot.Name, "namespace", instance.ServiceSnapshot.Namespace)
			}
		}
	}
}
