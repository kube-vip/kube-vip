package kubevip

import (
	"fmt"
	"io/ioutil"
	"os"

	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v2"
)

var endPointIndex int // Holds the previous endpoint (for determining decisions on next endpoint)

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
		RemotePeers: []RaftPeer{
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
		// Load Balancer Configuration
		LoadBalancers: []LoadBalancer{
			LoadBalancer{
				Name:      "Kubernetes Control Plane",
				Type:      "http",
				Port:      6443,
				BindToVip: true,
				Backends: []BackEnd{
					BackEnd{
						Address: "192.168.0.100",
						Port:    6443,
					},
					BackEnd{
						Address: "192.168.0.101",
						Port:    6443,
					},
					BackEnd{
						Address: "192.168.0.102",
						Port:    6443,
					},
				},
			},
		},
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
