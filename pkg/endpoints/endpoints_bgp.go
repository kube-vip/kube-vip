package endpoints

import (
	"context"
	"fmt"
	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/bgp"
	"github.com/kube-vip/kube-vip/pkg/instance"
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

func (b *BGP) processInstance(ctx context.Context, service *v1.Service, leaderElectionActive *bool) error {
	instance, err := instance.FindServiceInstanceWithTimeout(ctx, service, *b.instances)
	if err != nil {
		log.Error("error finding instance", "service", service.UID, "provider", b.provider.GetLabel(), "err", err)
	}
	if instance != nil {
		for _, cluster := range instance.Clusters {
			for i := range cluster.Network {
				address := fmt.Sprintf("%s/%s", cluster.Network[i].IP(), b.config.VIPCIDR)
				log.Debug("attempting to advertise BGP service", "provider", b.provider.GetLabel(), "ip", address)
				err := b.bgpServer.AddHost(address)
				if err != nil {
					log.Error("error adding BGP host", "provider", b.provider.GetLabel(), "err", err)
				} else {
					log.Info("added BGP host", "provider",
						b.provider.GetLabel(), "ip", address, "service name", service.Name, "namespace", service.Namespace)
					b.configuredLocalRoutes.Store(string(service.UID), true)
					*leaderElectionActive = true
				}
			}
		}
	}
	return nil
}

func (b *BGP) clear(lastKnownGoodEndpoint *string, service *v1.Service, cancel context.CancelFunc, leaderElectionActive *bool) {
	if !b.config.EnableServicesElection && !b.config.EnableLeaderElection {
		// If BGP mode is enabled - routes should be deleted
		if instance := instance.FindServiceInstance(service, *b.instances); instance != nil {
			for _, cluster := range instance.Clusters {
				for i := range cluster.Network {
					address := fmt.Sprintf("%s/%s", cluster.Network[i].IP(), b.config.VIPCIDR)
					err := b.bgpServer.DelHost(address)
					if err != nil {
						log.Error("deleting BGP host", "provider", b.provider.GetLabel(), "ip", address, "err", err)
					} else {
						log.Info("deleted BGP host", "provider",
							b.provider.GetLabel(), "ip", address, "service name", service.Name, "namespace", service.Namespace)
						b.configuredLocalRoutes.Store(string(service.UID), false)
						*leaderElectionActive = false
					}
				}
			}

		}
	}

	b.clearEgress(lastKnownGoodEndpoint, service, cancel, leaderElectionActive)
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
	b.ClearBGPHosts(service)
}

func (b *BGP) ClearBGPHosts(service *v1.Service) {
	if instance := instance.FindServiceInstance(service, *b.instances); instance != nil {
		for _, cluster := range instance.Clusters {
			for i := range cluster.Network {
				address := fmt.Sprintf("%s/%s", cluster.Network[i].IP(), b.config.VIPCIDR)
				err := b.bgpServer.DelHost(address)
				if err != nil {
					log.Error("[endpoint] error deleting BGP host", "err", err)
				} else {
					log.Debug("[endpoint] deleted BGP host", "ip",
						address, "service name", service.Name, "namespace", service.Namespace)
				}
			}
		}
	}
}

func (b *BGP) setInstanceEndpointsStatus(_ *v1.Service, _ bool) error {
	return nil
}
