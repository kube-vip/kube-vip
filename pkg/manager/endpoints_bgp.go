package manager

import (
	"context"
	"fmt"
	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/cluster"
	v1 "k8s.io/api/core/v1"
)

type BGP struct {
	Generic
}

func NewBGP(sm *Manager, provider epProvider) EndpointWorker {
	return &BGP{
		Generic: Generic{
			sm:       sm,
			provider: provider,
		},
	}
}

func (b *BGP) ProcessInstance(svcCtx *serviceContext, service *v1.Service, leaderElectionActive *bool) error {
	if instance := b.sm.findServiceInstance(service); instance != nil {
		for _, cluster := range instance.Clusters {
			for i := range cluster.Network {
				if !svcCtx.isNetworkConfigured(cluster.Network[i].IP()) {
					network := cluster.Network[i]
					log.Debug("attempting to advertise BGP service", "provider", b.provider.getLabel(), "ip", network.CIDR())
					err := b.sm.bgpServer.AddHost(network.CIDR())
					if err != nil {
						log.Error("error adding BGP host", "provider", b.provider.getLabel(), "err", err)
					} else {
						log.Info("added BGP host", "provider",
							b.provider.getLabel(), "ip", network.CIDR(), "service name", service.Name, "namespace", service.Namespace)
						svcCtx.configuredNetworks.Store(cluster.Network[i].IP(), cluster.Network[i])
						*leaderElectionActive = true
					}
				}
			}
		}
	}
	return nil
}

func (b *BGP) Clear(svcCtx *serviceContext, lastKnownGoodEndpoint *string, service *v1.Service, cancel context.CancelFunc, leaderElectionActive *bool) {
	if !b.sm.config.EnableServicesElection && !b.sm.config.EnableLeaderElection {
		// If BGP mode is enabled - routes should be deleted
		if instance := b.sm.findServiceInstance(service); instance != nil {
			for _, cluster := range instance.Clusters {
				for i := range cluster.Network {
					network := cluster.Network[i]
					err := b.sm.bgpServer.DelHost(network.CIDR())
					if err != nil {
						log.Error("[endpoint] deleting BGP host", "provider", b.provider.getLabel(), "ip", network.CIDR(), "err", err)
					} else {
						log.Info("[endpoint] deleted BGP host", "provider",
							b.provider.getLabel(), "ip", network.CIDR(), "service name", service.Name, "namespace", service.Namespace)

						svcCtx.configuredNetworks.Delete(cluster.Network[i].IP())
						*leaderElectionActive = false
					}
				}
			}
		}
	}

	b.clearEgress(lastKnownGoodEndpoint, service, cancel, leaderElectionActive)
}

func (b *BGP) GetEndpoints(service *v1.Service, id string) ([]string, error) {
	return b.getAllEndpoints(service, id)
}

func (b *BGP) Delete(service *v1.Service, id string) error {
	// When no-leader-elecition mode
	if !b.sm.config.EnableServicesElection && !b.sm.config.EnableLeaderElection {
		// find all existing local endpoints
		endpoints, err := b.GetEndpoints(service, id)
		if err != nil {
			return fmt.Errorf("[%s] error getting endpoints: %w", b.provider.getLabel(), err)
		}

		// If there were local endpoints deleted
		if len(endpoints) > 0 {
			b.deleteAction(service)
		}
	}

	return nil
}

func (b *BGP) deleteAction(service *v1.Service) {
	b.sm.ClearBGPHosts(service)
}

func (sm *Manager) ClearBGPHosts(service *v1.Service) {
	if instance := sm.findServiceInstance(service); instance != nil {
		sm.clearBGPHostsByInstance(instance)
	}
}

func (sm *Manager) clearBGPHostsByInstance(instance *cluster.Instance) {
	for _, cluster := range instance.Clusters {
		for i := range cluster.Network {
			// address := fmt.Sprintf("%s/%s", cluster.Network[i].IP(), sm.config.VIPSubnet)
			err := sm.bgpServer.DelHost(cluster.Network[i].CIDR())
			if err != nil {
				log.Error("[endpoint] error deleting BGP host", "err", err)
			} else {
				log.Debug("[endpoint] deleted BGP host", "ip",
					cluster.Network[i].CIDR(), "service name", instance.ServiceSnapshot.Name, "namespace", instance.ServiceSnapshot.Namespace)
			}
		}
	}
}

func (b *BGP) SetInstanceEndpointsStatus(_ *v1.Service, _ []string) error {
	return nil
}
