package vip

import (
	"fmt"
	"strconv"
	"strings"

	log "log/slog"

	iptables "github.com/kube-vip/kube-vip/pkg/iptables"

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
	comment        string
}

func CreateIptablesClient(nftables bool, namespace string, protocol iptables.Protocol) (*Egress, error) {
	proto := "IPv4"
	if protocol == iptables.ProtocolIPv6 {
		proto = "IPv6"
	}
	log.Info("[egress] Creating an iptables client", "nftables", nftables, "protocol", proto)
	e := new(Egress)
	var err error

	options := []iptables.Option{}
	options = append(options, iptables.EnableNFTables(nftables))

	if protocol == iptables.ProtocolIPv6 {
		options = append(options, iptables.IPFamily(iptables.ProtocolIPv6), iptables.Timeout(5))
	}

	e.ipTablesClient, err = iptables.New(options...)
	e.comment = Comment + "-" + namespace
	return e, err
}

func (e *Egress) CheckMangleChain(name string) (bool, error) {
	log.Info("[egress] chain exists", "name", name)
	return e.ipTablesClient.ChainExists("mangle", name)
}

func (e *Egress) DeleteMangleChain(name string) error {
	return e.ipTablesClient.ClearAndDeleteChain("mangle", name)
}

func (e *Egress) DeleteManglePrerouting(name string) error {
	return e.ipTablesClient.Delete("mangle", "PREROUTING", "-j", name)
}

func (e *Egress) DeleteMangleReturnForNetwork(name, network string) error {
	return e.ipTablesClient.Delete("mangle", name, "-d", network, "-j", "RETURN", "-m", "comment", "--comment", e.comment)
}

func (e *Egress) DeleteMangleMarking(podIP, name string) error {
	log.Info("[egress] Stopping marking packets on network", "podIP", podIP)

	exists, _ := e.ipTablesClient.Exists("mangle", name, "-s", podIP, "-j", "MARK", "--set-mark", "64/64", "-m", "comment", "--comment", e.comment)

	if !exists {
		return fmt.Errorf("unable to find source Mangle rule for [%s]", podIP)
	}
	return e.ipTablesClient.Delete("mangle", name, "-s", podIP, "-j", "MARK", "--set-mark", "64/64", "-m", "comment", "--comment", e.comment)
}

func (e *Egress) DeleteMangleMarkingForNetwork(podIP, name, network string) error {
	log.Info("[egress] Stopping marking packets", "podIP", podIP, "network", network)

	// exists, _ := e.ipTablesClient.Exists("mangle", name, "-s", podIP, "-d", network, "-j", "MARK", "--set-mark", "64/64", "-m", "comment", "--comment", e.comment)

	// if !exists {
	// 	return fmt.Errorf("unable to find source Mangle rule for [%s]", podIP)
	// }
	return e.ipTablesClient.Delete("mangle", name, "-s", podIP, "-d", network, "-j", "MARK", "--set-mark", "64/64", "-m", "comment", "--comment", e.comment)
}

func (e *Egress) DeleteSourceNat(podIP, vip string) error {
	log.Info("[egress] Removing source nat", "podIP", podIP, "vip", vip)

	exists, _ := e.ipTablesClient.Exists("nat", "POSTROUTING", "-s", podIP+"/32", "-m", "mark", "--mark", "64/64", "-j", "SNAT", "--to-source", vip, "-m", "comment", "--comment", e.comment)

	if !exists {
		return fmt.Errorf("unable to find source Nat rule for [%s]", podIP)
	}
	return e.ipTablesClient.Delete("nat", "POSTROUTING", "-s", podIP+"/32", "-m", "mark", "--mark", "64/64", "-j", "SNAT", "--to-source", vip, "-m", "comment", "--comment", e.comment)
}

func (e *Egress) DeleteSourceNatForDestinationPort(podIP, vip, port, proto string) error {
	log.Info("[egress] Removing source nat", "podIP", podIP, "vip", vip, "destination port", port)

	exists, _ := e.ipTablesClient.Exists("nat", "POSTROUTING", "-s", podIP+"/32", "-m", "mark", "--mark", "64/64", "-j", "SNAT", "--to-source", vip, "-p", proto, "--dport", port, "-m", "comment", "--comment", e.comment)

	if !exists {
		return fmt.Errorf("unable to find source Nat rule for [%s], with destination port [%s]", podIP, port)
	}
	return e.ipTablesClient.Delete("nat", "POSTROUTING", "-s", podIP+"/32", "-m", "mark", "--mark", "64/64", "-j", "SNAT", "--to-source", vip, "-p", proto, "--dport", port, "-m", "comment", "--comment", e.comment)
}

