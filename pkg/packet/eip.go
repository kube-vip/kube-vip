package packet

import (
	"fmt"
	"path"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/packethost/packngo"
	log "github.com/sirupsen/logrus"
)

// AttachEIP will use the packet APIs to move an EIP and attach to a host
func AttachEIP(c *packngo.Client, k *kubevip.Config, hostname string) error {

	// Find our project
	proj := findProject(k.MetalProject, c)
	if proj == nil {
		return fmt.Errorf("Unable to find Project [%s]", k.MetalProject)
	}

	ips, _, _ := c.ProjectIPs.List(proj.ID, &packngo.ListOptions{})
	for _, ip := range ips {

		// Find the device id for our EIP
		if ip.Address == k.VIP {
			log.Infof("Found EIP ->%s ID -> %s\n", ip.Address, ip.ID)
			// If attachements already exist then remove them
			if len(ip.Assignments) != 0 {
				hrefID := path.Base(ip.Assignments[0].Href)
				c.DeviceIPs.Unassign(hrefID)
			}
		}
	}

	// Lookup this server through the packet API
	thisDevice := findSelf(c, proj.ID)
	if thisDevice == nil {
		return fmt.Errorf("Unable to find local/this device in packet API")
	}

	// Assign the EIP to this device
	log.Infof("Assigning EIP to -> %s\n", thisDevice.Hostname)
	_, _, err := c.DeviceIPs.Assign(thisDevice.ID, &packngo.AddressStruct{
		Address: k.VIP,
	})
	if err != nil {
		return err
	}

	return nil
}
