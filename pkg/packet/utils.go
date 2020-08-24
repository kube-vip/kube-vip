package packet

import (
	"os"

	"github.com/packethost/packngo"
	log "github.com/sirupsen/logrus"
)

func findProject(project string, c *packngo.Client) *packngo.Project {
	l := &packngo.ListOptions{Includes: []string{project}}
	ps, _, err := c.Projects.List(l)
	if err != nil {
		log.Error(err)
	}
	for _, p := range ps {

		// Find our project
		if p.Name == project {
			return &p
		}
	}
	return nil
}

func findSelf(c *packngo.Client, projectID string) *packngo.Device {
	// Go through devices
	dev, _, _ := c.Devices.List(projectID, &packngo.ListOptions{})
	for _, d := range dev {
		me, _ := os.Hostname()
		if me == d.Hostname {
			return &d
		}
	}
	return nil
}