func (e *Egress) CreateMangleChain(name string) error {

	log.Info("[egress] Creating Chain", "name", name)
	// Creates a new chain in the mangle table
	return e.ipTablesClient.NewChain("mangle", name)

}
func (e *Egress) AppendReturnRulesForDestinationSubnet(name, subnet string) error {
	log.Info("[egress] Adding jump for subnet to RETURN to previous chain/rules", "subnet", subnet)
	exists, _ := e.ipTablesClient.Exists("mangle", name, "-d", subnet, "-j", "RETURN", "-m", "comment", "--comment", e.comment)
	if !exists {
		return e.ipTablesClient.Append("mangle", name, "-d", subnet, "-j", "RETURN", "-m", "comment", "--comment", e.comment)
	}
	return nil
}

func (e *Egress) AppendReturnRulesForMarking(name, subnet string) error {
	log.Info("[egress] Marking packets on network", "subnet", subnet)
	exists, _ := e.ipTablesClient.Exists("mangle", name, "-s", subnet, "-j", "MARK", "--set-mark", "64/64", "-m", "comment", "--comment", e.comment)
	if !exists {
		return e.ipTablesClient.Append("mangle", name, "-s", subnet, "-j", "MARK", "--set-mark", "64/64", "-m", "comment", "--comment", e.comment)
	}
	return nil
}

func (e *Egress) AppendReturnRulesForMarkingForNetwork(name, subnet, destination string) error {
	log.Info("[egress] Marking packets on network", "subnet", subnet)
	exists, _ := e.ipTablesClient.Exists("mangle", name, "-s", subnet, "-d", destination, "-j", "MARK", "--set-mark", "64/64", "-m", "comment", "--comment", e.comment)
	if !exists {
		return e.ipTablesClient.Append("mangle", name, "-s", subnet, "-d", destination, "-j", "MARK", "--set-mark", "64/64", "-m", "comment", "--comment", e.comment)
	}
	return nil
}

func (e *Egress) InsertMangeTableIntoPrerouting(name string) error {
	log.Info("[egress] Adding jump from mangle prerouting", "destination", name)
	if exists, err := e.ipTablesClient.Exists("mangle", "PREROUTING", "-j", name, "-m", "comment", "--comment", e.comment); err != nil {
		return err
	} else if exists {
		if err2 := e.ipTablesClient.Delete("mangle", "PREROUTING", "-j", name, "-m", "comment", "--comment", e.comment); err2 != nil {
			return err2
		}
	}

	return e.ipTablesClient.Insert("mangle", "PREROUTING", 1, "-j", name, "-m", "comment", "--comment", e.comment)
}

func (e *Egress) InsertSourceNat(vip, podIP string) error {
	log.Info("[egress] Adding source nat", "original source", podIP, "new source", vip)
	if exists, err := e.ipTablesClient.Exists("nat", "POSTROUTING", "-s", podIP+"/32", "-m", "mark", "--mark", "64/64", "-j", "SNAT", "--to-source", vip, "-m", "comment", "--comment", e.comment); err != nil {
		return err
	} else if exists {
		if err2 := e.ipTablesClient.Delete("nat", "POSTROUTING", "-s", podIP+"/32", "-m", "mark", "--mark", "64/64", "-j", "SNAT", "--to-source", vip, "-m", "comment", "--comment", e.comment); err2 != nil {
			return err2
		}
	}

	return e.ipTablesClient.Insert("nat", "POSTROUTING", 1, "-s", podIP+"/32", "-m", "mark", "--mark", "64/64", "-j", "SNAT", "--to-source", vip, "-m", "comment", "--comment", e.comment)
}

