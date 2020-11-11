package cluster

import (
	"github.com/plunder-app/kube-vip/pkg/kubevip"
	"github.com/plunder-app/kube-vip/pkg/vip"
)

const leaderLogcount = 5

// Cluster - The Cluster object manages the state of the cluster for a particular node
type Cluster struct {
	stateMachine FSM
	stop         chan bool
	completed    chan bool
	Network      vip.Network
}

// InitCluster - Will attempt to initialise all of the required settings for the cluster
func InitCluster(c *kubevip.Config, disableVIP bool) (*Cluster, error) {

	// TODO - Check for root (needed to netlink)
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

	network, err := vip.NewConfig(address, c.Interface, c.DDNS)
	if err != nil {
		return nil, err
	}
	return network, nil
}
