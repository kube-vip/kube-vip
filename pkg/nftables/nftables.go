package nftables

import (
	"errors"
	"fmt"
	"log/slog"
	"net"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"golang.org/x/sys/unix"
)

var Accept = nftables.ChainPolicyAccept

const (
	NatTable  = "kube_vip"
	SNatChain = "kube_vip_snat"
	DNatChain = "kube_vip_dnat"
)

func ApplySNAT(podIP, vipIP string, ignoreCIDR []string, IPv6 bool) error {

	conn, err := nftables.New()
	if err != nil {
		panic(err)
	}

	err = Flush(conn, IPv6) // TODO: pass in the type of table we need to concern ourselves with
	if err != nil {
		panic(err)
	}

	rule, err := CreateRule(podIP, vipIP, ignoreCIDR, conn, IPv6)
	if err != nil {
		panic(err)
	}
	slog.Debug("[egress]", "table", rule.Table.Name, "chain", rule.Chain.Name, "expr", rule.Exprs)
	conn.AddRule(rule) // Add the rule
	conn.Flush()       // Commit the rule to nftables
	err = conn.CloseLasting()
	if err != nil {
		panic(err)
	}
	return nil
}

func GetTable(IPv6 bool) *nftables.Table {
	// Default to IPv4
	table := &nftables.Table{
		Family: nftables.TableFamilyIPv4,
		Name:   NatTable,
	}

	// Move to IPv6 if needed
	if IPv6 == true {
		table.Family = nftables.TableFamilyIPv6
	}
	return table
}

func GetSNatChain(IPv6 bool) *nftables.Chain {
	policy := nftables.ChainPolicyAccept
	return &nftables.Chain{
		Name:     SNatChain,
		Table:    GetTable(IPv6),
		Type:     nftables.ChainTypeNAT,
		Hooknum:  nftables.ChainHookPostrouting,
		Priority: nftables.ChainPriorityNATSource,
		Policy:   &policy,
	}
}

func FilterTable(conn *nftables.Conn, tableName string) (table *nftables.Table, err error) {
	tableList, err := conn.ListTables()
	if err != nil {
		return nil, fmt.Errorf("list tables %v", err)
	}
	for _, t := range tableList {
		if t.Name == tableName {
			return t, nil
		}
	}
	return nil, nil
}

func Flush(conn *nftables.Conn, IPv6 bool) error {
	if t, err := FilterTable(conn, NatTable); err != nil {
		return err
	} else if t != nil {
		conn.DelTable(t)
	}

	// These don't return errors, so not 100% sure how to guarentee things were created
	conn.AddTable(GetTable(IPv6))
	conn.AddChain(GetSNatChain(IPv6))
	return conn.Flush()
}

func CreateRule(podIP, vipIP string, ignoreCIDR []string, conn *nftables.Conn, IPv6 bool) (*nftables.Rule, error) {

	// Get the kube-vip table
	table := GetTable(IPv6)

	// Create our rule
	rule := &nftables.Rule{
		Table: table,
		Exprs: []expr.Any{},
	}
	// Set the correct chain
	rule.Chain = GetSNatChain(IPv6)

	// original pod IP
	if net.ParseIP(podIP) == nil {
		return nil, errors.New("ip is invalid")
	}

	// Create an element using our pod IP
	elements := []nftables.SetElement{
		{
			Key: net.ParseIP(podIP).To4(),
		},
	}
	set := &nftables.Set{
		Table:     table,
		Anonymous: true,
		Constant:  true,
		KeyType:   nftables.TypeIPAddr,
		Interval:  false,
	}

	// Add the set
	conn.AddSet(set, elements)

	// Create the expression using the set
	expression := []expr.Any{
		&expr.Payload{
			OperationType:  expr.PayloadLoad,
			Base:           expr.PayloadBaseNetworkHeader,
			DestRegister:   1,
			SourceRegister: 0,
			Offset:         12,
			Len:            4,
		},
		&expr.Lookup{
			SourceRegister: 1,
			DestRegister:   0,
			SetID:          set.ID,
		},
	}

	// Add expression to the rule
	rule.Exprs = append(rule.Exprs, expression...)

	for _, cidr := range ignoreCIDR {
		start, end, err := nftables.NetFirstAndLastIP(cidr)
		if err != nil {
			return nil, err
		}
		expression = []expr.Any{
			&expr.Payload{
				DestRegister: 1,
				Base:         expr.PayloadBaseNetworkHeader,
				Offset:       16,
				Len:          4,
			},
			&expr.Range{
				Op:       expr.CmpOpNeq,
				Register: 1,
				FromData: start.To4(),
				ToData:   end.To4(),
			},
		}
		// // Add expression to the rule
		rule.Exprs = append(rule.Exprs, expression...)
	}

	// Create SNAT rule
	if net.ParseIP(vipIP) == nil {
		return nil, errors.New("output_ip is not a valid ip")
	}

	expression = []expr.Any{
		&expr.Immediate{
			Register: 1,
			Data:     net.ParseIP(vipIP).To4(),
		},
		&expr.NAT{
			Type:        expr.NATTypeSourceNAT,
			Family:      unix.NFPROTO_IPV4,
			RegAddrMin:  1,
			RegAddrMax:  1,
			RegProtoMin: 0,
			RegProtoMax: 0,
			Random:      false, FullyRandom: false, Persistent: false, Prefix: false,
		},
	}

	// https://github.com/google/nftables/blob/main/nftables_test.go#L5375

	rule.Exprs = append(rule.Exprs, expression...)

	return rule, nil
}