func (e *Egress) InsertSourceNatForDestinationPort(vip, podIP, port, proto string) error {
	log.Info("[egress] Adding source nat", "from", podIP, "to", vip, "port", port)
	natRules, err := e.ipTablesClient.List("nat", "POSTROUTING")
	if err != nil {
		return err
	}
	foundNatRules := e.findExistingVIP(natRules, vip)
	log.Warn("[egress] Cleaning existing postrouting nat rules for vip", "rulecount", len(foundNatRules), "vip", vip)
	for x := range foundNatRules {
		err = e.ipTablesClient.Delete("nat", "POSTROUTING", foundNatRules[x][2:]...)
		if err != nil {
			log.Error("[egress] removing rule", "err", err)
		}
	}

	if exists, err := e.ipTablesClient.Exists("nat", "POSTROUTING", "-s", podIP+"/32", "-m", "mark", "--mark", "64/64", "-j", "SNAT", "--to-source", vip, "-p", proto, "--dport", port, "-m", "comment", "--comment", e.comment); err != nil {
		return err
	} else if exists {
		if err2 := e.ipTablesClient.Delete("nat", "POSTROUTING", "-s", podIP+"/32", "-m", "mark", "--mark", "64/64", "-j", "SNAT", "--to-source", vip, "-p", proto, "--dport", port, "-m", "comment", "--comment", e.comment); err2 != nil {
			return err2
		}
	}

	return e.ipTablesClient.Insert("nat", "POSTROUTING", 1, "-s", podIP+"/32", "-m", "mark", "--mark", "64/64", "-j", "SNAT", "--to-source", vip, "-p", proto, "--dport", port, "-m", "comment", "--comment", e.comment)
}

func DeleteExistingSessions(sessionIP string, destination bool, destinationPorts, srcPorts string) error {

	nfct, err := ct.Open(&ct.Config{})
	if err != nil {
		log.Error("create conntrack client", "err", err)
		return err
	}
	defer nfct.Close()
	sessions, err := nfct.Dump(ct.Conntrack, ct.IPv4)
	if err != nil {
		log.Error("could not dump sessions", "err", err)
		return err
	}
	destPortProtocol := make(map[uint16]uint8)
	srcPortProtocol := make(map[uint16]uint8)

	if destinationPorts != "" {
		fixedPorts := strings.Split(destinationPorts, ",")

		for _, fixedPort := range fixedPorts {

			data := strings.Split(fixedPort, ":")
			if len(data) == 0 {
				continue
			}
			port, err := strconv.ParseUint(data[1], 10, 16)
			if err != nil {
				return fmt.Errorf("[egress] error parsing annotaion [%s]", destinationPorts)
			}
			switch data[0] {
			case strings.ToLower("udp"):
				destPortProtocol[uint16(port)] = ProtocolUDP
			case strings.ToLower("tcp"):
				destPortProtocol[uint16(port)] = ProtocolTCP
			case strings.ToLower("sctp"):
				destPortProtocol[uint16(port)] = ProtocolSCTP
			default:
				log.Error("[egress] annotation protocol isn't supported", "protocol ID", data[0])
			}
		}
	}

	if srcPorts != "" {
		fixedPorts := strings.Split(srcPorts, ",")

		for _, fixedPort := range fixedPorts {

			data := strings.Split(fixedPort, ":")
			if len(data) == 0 {
				continue
			}
			port, err := strconv.ParseUint(data[1], 10, 16)
			if err != nil {
				return fmt.Errorf("[egress] error parsing annotaion [%s]", srcPorts)
			}
			switch data[0] {
			case strings.ToLower("udp"):
				srcPortProtocol[uint16(port)] = ProtocolUDP
			case strings.ToLower("tcp"):
				srcPortProtocol[uint16(port)] = ProtocolTCP
			case strings.ToLower("sctp"):
				srcPortProtocol[uint16(port)] = ProtocolSCTP
			default:
				log.Error("[egress] annotation protocol isn't supported", "protocol ID", data[0])
			}
		}
	}

	// by default we only clear source (i.e. connections going from the vip (egress))
	if !destination {
		for _, session := range sessions {
			//session.Origin.Proto
			if session.Origin.Src.String() == sessionIP /*&& *session.Origin.Proto.DstPort == uint16(destinationPort)*/ {
				if destinationPorts != "" {
					proto := destPortProtocol[*session.Origin.Proto.DstPort]
					if proto == *session.Origin.Proto.Number {
						log.Info("[egress] cleaning existing connection", "src", session.Origin.Src.String(), "dst", session.Origin.Dst.String(), "dst port", *session.Origin.Proto.DstPort, "protocol", *session.Origin.Proto.Number)
						err = nfct.Delete(ct.Conntrack, ct.IPv4, session)
					}
				} else {
					err = nfct.Delete(ct.Conntrack, ct.IPv4, session)
				}
				if err != nil {
					log.Error("could not delete sessions", "err", err)
				}
			}
		}
	} else {
		// This will clear any "dangling" outbound connections.
		for _, session := range sessions {
			//fmt.Printf("Looking for [%s] found [%s]\n", podIP, session.Origin.Dst.String())

			if session.Origin.Dst.String() == sessionIP /*&& *session.Origin.Proto.DstPort == uint16(destinationPort)*/ {
				if srcPorts != "" {
					proto := srcPortProtocol[*session.Origin.Proto.DstPort]
					if proto == *session.Origin.Proto.Number {
						log.Info("[egress] cleaning existing connection", "src", session.Origin.Src.String(), "dst", session.Origin.Dst.String(), "dst port", *session.Origin.Proto.DstPort, "protocol", *session.Origin.Proto.Number)
						err = nfct.Delete(ct.Conntrack, ct.IPv4, session)
					}
				} else {
					err = nfct.Delete(ct.Conntrack, ct.IPv4, session)
				}
				if err != nil {
					log.Error("could not delete sessions", "err", err)
				}
			}
		}
	}

	return nil
}

