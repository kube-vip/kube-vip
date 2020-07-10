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
	network      *vip.Network
}

// InitCluster - Will attempt to initialise all of the required settings for the cluster
func InitCluster(c *kubevip.Config, disableVIP bool) (*Cluster, error) {

	// TODO - Check for root (needed to netlink)
	var network *vip.Network
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
		network: network,
	}

	return newCluster, nil
}

func startNetworking(c *kubevip.Config) (*vip.Network, error) {
	network, err := vip.NewConfig(c.VIP, c.Interface)
	if err != nil {
		// log.WithFields(log.Fields{"error": err}).Error("Network failure")

		// os.Exit(-1)
		return nil, err
	}
	return &network, nil
}
