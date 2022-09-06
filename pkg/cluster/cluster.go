package cluster

import (
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/vip"
	log "github.com/sirupsen/logrus"
)

// Cluster - The Cluster object manages the state of the cluster for a particular node
type Cluster struct {
	stop      chan bool
	completed chan bool
	Network   vip.Network
}

// InitCluster - Will attempt to initialise all of the required settings for the cluster
func InitCluster(c *kubevip.Config, disableVIP bool) (*Cluster, error) {
	var network vip.Network
	var err error

	if !disableVIP {
		// Start the Virtual IP Networking configuration
		network, err = startNetworking(c)
		if err != nil {
			return nil, err
		}
	}
	// Initialise the Cluster structure
	newCluster := &Cluster{
		Network: network,
	}

	return newCluster, nil
}

func startNetworking(c *kubevip.Config) (vip.Network, error) {
	address := c.VIP

	if c.Address != "" {
		address = c.Address
	}

	network, err := vip.NewConfig(address, c.Interface, c.VIPSubnet, c.DDNS, c.RoutingTableID)
	if err != nil {
		return nil, err
	}

	return network, nil
}

// Stop - Will stop the Cluster and release VIP if needed
func (cluster *Cluster) Stop() {
	// Close the stop chanel, which will shut down the VIP (if needed)
	if cluster.stop != nil {
		close(cluster.stop)
	}

	// Wait until the completed channel is closed, signalling all shutdown tasks completed
	<-cluster.completed

	log.Info("Stopped")
}