// Debug functions

func (e *Egress) DumpChain(name string) error {
	log.Info("Dumping chain", "name", name)
	c, err := e.ipTablesClient.List("mangle", name)
	if err != nil {
		return err
	}
	for x := range c {
		log.Info("", "rule", c[x])
	}
	return nil
}

func (e *Egress) CleanIPtables() error {
	natRules, err := e.ipTablesClient.List("nat", "POSTROUTING")
	if err != nil {
		return err
	}
	foundNatRules := e.findRules(natRules)
	log.Warn("[egress] Cleaning dangling postrouting nat rules", "rulecount", len(foundNatRules))
	for x := range foundNatRules {
		err = e.ipTablesClient.Delete("nat", "POSTROUTING", foundNatRules[x][2:]...)
		if err != nil {
			log.Error("[egress] Error removing rule", "err", err)
		}
	}
	exists, err := e.CheckMangleChain(MangleChainName)
	if err != nil {
		log.Debug("[egress] No Mangle chain exists", "err", err)
	}
	if exists {
		mangleRules, err := e.ipTablesClient.List("mangle", MangleChainName)
		if err != nil {
			return err
		}
		foundNatRules = e.findRules(mangleRules)
		log.Warn("[egress] Cleaning dangling prerouting mangle rules", "rulecount", len(foundNatRules))
		for x := range foundNatRules {
			err = e.ipTablesClient.Delete("mangle", MangleChainName, foundNatRules[x][2:]...)
			if err != nil {
				log.Error("[egress] Error removing rule", "err", err)
			}
		}

		// For unknown reasons RHEL and the nftables wrapper sometimes leave dangling rules
		// So we shall nuke them from orbit (just to be sure)

		// err = e.ipTablesClient.ClearChain("mangle", MangleChainName)
		// if err != nil {
		// 	log.Errorf("[egress] Error removing flushing table [%v]", err)
		// }
	} else {
		log.Warn("No existing mangle chain exists", "chain name", MangleChainName)
	}
	return nil
}

func (e *Egress) findRules(rules []string) [][]string {
	var foundRules [][]string

	for i := range rules {
		r := strings.Split(rules[i], " ")
		for x := range r {
			if r[x] == "\""+e.comment+"\"" {
				// Remove the quotes around the comment
				r[x] = strings.Trim(r[x], "\"")
				foundRules = append(foundRules, r)
			}
		}
	}

	return foundRules
}

func (e *Egress) findExistingVIP(rules []string, vip string) [][]string {
	var foundRules [][]string

	for i := range rules {
		r := strings.Split(rules[i], " ")
		for x := range r {
			// Look for a vip already in a post Routing rule
			if r[x] == vip {
				foundRules = append(foundRules, r)
			}
		}
	}

	return foundRules
}

func ClearIPTables(useNftables bool, namespace string, protocol iptables.Protocol) {
	i, err := CreateIptablesClient(useNftables, namespace, protocol)
	if err != nil {
		log.Warn("[egress] Unable to clean any dangling egress rules", "err", err)
		log.Warn("[egress] Can be ignored in non iptables release of kube-vip")
	} else {
		log.Info("[egress] Cleaning any dangling kube-vip egress rules")
		cleanErr := i.CleanIPtables()
		if cleanErr != nil {
			log.Error("Error cleaning rules", "err", cleanErr)
		}
	}
}
