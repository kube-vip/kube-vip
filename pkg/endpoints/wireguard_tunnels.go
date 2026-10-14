package endpoints

import (
	"errors"
	"fmt"
	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/utils"
	"github.com/kube-vip/kube-vip/pkg/wireguard"
	v1 "k8s.io/api/core/v1"
)

type tunnelConfigProvider interface {
	GetConfigForVIP(vip string) *wireguard.TunnelConfig
}

type wireguardTunnelReleaser interface {
	ReleaseTunnelForVIP(vip, owner string) error
}

// AcquireWireguardServiceTunnels brings up every configured tunnel for one
// active Service. The Service UID makes repeated activation idempotent and lets
// cleanup release only this Service's claim on a shared VIP.
func AcquireWireguardServiceTunnels(tunnelMgr wireguard.ServiceTunnelManager, service *v1.Service) error {
	if tunnelMgr == nil {
		return fmt.Errorf("WireGuard tunnel manager not configured")
	}
	if service == nil {
		return fmt.Errorf("cannot acquire WireGuard tunnels for nil Service")
	}

	serviceIPs, err := utils.FetchServiceIPs(service)
	if err != nil {
		return fmt.Errorf("get WireGuard VIPs for Service %s/%s: %w", service.Namespace, service.Name, err)
	}
	for _, serviceIP := range serviceIPs {
		if !tunnelMgr.HasConfigForVIP(serviceIP) {
			return fmt.Errorf("no WireGuard tunnel configuration found for VIP %s", serviceIP)
		}
	}

	owner := string(service.UID)
	acquired := make([]string, 0, len(serviceIPs))
	for _, serviceIP := range serviceIPs {
		if err := tunnelMgr.AcquireTunnelForVIP(serviceIP, owner); err != nil {
			acquireErr := fmt.Errorf("bring up WireGuard tunnel for VIP %s: %w", serviceIP, err)
			return errors.Join(acquireErr, rollbackWireguardServiceTunnels(tunnelMgr, acquired, owner))
		}
		acquired = append(acquired, serviceIP)
	}
	return nil
}

func rollbackWireguardServiceTunnels(tunnelMgr wireguard.ServiceTunnelManager, serviceIPs []string, owner string) error {
	var rollbackErrors []error
	for index := len(serviceIPs) - 1; index >= 0; index-- {
		if err := tunnelMgr.ReleaseTunnelForVIP(serviceIPs[index], owner); err != nil {
			rollbackErrors = append(rollbackErrors,
				fmt.Errorf("roll back WireGuard tunnel for VIP %s: %w", serviceIPs[index], err))
		}
	}
	return errors.Join(rollbackErrors...)
}

func releaseWireguardServiceTunnels(tunnelMgr wireguardTunnelReleaser, service *v1.Service) {
	serviceIPs, _ := utils.FetchServiceIPs(service)
	for _, serviceIP := range serviceIPs {
		if err := tunnelMgr.ReleaseTunnelForVIP(serviceIP, string(service.UID)); err != nil {
			log.Error("[wireguard] failed to tear down tunnel", "service", service.Name, "vip", serviceIP, "err", err)
		}
	}
}
