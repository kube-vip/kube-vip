package vip

import (
	"github.com/pkg/errors"
	"github.com/vishvananda/netlink"
)

//VIP -
type VIP interface {
	AddIP() error
	DeleteIP() error
	IsSet() (bool, error)
	IP() string
	Interface() string
}

// Network - This allows network configuration
type Network struct {
	address *netlink.Addr
	link    netlink.Link
}

// NewConfig will attempt to provide an interface to the kernel network configuration
func NewConfig(address, iface string) (result Network, err error) {
	result = Network{}

	result.address, err = netlink.ParseAddr(address + "/32")
	if err != nil {
		err = errors.Wrapf(err, "could not parse address '%s'", address)

		return
	}

	result.link, err = netlink.LinkByName(iface)
	if err != nil {
		err = errors.Wrapf(err, "could not get link for interface '%s'", iface)

		return
	}

	return
}

//AddIP - Add an IP address to the interface
func (configurator Network) AddIP() error {
	result, err := configurator.IsSet()
	if err != nil {
		return errors.Wrap(err, "ip check in AddIP failed")
	}

	// Already set
	if result {
		return nil
	}

	if err = netlink.AddrAdd(configurator.link, configurator.address); err != nil {
		return errors.Wrap(err, "could not add ip")
	}

	return nil
}

//DeleteIP - Remove an IP address from the interface
func (configurator Network) DeleteIP() error {
	result, err := configurator.IsSet()
	if err != nil {
		return errors.Wrap(err, "ip check in DeleteIP failed")
	}

	// Nothing to delete
	if !result {
		return nil
	}

	if err = netlink.AddrDel(configurator.link, configurator.address); err != nil {
		return errors.Wrap(err, "could not delete ip")
	}

	return nil
}

// IsSet - Check to see if VIP is set
func (configurator Network) IsSet() (result bool, err error) {
	var addresses []netlink.Addr

	addresses, err = netlink.AddrList(configurator.link, 0)
	if err != nil {
		err = errors.Wrap(err, "could not list addresses")

		return
	}

	for _, address := range addresses {
		if address.Equal(*configurator.address) {
			return true, nil
		}
	}

	return false, nil
}

// IP - return the IP Address
func (configurator Network) IP() string {
	return configurator.address.IP.String()
}

// Interface - return the Interface name
func (configurator Network) Interface() string {
	return configurator.link.Attrs().Name
}
