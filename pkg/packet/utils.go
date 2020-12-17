package packet

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
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

func GetPacketConfig(providerConfig string) (string, string, error) {
	var config struct {
		AuthToken string `json:"apiKey"`
		ProjectID string `json:"projectId"`
	}
	// get our token and project
	if providerConfig != "" {
		configBytes, err := ioutil.ReadFile(providerConfig)
		if err != nil {
			return "", "", fmt.Errorf("failed to get read configuration file at path %s: %v", providerConfig, err)
		}
		err = json.Unmarshal(configBytes, &config)
		if err != nil {
			return "", "", fmt.Errorf("failed to process json of configuration file at path %s: %v", providerConfig, err)
		}
	}
	return config.AuthToken, config.ProjectID, nil
}
