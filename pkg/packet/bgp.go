package packet

import (
	"fmt"

	"github.com/packethost/packngo"
	"github.com/plunder-app/kube-vip/pkg/bgp"
	"github.com/plunder-app/kube-vip/pkg/kubevip"
)

// BGPLookup will use the Packet API functions to populate the BGP information
func BGPLookup(c *packngo.Client, k *kubevip.Config) error {

	proj := findProject(k.PacketProject, c)
	if proj == nil {
		return fmt.Errorf("Unable to find Project [%s]", k.PacketProject)
	}
	thisDevice := findSelf(c, proj.ID)
	if thisDevice == nil {
		return fmt.Errorf("Unable to find local/this device in packet API")
	}

	fmt.Printf("Querying BGP settings for [%s]", thisDevice.Hostname)
	neighbours, _, _ := c.Devices.ListBGPNeighbors(thisDevice.ID, &packngo.ListOptions{})
	if len(neighbours) > 1 {
		return fmt.Errorf("There are [%d] neighbours, only designed to manage one", len(neighbours))
	}
	k.BGPConfig.RouterID = neighbours[0].CustomerIP
	k.BGPConfig.AS = uint32(neighbours[0].CustomerAs)

	// Add the peer
	k.BGPConfig.Peers = []bgp.Peer{
		{
			Address: neighbours[0].PeerIps[0],
			AS:      uint32(neighbours[0].PeerAs),
		},
	}

	return nil
}
