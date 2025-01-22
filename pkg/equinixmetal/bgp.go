package equinixmetal

import (
	"fmt"

	"github.com/kube-vip/kube-vip/api/v1alpha1"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/packethost/packngo"
	log "github.com/sirupsen/logrus"
)

// BGPLookup will use the Equinix Metal API functions to populate the BGP information
func BGPLookup(c *packngo.Client, k *kubevip.Config) error {
	var thisDevice *packngo.Device
	if k.MetalProjectID == "" {
		proj := findProject(k.MetalProject, c)
		if proj == nil {
			return fmt.Errorf("Unable to find Project [%s]", k.MetalProject)
		}
		thisDevice = findSelf(c, proj.ID)
	} else {
		thisDevice = findSelf(c, k.MetalProjectID)
	}
	if thisDevice == nil {
		return fmt.Errorf("Unable to find local/this device in Equinix Metal API")
	}

	log.Infof("Querying BGP settings for [%s]", thisDevice.Hostname)
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
	k.BGPConfig.AS = uint32(neighbours[0].CustomerAs) //nolint (being removed)

	// Add the peer(s)
	for x := range neighbours[0].PeerIps {
		peer := v1alpha1.BGPPeer{
			Spec: v1alpha1.BGPPeerSpec{
				Address:  neighbours[0].PeerIps[x],
				AS:       uint32(neighbours[0].PeerAs), //nolint (being removed)
				MultiHop: neighbours[0].Multihop,
				Password: neighbours[0].Md5Password,
			},
		}
		k.BGPConfig.Peers = append(k.BGPConfig.Peers, peer)
	}

	return nil
}
