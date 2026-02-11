package cluster

import (
	"context"
	"sync"

	"github.com/kube-vip/kube-vip/pkg/vip"
)

// StartDDNS should start go routine for dhclient to hold the lease for the IP
// StartDDNS should wait until IP is allocated from DHCP, set it to cluster.Network
// so the OnStartedLeading can continue to configure the VIP initially
// during runtime if IP changes, startDDNS don't have to do reconfigure because
// dnsUpdater already have the functionality to keep trying resolve the IP
// and update the VIP configuration if it changes
func (cluster *Cluster) StartDDNS(ctx context.Context, network vip.Network, backoffAttempts uint, wg *sync.WaitGroup) error {
	ddnsMgr := vip.NewDDNSManager(network, backoffAttempts)
	ip, err := ddnsMgr.Start(ctx, wg)
	if err != nil {
		return err
	}
	if err = network.SetIP(ip); err != nil {
		return err
	}

	return nil
}
