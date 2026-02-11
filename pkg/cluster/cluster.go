package cluster

import (
	"sync"

	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/arp"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/networkinterface"
	"github.com/kube-vip/kube-vip/pkg/vip"
)

// Cluster - The Cluster object manages the state of the cluster for a particular node
type Cluster struct {
	stop      chan bool
	completed chan bool
	once      sync.Once
	Network   []vip.Network
	arpMgr    *arp.Manager
}

// InitCluster - Will attempt to initialise all of the required settings for the cluster
func InitCluster(c *kubevip.Config, disableVIP bool, intfMgr *networkinterface.Manager, arpMgr *arp.Manager) (*Cluster, error) {
	var networks []vip.Network
	var err error

	if !disableVIP {
		// Start the Virtual IP Networking configuration
		networks, err = startNetworking(c, intfMgr)
		if err != nil {
			return nil, err
		}
	}
	// Initialise the Cluster structure
	newCluster := &Cluster{
		Network:   networks,
		arpMgr:    arpMgr,
		completed: make(chan bool),
		stop:      make(chan bool),
	}

	log.Debug("service security", "enabled", c.EnableServiceSecurity)

	return newCluster, nil
}

func startNetworking(c *kubevip.Config, intfMgr *networkinterface.Manager) ([]vip.Network, error) {
	address := c.VIP

	if c.Address != "" {
		address = c.Address
	}

	addresses := vip.Split(address)

	networks := []vip.Network{}
	for _, addr := range addresses {
		network, err := vip.NewConfig(addr, c.Interface, c.LoInterfaceGlobalScope, c.VIPSubnet, c.DDNS, c.DHCPMode,
			c.RequireDualStack, c.IsDualStack, c.RoutingTableID, c.RoutingTableType, c.RoutingProtocol, c.DNSMode,
			c.LoadBalancerForwardingMethod, c.IptablesBackend, c.EnableLoadBalancer, c.EnableServiceSecurity, intfMgr)
		if err != nil {
			return nil, err
		}
		networks = append(networks, network...)
	}

	return networks, nil
}

// Stop - Will stop the Cluster and release VIP if needed
func (cluster *Cluster) Stop() {
	// Close the stop channel, which will shut down the VIP (if needed)
	if cluster.stop != nil {
		cluster.once.Do(func() { // Ensure that the close channel can only ever be called once
			close(cluster.stop)
		})
	}

	// Wait until the completed channel is closed, signallign all shutdown tasks completed
	<-cluster.completed
}
