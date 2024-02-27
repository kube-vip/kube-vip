package trafficmirror

import (
	"errors"
	"fmt"

	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

var qDiscNotFound = errors.New("qdisc not found")

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
	qdiscID, err := getQdiscFromInterfaceByType(fromNICName, "ingress")
	if err != nil {
		if err == qDiscNotFound {
			return fmt.Errorf("no qdisc under interface %s is prio type: %v", fromNICName, err)
		} else {
			return err
		}
	}

	log.Debugf("step 4: tc filter add dev %s parent %d: protocol ip u32 match u8 0 0 action mirred egress mirror dev %s", fromNICName, qdiscID, toNICName)

	filter2 := &netlink.U32{
		FilterAttrs: netlink.FilterAttrs{
			LinkIndex: fromNICID,
			Parent:    netlink.MakeHandle(uint16(qdiscID), 0),
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
	_, err = getQdiscFromInterfaceByType(nicName, "ingress")
	if err != nil {
		if err == qDiscNotFound {
			log.Debugf("ingress qdisc doesn't exist on interface %s, skip deleting", nicName)
		} else {
			return err
		}
	}

	qdisc1 := &netlink.Ingress{
		QdiscAttrs: netlink.QdiscAttrs{
			LinkIndex: nicID,
			Parent:    netlink.HANDLE_INGRESS,
		},
	}
	if err := netlink.QdiscDel(qdisc1); err != nil {
		return err
	}

	log.Debug("step 2: delete root prio qdisc")
	_, err = getQdiscFromInterfaceByType(nicName, "prio")
	if err != nil {
		if err == qDiscNotFound {
			log.Debugf("prio qdisc doesn't exist on interface %s, skip deleting", nicName)
		} else {
			return err
		}
	}
	qdiscRoot := netlink.NewPrio(netlink.QdiscAttrs{
		LinkIndex: nicID,
		Parent:    netlink.HANDLE_ROOT,
	})
	if err := netlink.QdiscDel(qdiscRoot); err != nil {
		return err
	}

	log.Infof("finished cleaning up all qdisc config on interface %s", nicName)
	return nil
}

func getQdiscFromInterfaceByType(netIf string, qtype string) (uint32, error) {
	// name of nic which traffic will be mirrored to
	toNIC, err := netlink.LinkByName(netIf)
	if err != nil {
		return 0, fmt.Errorf("failed to find nic %s: %v", netIf, err)
	}
	toNICID := toNIC.Attrs().Index
	// get id through tc qdisc show dev fromNICName
	qs, err := netlink.QdiscList(&netlink.Ifb{LinkAttrs: netlink.LinkAttrs{Index: toNICID}})
	if err != nil {
		fmt.Printf("Failed to list qdisc for interface %s: %v", netIf, err)
		return 0, err
	}
	for _, q := range qs {
		if q.Type() == qtype {
			return q.Attrs().Handle, nil
		}
	}
	log.Errorf("no qdisc under interface %s is prio type: %v", netIf, err)
	return 0, qDiscNotFound
}
