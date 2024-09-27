package cluster

import (
	"sync"

	log "github.com/sirupsen/logrus"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/vip"
)

// Cluster - The Cluster object manages the state of the cluster for a particular node
type Cluster struct {
	stop      chan bool
	completed chan bool
	once      sync.Once
	Network   []vip.Network
}

// InitCluster - Will attempt to initialise all of the required settings for the cluster
func InitCluster(c *kubevip.Config, disableVIP bool) (*Cluster, error) {
	var networks []vip.Network
	var err error

	if !disableVIP {
		// Start the Virtual IP Networking configuration
		networks, err = startNetworking(c)
		if err != nil {
			return nil, err
		}
	}
	// Initialise the Cluster structure
	newCluster := &Cluster{
		Network: networks,
	}

	log.Debugf("init enable service security: %t", c.EnableServiceSecurity)

	return newCluster, nil
}

func startNetworking(c *kubevip.Config) ([]vip.Network, error) {
	address := c.VIP

	if c.Address != "" {
		address = c.Address
	}

	addresses := vip.GetIPs(address)

	networks := []vip.Network{}
	for _, addr := range addresses {
		network, err := vip.NewConfig(addr, c.Interface, c.LoInterfaceGlobalScope, c.VIPSubnet, c.DDNS, c.RoutingTableID, c.RoutingTableType, c.RoutingProtocol, c.DNSMode, c.LoadBalancerForwardingMethod, c.IptablesBackend)
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
