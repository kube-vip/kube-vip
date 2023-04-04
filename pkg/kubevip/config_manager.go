package kubevip

import (
	"fmt"

	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
	"sigs.k8s.io/yaml"
)

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
