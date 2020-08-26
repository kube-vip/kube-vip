package packet

import (
	"fmt"

	"github.com/packethost/packngo"
	"github.com/plunder-app/kube-vip/pkg/bgp"
	"github.com/plunder-app/kube-vip/pkg/kubevip"
	log "github.com/sirupsen/logrus"
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
	neighbours, _, err := c.Devices.ListBGPNeighbors(thisDevice.ID, &packngo.ListOptions{})
	if err != nil {
		return err
	}
	// Ensure neighbours exist (and it's enabled)
	if len(neighbours) == 0 {
		return fmt.Errorf("The server [%s]/[%s] has no BGP neighbours, ensure BGP is enabled", thisDevice.Hostname, thisDevice.ID)
	}

	// Add a warning (TODO)
	if len(neighbours) > 1 {
		log.Warnf("There are [%d] neighbours, only designed to manage one", len(neighbours))
	}

	// Ensure a peer exists
	if len(neighbours[0].PeerIps) == 0 {
		return fmt.Errorf("The server [%s]/[%s] has no BGP peers, ensure BGP is enabled", thisDevice.Hostname, thisDevice.ID)
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
