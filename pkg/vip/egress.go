package vip

import (
	"fmt"

	iptables "github.com/coreos/go-iptables/iptables"
	log "github.com/sirupsen/logrus"

	ct "github.com/florianl/go-conntrack"
)

//Notes: https://github.com/cloudnativelabs/kube-router/issues/434

// This file contains all of the functions related to changing SNAT for a
// pod so that it appears to be coming from a VIP.

// 1. Create a new chain in the mangle table
// 2. Ignore (or RETURN) packets going to a service or other pod address
// 3. Mark packets coming from a pod
// 4. Add a rule in the mangle chain PREROUTING to jump to the new chain created above
// 5. Mark packets going through this host (not originating) (might not be needed)
// 6. Perform source nating on marked packets

// Create new iptables client
// Test to find out what exists before hand

const MangleChainName = "KUBE-VIP-EGRESS"
const iptableCommentSuffiv = "KUBE-VIP-"

// DEBUG
const podSubnet = "10.0.0.0/26"
const serviceSubnet = "10.96.0.0/12"

// Create new table

func (e *Egress) DumpChain(name string) error {
	log.Infof("Dumping chain [%s]", name)
	c, err := e.ipTablesClient.List("mangle", name)
	if err != nil {
		return err
	}
	for x := range c {
		log.Infof("Rule -> %s", c[x])
	}
	return nil
}

type Egress struct {
	ipTablesClient *iptables.IPTables
}

func CreateIptablesClient() (*Egress, error) {
	e := new(Egress)
	var err error
	e.ipTablesClient, err = iptables.New()
	return e, err
}

func (e *Egress) CheckMangleChain(name string) (bool, error) {

	log.Infof("[Egress] Cheching for Chain [%s]", name)
	return e.ipTablesClient.ChainExists("mangle", name)

}

func (e *Egress) DeleteMangleChain(name string) error {
	return e.ipTablesClient.ClearAndDeleteChain("mangle", name)
}

func (e *Egress) DeleteManglePrerouting(name string) error {
	return e.ipTablesClient.Delete("mangle", "PREROUTING", "-j", name)
}

func (e *Egress) DeleteSourceNat(podIP, vip string) error {
	return e.ipTablesClient.Delete("nat", "POSTROUTING", "-s", podIP+"/32", "-m", "mark", "--mark", "64/64", "-j", "SNAT", "--to-source", vip)
}

func (e *Egress) CreateMangleChain(name string) error {

	log.Infof("[Egress] Creating Chain [%s]", name)
	// Creates a new chain in the mangle table
	return e.ipTablesClient.NewChain("mangle", name)

}
func (e *Egress) AppendReturnRulesForDestinationSubnet(name, subnet string) error {
	log.Infof("[Egress] Adding jump for subnet [%s] to RETURN to previous chain/rules", subnet)
	exists, _ := e.ipTablesClient.Exists("mangle", name, "-d", subnet, "-j", "RETURN")
	if !exists {
		return e.ipTablesClient.Append("mangle", name, "-d", subnet, "-j", "RETURN")
	}
	return nil
}

func (e *Egress) AppendReturnRulesForMarking(name, subnet string) error {
	log.Infof("[Egress] Marking packets on network [%s]", subnet)
	exists, _ := e.ipTablesClient.Exists("mangle", name, "-s", subnet, "-j", "MARK", "--set-mark", "64/64")
	if !exists {
		return e.ipTablesClient.Append("mangle", name, "-s", subnet, "-j", "MARK", "--set-mark", "64/64")
	}
	return nil
}

func (e *Egress) InsertMangeTableIntoPrerouting(name string) error {
	log.Infof("[Egress] Adding jump from mangle prerouting to [%s]", name)
	if exists, err := e.ipTablesClient.Exists("mangle", "PREROUTING", "-j", name); err != nil {
		return err
	} else if exists {
		if err2 := e.ipTablesClient.Delete("mangle", "PREROUTING", "-j", name); err2 != nil {
			return err2
		}
	}

	return e.ipTablesClient.Insert("mangle", "PREROUTING", 1, "-j", name)
}

func (e *Egress) InsertSourceNat(vip, podIP string) error {
	log.Infof("[Egress] Adding jump from mangle prerouting to [%s]", "name")
	if exists, err := e.ipTablesClient.Exists("nat", "POSTROUTING", "-s", podIP+"/32", "-m", "mark", "--mark", "64/64", "-j", "SNAT", "--to-source", vip); err != nil {
		return err
	} else if exists {
		if err2 := e.ipTablesClient.Delete("nat", "POSTROUTING", "-s", podIP+"/32", "-m", "mark", "--mark", "64/64", "-j", "SNAT", "--to-source", vip); err2 != nil {
			return err2
		}
	}

	return e.ipTablesClient.Insert("nat", "POSTROUTING", 1, "-s", podIP+"/32", "-m", "mark", "--mark", "64/64", "-j", "SNAT", "--to-source", vip)
}

func DeleteExistingSessions(podIP string) error {
	nfct, err := ct.Open(&ct.Config{})
	if err != nil {
		fmt.Println("could not create nfct:", err)
		return nil
	}
	defer nfct.Close()
	sessions, err := nfct.Dump(ct.Conntrack, ct.IPv4)
	if err != nil {
		fmt.Println("could not dump sessions:", err)
		return nil
	}

	for _, session := range sessions {
		if session.Origin.Dst.String() == podIP /*&& *session.Origin.Proto.DstPort == uint16(destinationPort)*/ {
			fmt.Printf("Source -> %s  Destination -> %s:%d\n", session.Origin.Src.String(), session.Origin.Dst.String(), *session.Origin.Proto.DstPort)

			err = nfct.Delete(ct.Conntrack, ct.IPv4, session)
			if err != nil {
				fmt.Println("could not delete sessions:", err)
			}

		}
	}
	return nil
}
