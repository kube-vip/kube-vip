package forwarder

import (
	"context"
	"strconv"
	"strings"

	log "github.com/sirupsen/logrus"

	"github.com/kube-vip/kube-vip/pkg/network"
)

func startIPTables(ctx context.Context, srcAddress string, srcPort int, destAddress string, destPort int) {
	//TODO support IPV6. iptables library support that (need to add ip6tables binary to the container first)
	if strings.Contains(srcAddress, ":") {
		log.Fatal("IPV6 forwarding isn't yet supported. Feel free to contribute.")
	}
	// Have a routine ensuring the iptables forwarding rules each 5 seconds
	log.Infof("Setting up iptables backend forwarding: from %s:%d to %s:%d", srcAddress, srcPort, destAddress, destPort)
	go network.SetupAndEnsureIPTables(ctx, forwardRules(srcAddress, srcPort, destAddress, destPort), 5)
}

func stopIPTables(srcAddress string, srcPort int, destAddress string, destPort int) error {
	log.Infof("Stopping iptables backend forwarding: from %s:%d to %s:%d", srcAddress, srcPort, destAddress, destPort)
	return network.DeleteIPTables(forwardRules(srcAddress, srcPort, destAddress, destPort))
}

func forwardRules(srcAddress string, srcPort int, destAddress string, destPort int) []network.IPTablesRule {
	random := false
	ipt, err := network.NewIPTables()
	if err == nil {
		random = ipt.HasRandomFully()
	}

	rr := []network.IPTablesRule{
		{Table: "nat", Chain: "PREROUTING", Rulespec: []string{"-d", srcAddress, "-p", "tcp", "-m", "tcp", "--dport", strconv.Itoa(srcPort), "-j", "DNAT", "--to-destination", destAddress + ":" + strconv.Itoa(destPort)}},
		{Table: "nat", Chain: "POSTROUTING", Rulespec: []string{"-d", destAddress, "-p", "tcp", "-m", "tcp", "--dport", strconv.Itoa(destPort), "-j", "MASQUERADE"}},
		{Table: "nat", Chain: "OUTPUT", Rulespec: []string{"-d", srcAddress, "-p", "tcp", "-m", "tcp", "--dport", strconv.Itoa(srcPort), "-j", "DNAT", "--to-destination", destAddress + ":" + strconv.Itoa(destPort)}},
	}

	if random {
		rr = []network.IPTablesRule{
			{Table: "nat", Chain: "PREROUTING", Rulespec: []string{"-d", srcAddress, "-p", "tcp", "-m", "tcp", "--dport", strconv.Itoa(srcPort), "-j", "DNAT", "--to-destination", destAddress + ":" + strconv.Itoa(destPort), "--random"}},
			{Table: "nat", Chain: "POSTROUTING", Rulespec: []string{"-d", destAddress, "-p", "tcp", "-m", "tcp", "--dport", strconv.Itoa(destPort), "-j", "MASQUERADE", "--random-fully"}},
			{Table: "nat", Chain: "OUTPUT", Rulespec: []string{"-d", srcAddress, "-p", "tcp", "-m", "tcp", "--dport", strconv.Itoa(srcPort), "-j", "DNAT", "--to-destination", destAddress + ":" + strconv.Itoa(destPort), "--random"}},
		}
	}

	return rr
}
