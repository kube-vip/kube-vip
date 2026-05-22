package utils

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/vishvananda/netlink"
)

// ParseVLANInterface does vlan name validation
// and splits it into the parent interface and tag
func ParseVLANInterface(name string) (string, int, error) {
	vlanParts := strings.Split(name, ".")
	if len(vlanParts) != 2 {
		return "", 0, fmt.Errorf("invalid VLAN name %s, expected format: eth1.300", name)
	}

	parent := vlanParts[0]
	if parent == "" {
		return "", 0, fmt.Errorf("parent interface for VLAN is empty")
	}

	tagStr := vlanParts[1]
	tag, err := strconv.Atoi(tagStr)
	if err != nil {
		return "", 0, fmt.Errorf("invalid VLAN tag %s", tagStr)
	}

	// according to IEEE 802.1Q
	if tag < 1 || tag > 4094 {
		return "", 0, fmt.Errorf("VLAN tag must be between 1 and 4094, got %d", tag)
	}

	return parent, tag, nil
}

// ValidateVLANInterface validates vlans parent index and expected vlan tag
func ValidateVLANInterface(link netlink.Link, parent netlink.Link, expectedID int) error {
	vlan, ok := link.(*netlink.Vlan)
	if !ok {
		return fmt.Errorf("link %s exists but is not a VLAN interface", link.Attrs().Name)
	}

	if vlan.ParentIndex != parent.Attrs().Index {
		return fmt.Errorf(
			"VLAN interface %s has parent index %d, expected %d",
			link.Attrs().Name,
			vlan.ParentIndex,
			parent.Attrs().Index,
		)
	}

	if vlan.VlanId != expectedID {
		return fmt.Errorf(
			"VLAN interface %s has id %d, expected %d",
			link.Attrs().Name,
			vlan.VlanId,
			expectedID,
		)
	}

	return nil
}
