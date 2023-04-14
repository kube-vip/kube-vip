package vip

import (
	"fmt"
	"strings"

	iptables "github.com/kube-vip/kube-vip/pkg/iptables"
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
const Comment = "a3ViZS12aXAK=kube-vip"

type Egress struct {
	ipTablesClient *iptables.IPTables
}

func CreateIptablesClient(nftables bool) (*Egress, error) {
	log.Infof("[egress] Creating an iptables client, nftables mode [%t]", nftables)
	e := new(Egress)
	var err error
	e.ipTablesClient, err = iptables.New(iptables.EnableNFTables(nftables))
	return e, err
}

func (e *Egress) CheckMangleChain(name string) (bool, error) {
	log.Infof("[egress] Checking for Chain [%s]", name)
	return e.ipTablesClient.ChainExists("mangle", name)
}

func (e *Egress) DeleteMangleChain(name string) error {
	return e.ipTablesClient.ClearAndDeleteChain("mangle", name)
}

func (e *Egress) DeleteManglePrerouting(name string) error {
	return e.ipTablesClient.Delete("mangle", "PREROUTING", "-j", name)
}

func (e *Egress) DeleteMangleMarking(podIP, name string) error {
	log.Infof("[egress] Stopping marking packets on network [%s]", podIP)

	exists, _ := e.ipTablesClient.Exists("mangle", name, "-s", podIP, "-j", "MARK", "--set-mark", "64/64", "-m", "comment", "--comment", Comment)

	if !exists {
		return fmt.Errorf("unable to find source Mangle rule for [%s]", podIP)
	}
	return e.ipTablesClient.Delete("mangle", name, "-s", podIP, "-j", "MARK", "--set-mark", "64/64", "-m", "comment", "--comment", Comment)
}

func (e *Egress) DeleteSourceNat(podIP, vip string) error {
	log.Infof("[egress] Removing source nat from [%s] => [%s]", podIP, vip)

	exists, _ := e.ipTablesClient.Exists("nat", "POSTROUTING", "-s", podIP+"/32", "-m", "mark", "--mark", "64/64", "-j", "SNAT", "--to-source", vip, "-m", "comment", "--comment", Comment)

	if !exists {
		return fmt.Errorf("unable to find source Nat rule for [%s]", podIP)
	}
	return e.ipTablesClient.Delete("nat", "POSTROUTING", "-s", podIP+"/32", "-m", "mark", "--mark", "64/64", "-j", "SNAT", "--to-source", vip, "-m", "comment", "--comment", Comment)
}

func (e *Egress) DeleteSourceNatForDestinationPort(podIP, vip, port, proto string) error {
	log.Infof("[egress] Adding source nat from [%s] => [%s]", podIP, vip)

	exists, _ := e.ipTablesClient.Exists("nat", "POSTROUTING", "-s", podIP+"/32", "-m", "mark", "--mark", "64/64", "-j", "SNAT", "--to-source", vip, "-p", proto, "--dport", port, "-m", "comment", "--comment", Comment)

	if !exists {
		return fmt.Errorf("unable to find source Nat rule for [%s], with destination port [%s]", podIP, port)
	}
	return e.ipTablesClient.Delete("nat", "POSTROUTING", "-s", podIP+"/32", "-m", "mark", "--mark", "64/64", "-j", "SNAT", "--to-source", vip, "-p", proto, "--dport", port, "-m", "comment", "--comment", Comment)
}

func (e *Egress) CreateMangleChain(name string) error {

	log.Infof("[egress] Creating Chain [%s]", name)
	// Creates a new chain in the mangle table
	return e.ipTablesClient.NewChain("mangle", name)

}
func (e *Egress) AppendReturnRulesForDestinationSubnet(name, subnet string) error {
	log.Infof("[egress] Adding jump for subnet [%s] to RETURN to previous chain/rules", subnet)
	exists, _ := e.ipTablesClient.Exists("mangle", name, "-d", subnet, "-j", "RETURN", "-m", "comment", "--comment", Comment)
	if !exists {
		return e.ipTablesClient.Append("mangle", name, "-d", subnet, "-j", "RETURN", "-m", "comment", "--comment", Comment)
	}
	return nil
}

func (e *Egress) AppendReturnRulesForMarking(name, subnet string) error {
	log.Infof("[egress] Marking packets on network [%s]", subnet)
	exists, _ := e.ipTablesClient.Exists("mangle", name, "-s", subnet, "-j", "MARK", "--set-mark", "64/64", "-m", "comment", "--comment", Comment)
	if !exists {
		return e.ipTablesClient.Append("mangle", name, "-s", subnet, "-j", "MARK", "--set-mark", "64/64", "-m", "comment", "--comment", Comment)
	}
	return nil
}

func (e *Egress) InsertMangeTableIntoPrerouting(name string) error {
	log.Infof("[egress] Adding jump from mangle prerouting to [%s]", name)
	if exists, err := e.ipTablesClient.Exists("mangle", "PREROUTING", "-j", name, "-m", "comment", "--comment", Comment); err != nil {
		return err
	} else if exists {
		if err2 := e.ipTablesClient.Delete("mangle", "PREROUTING", "-j", name, "-m", "comment", "--comment", Comment); err2 != nil {
			return err2
		}
	}

	return e.ipTablesClient.Insert("mangle", "PREROUTING", 1, "-j", name, "-m", "comment", "--comment", Comment)
}

func (e *Egress) InsertSourceNat(vip, podIP string) error {
	log.Infof("[egress] Adding source nat from [%s] => [%s]", podIP, vip)
	if exists, err := e.ipTablesClient.Exists("nat", "POSTROUTING", "-s", podIP+"/32", "-m", "mark", "--mark", "64/64", "-j", "SNAT", "--to-source", vip, "-m", "comment", "--comment", Comment); err != nil {
		return err
	} else if exists {
		if err2 := e.ipTablesClient.Delete("nat", "POSTROUTING", "-s", podIP+"/32", "-m", "mark", "--mark", "64/64", "-j", "SNAT", "--to-source", vip, "-m", "comment", "--comment", Comment); err2 != nil {
			return err2
		}
	}

	return e.ipTablesClient.Insert("nat", "POSTROUTING", 1, "-s", podIP+"/32", "-m", "mark", "--mark", "64/64", "-j", "SNAT", "--to-source", vip, "-m", "comment", "--comment", Comment)
}

func (e *Egress) InsertSourceNatForDestinationPort(vip, podIP, port, proto string) error {
	log.Infof("[egress] Adding source nat from [%s] => [%s], with destination port [%s]", podIP, vip, port)
	if exists, err := e.ipTablesClient.Exists("nat", "POSTROUTING", "-s", podIP+"/32", "-m", "mark", "--mark", "64/64", "-j", "SNAT", "--to-source", vip, "-p", proto, "--dport", port, "-m", "comment", "--comment", Comment); err != nil {
		return err
	} else if exists {
		if err2 := e.ipTablesClient.Delete("nat", "POSTROUTING", "-s", podIP+"/32", "-m", "mark", "--mark", "64/64", "-j", "SNAT", "--to-source", vip, "-p", proto, "--dport", port, "-m", "comment", "--comment", Comment); err2 != nil {
			return err2
		}
	}

	return e.ipTablesClient.Insert("nat", "POSTROUTING", 1, "-s", podIP+"/32", "-m", "mark", "--mark", "64/64", "-j", "SNAT", "--to-source", vip, "-p", proto, "--dport", port, "-m", "comment", "--comment", Comment)
}

func DeleteExistingSessions(sessionIP string, destination bool) error {

	nfct, err := ct.Open(&ct.Config{})
	if err != nil {
		log.Errorf("could not create nfct: %v", err)
		return err
	}
	defer nfct.Close()
	sessions, err := nfct.Dump(ct.Conntrack, ct.IPv4)
	if err != nil {
		log.Errorf("could not dump sessions: %v", err)
		return err
	}
	// by default we only clear source (i.e. connections going from the vip (egress))
	if !destination {
		for _, session := range sessions {
			//fmt.Printf("Looking for [%s] found [%s]\n", podIP, session.Origin.Dst.String())

			if session.Origin.Src.String() == sessionIP /*&& *session.Origin.Proto.DstPort == uint16(destinationPort)*/ {
				//fmt.Printf("Source -> %s  Destination -> %s:%d\n", session.Origin.Src.String(), session.Origin.Dst.String(), *session.Origin.Proto.DstPort)
				err = nfct.Delete(ct.Conntrack, ct.IPv4, session)
				if err != nil {
					log.Errorf("could not delete sessions: %v", err)
				}
			}
		}
	} else {
		// This will clear any "dangling" outbound connections.
		for _, session := range sessions {
			//fmt.Printf("Looking for [%s] found [%s]\n", podIP, session.Origin.Dst.String())

			if session.Origin.Dst.String() == sessionIP /*&& *session.Origin.Proto.DstPort == uint16(destinationPort)*/ {
				//fmt.Printf("Source -> %s  Destination -> %s:%d\n", session.Origin.Src.String(), session.Origin.Dst.String(), *session.Origin.Proto.DstPort)
				err = nfct.Delete(ct.Conntrack, ct.IPv4, session)
				if err != nil {
					log.Errorf("could not delete sessions: %v", err)
				}
			}
		}
	}

	return nil
}

// Debug functions

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

func (e *Egress) CleanIPtables() error {
	natRules, err := e.ipTablesClient.List("nat", "POSTROUTING")
	if err != nil {
		return err
	}
	foundNatRules := findRules(natRules)
	log.Warnf("[egress] Cleaning [%d] dangling postrouting nat rules", len(foundNatRules))
	for x := range foundNatRules {
		err = e.ipTablesClient.Delete("nat", "POSTROUTING", foundNatRules[x][2:]...)
		if err != nil {
			log.Errorf("[egress] Error removing rule [%v]", err)
		}
	}
	exists, err := e.CheckMangleChain(MangleChainName)
	if err != nil {
		log.Debugf("[egress] No Mangle chain exists [%v]", err)
	}
	if exists {
		mangleRules, err := e.ipTablesClient.List("mangle", MangleChainName)
		if err != nil {
			return err
		}
		foundNatRules = findRules(mangleRules)
		log.Warnf("[egress] Cleaning [%d] dangling prerouting mangle rules", len(foundNatRules))
		for x := range foundNatRules {
			err = e.ipTablesClient.Delete("mangle", MangleChainName, foundNatRules[x][2:]...)
			if err != nil {
				log.Errorf("[egress] Error removing rule [%v]", err)
			}
		}
		// For unknown reasons RHEL and the nftables wrapper sometimes leave dangling rules
		// So we shall nuke them from orbit (just to be sure)
		err = e.ipTablesClient.ClearChain("mangle", MangleChainName)
		if err != nil {
			log.Errorf("[egress] Error removing flushing table [%v]", err)
		}
	} else {
		log.Warnf("No existing mangle chain [%s] exists", MangleChainName)
	}
	return nil
}

func findRules(rules []string) [][]string {
	var foundRules [][]string

	for i := range rules {
		r := strings.Split(rules[i], " ")
		for x := range r {
			if r[x] == "\""+Comment+"\"" {
				// Remove the quotes around the comment
				r[x] = strings.Trim(r[x], "\"")
				foundRules = append(foundRules, r)
			}
		}
	}

	return foundRules
}
