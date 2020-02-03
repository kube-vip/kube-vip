package cluster

import (
	"fmt"
	"io/ioutil"
	"os"

	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v2"
)

// Config defines all of the settings for the Virtual IP / Load-balancer
type Config struct {

	// Peers are all of the peers within the RAFT cluster
	Peers []RaftPeer `yaml:"peers"`

	// LocalPeer is the configuration of this host
	LocalPeer RaftPeer `yaml:"localPeer"`

	// VIP is the Virtual IP address exposed for the cluster
	VIP string `yaml:"vip"`

	// GratuituosArp will broadcast an ARP update when the VIP changes host
	GratuitousARP bool `yaml:"gratuitousARP"`

	// Interface is the network interface to bind to (default: First Adapter)
	Interface string `yaml:"interface,omitempty"`
}

// RaftPeer details the configuration of all cluster peers
type RaftPeer struct {
	// ID is the unique identifier a peer instance
	ID string `yaml:"id"`

	// IP Address of a peer instance
	Address string `yaml:"address"`

	// Listening port of this peer instance
	Port int `yaml:"port"`
}

//OpenConfig will attempt to read a file and parse it's contents into a configuration
func OpenConfig(path string) (*Config, error) {
	if path == "" {
		return nil, fmt.Errorf("Path cannot be blank")
	}

	log.Infof("Reading configuration from [%s]", path)

	// Check the actual path from the string
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		// Attempt to read the data
		configData, err := ioutil.ReadFile(path)
		if err != nil {
			return nil, err
		}

		// If data is read succesfully parse the yaml
		var c Config
		err = yaml.Unmarshal(configData, &c)
		if err != nil {
			return nil, err
		}
		return &c, nil

	}
	return nil, fmt.Errorf("Error reading [%s]", path)
}

//SampleConfig will create an example configuration and write it to the specified [path]
func SampleConfig() {

	// Generate Sample configuration
	c := &Config{
		// Generate sample peers
		Peers: []RaftPeer{
			RaftPeer{
				ID:      "server1",
				Address: "192.168.0.1",
				Port:    10000,
			},
			RaftPeer{
				ID:      "server2",
				Address: "192.168.0.2",
				Port:    10000,
			},
			RaftPeer{
				ID:      "server3",
				Address: "192.168.0.3",
				Port:    10000,
			},
		},
		LocalPeer: RaftPeer{
			ID:      "server1",
			Address: "192.168.0.1",
			Port:    10000,
		},
		// Virtual IP address
		VIP: "192.168.0.100",
		// Interface to bind to
		Interface: "eth0",
	}
	b, _ := yaml.Marshal(c)

	fmt.Printf(string(b))
}

//WriteConfig will write the current configuration to a specified [path]
func (c *Config) WriteConfig(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	bytesWritten, err := f.Write(b)
	if err != nil {
		return err
	}
	log.Debugf("wrote %d bytes\n", bytesWritten)
	return nil
}
