package kubevip

import (
	"fmt"
	"io/ioutil"
	"os"
	"strconv"
	"strings"

	"github.com/ghodss/yaml"
	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
)

//ParseBackendConfig -
func ParseBackendConfig(ep string) (*BackEnd, error) {
	endpoint := strings.Split(ep, ":")
	if len(endpoint) != 2 {
		return nil, fmt.Errorf("ensure a backend is in in the format address:port, e.g. 10.0.0.1:8080")
	}
	p, err := strconv.Atoi(endpoint[1])
	if err != nil {
		return nil, err
	}
	return &BackEnd{Address: endpoint[0], Port: p}, nil
}

//OpenConfig will attempt to read a file and parse it's contents into a configuration
func OpenConfig(path string) (*Config, error) {
	if path == "" {
		return nil, fmt.Errorf("path cannot be blank")
	}

	log.Infof("Reading configuration from [%s]", path)

	// Check the actual path from the string
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		// Attempt to read the data
		configData, err := ioutil.ReadFile(path)
		if err != nil {
			return nil, err
		}

		// If data is read successfully parse the yaml
		var c Config
		err = yaml.Unmarshal(configData, &c)
		if err != nil {
			return nil, err
		}
		return &c, nil

	}
	return nil, fmt.Errorf("error reading [%s]", path)
}

//PrintConfig - will print out an instance of the kubevip config
func (c *Config) PrintConfig() {
	b, _ := yaml.Marshal(c)

	fmt.Print(string(b))
}

//SampleConfig will create an example configuration and write it to the specified [path]
func SampleConfig() {

	// Generate Sample configuration
	c := &Config{

		// Virtual IP address
		VIP: "192.168.0.100",
		// Interface to bind to
		Interface: "eth0",
		// Load Balancer Configuration
		LoadBalancers: []LoadBalancer{
			{
				Name:      "Kubernetes Control Plane",
				Type:      "http",
				Port:      6443,
				BindToVip: true,
				Backends: []BackEnd{
					{
						Address: "192.168.0.100",
						Port:    6443,
					},
					{
						Address: "192.168.0.101",
						Port:    6443,
					},
					{
						Address: "192.168.0.102",
						Port:    6443,
					},
				},
			},
		},
	}
	b, _ := yaml.Marshal(c)

	fmt.Print(string(b))
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

func (c *Config) CheckInterface() error {
	if c.Interface != "" {
		if err := isValidInterface(c.Interface); err != nil {
			return fmt.Errorf("%s is not valid interface, reason: %w", c.Interface, err)
		}
	}

	if c.ServicesInterface != "" {
		if err := isValidInterface(c.ServicesInterface); err != nil {
			return fmt.Errorf("%s is not valid interface, reason: %w", c.ServicesInterface, err)
		}
	}

	return nil
}

func isValidInterface(iface string) error {
	l, err := netlink.LinkByName(iface)
	if err != nil {
		return fmt.Errorf("get %s failed, error: %w", iface, err)
	}
	attrs := l.Attrs()

	// Some interfaces (included but not limited to lo and point-to-point
	//	interfaces) do not provide a operational status but are safe to use.
	// From kernek.org: "Interface is in unknown state, neither driver nor
	// userspace has set operational state. Interface must be considered for user
	// data as setting operational state has not been implemented in every driver."
	if attrs.OperState == netlink.OperUnknown {
		log.Warningf(
			"the status of the interface %s is unknown. Ensure your interface is ready to accept traffic, if so you can safely ignore this message",
			iface,
		)
	} else if attrs.OperState != netlink.OperUp {
		return fmt.Errorf("%s is not up", iface)
	}

	return nil
}
