package trafficmirror

import (
	"errors"
	"fmt"

	log "github.com/gookit/slog"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

var errQdiscNotFound = errors.New("qdisc not found")

// MirrorTrafficFromNIC use netlink to implement tc command to mirror traffic from
// one interface to another
func MirrorTrafficFromNIC(fromNICName, toNICName string) error {
	// name of nic which traffic will be mirrored from
	fromNIC, err := netlink.LinkByName(fromNICName)
	if err != nil {
		return fmt.Errorf("failed to find nic %s: %v", fromNICName, err)
	}
	fromNICID := fromNIC.Attrs().Index

	// name of nic which traffic will be mirrored to
	toNIC, err := netlink.LinkByName(toNICName)
	if err != nil {
		return fmt.Errorf("failed to find nic %s: %v", toNICName, err)
	}
	toNICID := toNIC.Attrs().Index

	log.Debugf("interface %s has index %d", fromNICName, fromNICID)
	log.Debugf("interface %s has index %d", toNICName, toNICID)

	log.Debugf("clean up interface %s first in case it has stale qdsic", fromNICName)
	if err := CleanupQDSICFromNIC(fromNICName); err != nil {
		return err
	}

	log.Debugf("step 1: tc qdisc add dev %s ingress", fromNICName)
	qdisc1 := &netlink.Ingress{
		QdiscAttrs: netlink.QdiscAttrs{
			LinkIndex: fromNICID,
			Parent:    netlink.HANDLE_INGRESS,
		},
	}

	if err := netlink.QdiscAdd(qdisc1); err != nil {
		return fmt.Errorf("failed to add qdisc for interface %s: %v", fromNICName, err)
	}

	log.Debugf("step 2: tc filter add dev %s parent ffff: protocol ip u32 match u8 0 0 action mirred egress mirror dev %s", fromNICName, toNICName)
	// add a filter to mirror traffic from index1 to index2
	filter1 := &netlink.U32{
		FilterAttrs: netlink.FilterAttrs{
			LinkIndex: fromNICID,
			Parent:    netlink.MakeHandle(0xffff, 0),
			Protocol:  unix.ETH_P_ALL,
		},
		Actions: []netlink.Action{
			&netlink.MirredAction{
				ActionAttrs: netlink.ActionAttrs{
					Action: netlink.TC_ACT_PIPE,
				},
				MirredAction: netlink.TCA_EGRESS_MIRROR,
				Ifindex:      toNICID,
			},
		},
	}

	if err := netlink.FilterAdd(filter1); err != nil {
		return fmt.Errorf("failed to add filter for interface %s: %v", fromNICName, err)
	}

	log.Debugf("step 3: tc qdisc add dev %s ingress", fromNICName)
	qdiscTemp := netlink.NewPrio(netlink.QdiscAttrs{
		LinkIndex: fromNICID,
		Parent:    netlink.HANDLE_ROOT,
	})

	if err := netlink.QdiscReplace(qdiscTemp); err != nil {
		return fmt.Errorf("failed to replace qdisc with prio type qdisc: %v", err)
	}

	// get id through tc qdisc show dev fromNICName
	qdiscID, err := getQdiscFromInterfaceByType(fromNICID, fromNICName, "ingress")
	if err != nil {
		if err == errQdiscNotFound {
			return fmt.Errorf("no qdisc under interface %s is prio type: %v", fromNICName, err)
		}
		return err
	}

	log.Debugf("step 4: tc filter add dev %s parent %d: protocol ip u32 match u8 0 0 action mirred egress mirror dev %s", fromNICName, qdiscID, toNICName)

	filter2 := &netlink.U32{
		FilterAttrs: netlink.FilterAttrs{
			LinkIndex: fromNICID,
			Parent:    netlink.MakeHandle(uint16(qdiscID), 0), //nolint
			Protocol:  unix.ETH_P_ALL,
		},
		Actions: []netlink.Action{
			&netlink.MirredAction{
				ActionAttrs: netlink.ActionAttrs{
					Action: netlink.TC_ACT_PIPE,
				},
				MirredAction: netlink.TCA_EGRESS_MIRROR,
				Ifindex:      toNICID,
			},
		},
	}

	if err := netlink.FilterAdd(filter2); err != nil {
		return fmt.Errorf("failed to add filter for interface %s: %v", fromNICName, err)
	}

	log.Infof("traffic mirroring has been set up from interface %s to interface %s", toNICName, fromNICName)
	return nil
}

// CleanupQDSICFromNIC cleans up all qdisc config on interface
func CleanupQDSICFromNIC(nicName string) error {
	// name of nic which traffic will be mirrored to
	toNIC, err := netlink.LinkByName(nicName)
	if err != nil {
		return fmt.Errorf("failed to find nic %s: %v", nicName, err)
	}
	nicID := toNIC.Attrs().Index

	log.Debugf("interface %s has index %d", nicName, nicID)

	log.Debug("step 1: delete ingress qdisc")
	if err := tryCleanupQdiscByType(nicID, nicName, "ingress"); err != nil {
		return err
	}

	log.Debug("step 2: delete root prio qdisc")
	if err := tryCleanupQdiscByType(nicID, nicName, "prio"); err != nil {
		return err
	}

	log.Infof("finished cleaning up all qdisc config on interface %s", nicName)
	return nil
}

func getQdiscFromInterfaceByType(nicID int, nicName string, qType string) (uint32, error) {
	// get id through tc qdisc show dev fromNICName
	qs, err := netlink.QdiscList(&netlink.Ifb{LinkAttrs: netlink.LinkAttrs{Index: nicID}})
	if err != nil {
		fmt.Printf("Failed to list qdisc for interface %s: %v", nicName, err)
		return 0, err
	}
	for _, q := range qs {
		if q.Type() == qType {
			return q.Attrs().Handle, nil
		}
	}
	log.Errorf("no qdisc under interface %s is %s type", nicName, qType)
	return 0, errQdiscNotFound
}

func tryCleanupQdiscByType(nicID int, nicName, qType string) error {
	var qdisc netlink.Qdisc
	switch qType {
	case "ingress":
		qdisc = &netlink.Ingress{QdiscAttrs: netlink.QdiscAttrs{
			LinkIndex: nicID,
			Parent:    netlink.HANDLE_INGRESS,
		}}
	case "prio":
		qdisc = netlink.NewPrio(netlink.QdiscAttrs{
			LinkIndex: nicID,
			Parent:    netlink.HANDLE_ROOT,
		})
	default:
		return fmt.Errorf("unknown qdisc type %s", qType)
	}

	_, err := getQdiscFromInterfaceByType(nicID, nicName, qType)
	if err != nil {
		if err == errQdiscNotFound {
			log.Debugf("%s type qdisc doesn't exist on interface %s, skip deleting", qType, nicName)
			return nil
		}
		return err
	}
	if err := netlink.QdiscDel(qdisc); err != nil {
		return err
	}
	return nil
}
