package egress

//https://github.com/Trojan295/kube-router/commit/d48fd0a275249eb44e272d7f936ac91610c987cd#diff-3b65e4098d69eede2c4abfedc10116dda8fa05b9e308c18c1cb62b1a3fc8c119

import (
	"bufio"
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/coreos/go-iptables/iptables"
	log "github.com/sirupsen/logrus"
)

const (
	preroutingMarkChain  = "CHINCHILLA-PREROUTING-MARK"
	postroutingSnatChain = "CHINCHILLA-POSTROUTING-SNAT"

	egressIPAnnotation         = "egressSNAT.IPAddress"
	egressFixedPortsAnnotation = "egressSNAT.FixedPorts"

	routingTableFile = "/opt/rt_tables"
)

func iptablesChainExists(table string, chain string, it *iptables.IPTables) (bool, error) {
	chains, err := it.ListChains(table)
	if err != nil {
		return false, err
	}

	for _, c := range chains {
		if c == chain {
			return true, nil
		}
	}
	return false, nil
}

func iptablesEnsureChain(table string, chain string, it *iptables.IPTables) error {
	if exists, err := iptablesChainExists(table, chain, it); err != nil {
		return nil
	} else if !exists {
		return it.NewChain(table, chain)
	}
	return nil
}

func iptablesEnsureRuleAtPosition(table, chain string, position int, it *iptables.IPTables, rule ...string) error {
	if exists, err := it.Exists(table, chain, rule...); err != nil {
		return err
	} else if exists {
		if err2 := it.Delete(table, chain, rule...); err2 != nil {
			return err2
		}
	}

	return it.Insert(table, chain, position, rule...)
}

type routeTable struct {
	ID     int
	Name   string
	Subnet net.IPNet
}

func FindRouteTableForIP(ip net.IP, rts []routeTable) *routeTable {
	for _, rt := range rts {
		if rt.Subnet.Contains(ip) {
			return &rt
		}
	}
	return nil
}

func EnsureRouteRule(rt *routeTable) error {
	fwmark := fmt.Sprintf("%x/0xff", rt.ID)

	out, err := exec.Command("ip", "rule", "show", "fwmark", fwmark, "table", rt.Name).Output()
	if err != nil {
		return err
	}

	if string(out) != "" {
		return nil
	}

	_, err = exec.Command("ip", "rule", "add", "fwmark", fwmark, "table", rt.Name).Output()
	if err != nil {
		return err
	}

	return nil
}

func GetRouteTables() ([]routeTable, error) {
	tables := make([]routeTable, 0)

	fp, err := os.Open(routingTableFile)
	if err != nil {
		return tables, err
	}
	defer fp.Close()

	r := bufio.NewScanner(fp)
	for r.Scan() {
		line := strings.Trim(r.Text(), " ")
		if strings.HasPrefix(line, "#") {
			continue
		}

		cols := strings.Fields(line)
		name := cols[1]
		ID, err := strconv.Atoi(cols[0])
		if err != nil {
			log.Error("invalid route table entry in /etc/iproute2/rt_tables")
			continue
		}

		rt := routeTable{
			ID:   ID,
			Name: name,
		}

		if rt.ID == 0 {
			continue
		}

		cidr, err := getCIDRForRouteTable(&rt)
		if err != nil || cidr == nil {
			continue
		}

		rt.Subnet = *cidr

		tables = append(tables, rt)
	}

	return tables, nil
}

func getCIDRForRouteTable(rt *routeTable) (*net.IPNet, error) {
	tableID := fmt.Sprintf("%d", rt.ID)

	out, err := exec.Command("ip", "rule", "show", "table", tableID).Output()
	if err != nil {
		return nil, err
	}

	r := bufio.NewScanner(bytes.NewBuffer(out))

	var cidr *net.IPNet = nil

	pattern := fmt.Sprintf(`\d+:.+from (.+) lookup.+`)
	re := regexp.MustCompile(pattern)

	for r.Scan() {
		line := r.Text()
		result := re.FindStringSubmatch(line)

		if len(result) > 0 {
			_, cidr, _ = net.ParseCIDR(result[1])
			if cidr != nil {
				break
			}
		}
	}

	return cidr, nil
}

