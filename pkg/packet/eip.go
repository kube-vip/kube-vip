package packet

import (
	"fmt"
	"strings"

	"github.com/packethost/packngo"
	"github.com/plunder-app/kube-vip/pkg/kubevip"
	log "github.com/sirupsen/logrus"
)

// AttachEIP will use the packet APIs to move an EIP and attach to a host
func AttachEIP(c *packngo.Client, k *kubevip.Config, hostname string) error {

	// Find our project
	proj := findProject(k.PacketProject, c)
	if proj == nil {
		return fmt.Errorf("Unable to find Project [%s]", k.PacketProject)
	}

	ips, _, _ := c.ProjectIPs.List(proj.ID, &packngo.ListOptions{})
	for _, ip := range ips {

		// Find the device id for our EIP
		if ip.Address == k.VIP {
			log.Infof("Found EIP ->%s ID -> %s\n", ip.Address, ip.ID)
			// If attachements already exist then remove them
			if len(ip.Assignments) != 0 {
				hrefID := strings.Replace(ip.Assignments[0].Href, "/ips/", "", -1)
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
