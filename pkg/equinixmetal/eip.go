package equinixmetal

import (
	"fmt"
	"path"

	log "github.com/gookit/slog"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/packethost/packngo"
)

// AttachEIP will use the Equinix Metal APIs to move an EIP and attach to a host
func AttachEIP(c *packngo.Client, k *kubevip.Config, _ string) error {
	// Use MetalProjectID if it is defined
	projID := k.MetalProjectID

	if projID == "" {
		// Fallback to attempting to find the project by name
		proj := findProject(k.MetalProject, c)
		if proj == nil {
			return fmt.Errorf("unable to find Project [%s]", k.MetalProject)
		}

		projID = proj.ID
	}

	// Prefer Address over VIP
	vip := k.Address
	if vip == "" {
		vip = k.VIP
	}

	ips, _, _ := c.ProjectIPs.List(projID, &packngo.ListOptions{})
	for _, ip := range ips {
		// Find the device id for our EIP
		if ip.Address == vip {
			log.Infof("Found EIP ->%s ID -> %s\n", ip.Address, ip.ID)
			// If attachments already exist then remove them
			if len(ip.Assignments) != 0 {
				hrefID := path.Base(ip.Assignments[0].Href)
				_, err := c.DeviceIPs.Unassign(hrefID)
				if err != nil {
					return fmt.Errorf("unable to unassign deviceIP %q: %v", hrefID, err)
				}
			}
		}
	}

	// Lookup this server through the Equinix Metal API
	thisDevice := findSelf(c, projID)
	if thisDevice == nil {
		return fmt.Errorf("unable to find local/this device in Equinix Metal API")
	}

	// Assign the EIP to this device
	log.Infof("Assigning EIP to -> %s\n", thisDevice.Hostname)
	_, _, err := c.DeviceIPs.Assign(thisDevice.ID, &packngo.AddressStruct{
		Address: vip,
	})
	if err != nil {
		return err
	}

	return nil
}
