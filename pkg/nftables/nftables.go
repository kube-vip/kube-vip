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

const (
	NatTable  = "kube_vip_%s"
	SNatChain = "kube_vip_snat_%s_%s"
	Accept    = nftables.ChainPolicyAccept
)

func ApplySNAT(podIP, vipIP, service string, ignoreCIDR []string, IPv6 bool) error {

	conn, err := nftables.New()
	if err != nil {
		return err
	}

	err = Flush(conn, IPv6, service) // TODO: pass in the type of table we need to concern ourselves with
	if err != nil {
		return err
	}

	rule, err := CreateRule(podIP, vipIP, service, ignoreCIDR, conn, IPv6)
	if err != nil {
		return err
	}
	slog.Debug("[egress]", "table", rule.Table.Name, "chain", rule.Chain.Name, "expr", rule.Exprs)
	conn.AddRule(rule) // Add the rule

	err = conn.Flush() // Commit the rule to nftables
	if err != nil {
		return err
	}

	return conn.CloseLasting() // Close out any remaining netlink communication
}

func DeleteSNAT(IPv6 bool, service string) error {
	conn, err := nftables.New()
	if err != nil {
		return err
	}
	var chainName string
	if IPv6 {
		chainName = fmt.Sprintf(SNatChain, "v6", service)
	} else {
		chainName = fmt.Sprintf(SNatChain, "v4", service)
	}

	chains, err := conn.ListChains()
	if err != nil {
		return err
	}
	slog.Info("[egress]", "Looking for", chainName)
	for x := range chains {
		slog.Debug("[egress]", "found chain", chains[x].Name)
		// fmt.Printf("[egress] found chain [%s] looking for [%s]", chains[x].Name, chainName)
		if chains[x].Name == chainName {
			slog.Info("[egress]", "Deleting chain", chainName)
			conn.DelChain(chains[x])
			return conn.Flush()
		}
	}
	return fmt.Errorf("Unable to find chain [%s]", chainName)
}

func GetTable(IPv6 bool) *nftables.Table {
	var tableName string
	if IPv6 {
		tableName = fmt.Sprintf(NatTable, "v6")
	} else {
		tableName = fmt.Sprintf(NatTable, "v4")
	}
	// Default to IPv4
	table := &nftables.Table{
		Family: nftables.TableFamilyIPv4,
		Name:   tableName,
	}

	// Move to IPv6 if needed
	if IPv6 {
		table.Family = nftables.TableFamilyIPv6
	}
	return table
}

func GetSNatChain(IPv6 bool, service string) *nftables.Chain {
	var chainName string
	if IPv6 {
		chainName = fmt.Sprintf(SNatChain, "v6", service)
	} else {
		chainName = fmt.Sprintf(SNatChain, "v4", service)
	}
	policy := nftables.ChainPolicyAccept
	return &nftables.Chain{
		Name:     chainName,
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

func Flush(conn *nftables.Conn, IPv6 bool, service string) error {
	var tableName string
	if IPv6 {
		tableName = fmt.Sprintf(NatTable, "v6")
	} else {
		tableName = fmt.Sprintf(NatTable, "v4")
	}
	if t, err := FilterTable(conn, tableName); err != nil {
		return err
	} else if t != nil {
		conn.DelTable(t)
	}

	// These don't return errors, so not 100% sure how to guarantee things were created
	conn.AddTable(GetTable(IPv6))
	conn.AddChain(GetSNatChain(IPv6, service))
	return conn.Flush()
}

func CreateRule(podIP, vipIP, service string, ignoreCIDR []string, conn *nftables.Conn, IPv6 bool) (*nftables.Rule, error) {

	// Get the kube-vip table
	table := GetTable(IPv6)

	// Create our rule
	rule := &nftables.Rule{
		Table: table,
		Exprs: []expr.Any{},
	}
	// Set the correct chain
	rule.Chain = GetSNatChain(IPv6, service)

	// original pod IP
	if net.ParseIP(podIP) == nil {
		return nil, errors.New("ip is invalid")
	}

	// Create an element using our pod IP
	elements := []nftables.SetElement{}
	if IPv6 {
		elements = append(elements, nftables.SetElement{Key: net.ParseIP(podIP).To16()})
	} else {
		elements = append(elements, nftables.SetElement{Key: net.ParseIP(podIP).To4()})
	}

	set := &nftables.Set{
		Table:     table,
		Anonymous: true,
		Constant:  true,
		KeyType:   nftables.TypeIPAddr,
		Interval:  false,
	}
	if IPv6 {
		set.KeyType = nftables.TypeIP6Addr
	} else {
		set.KeyType = nftables.TypeIPAddr
	}

	// Add the set
	err := conn.AddSet(set, elements)
	if err != nil {
		return nil, err
	}

	// Create the expression using the set
	expression := []expr.Any{}

	payload := &expr.Payload{
		OperationType:  expr.PayloadLoad,
		Base:           expr.PayloadBaseNetworkHeader,
		DestRegister:   1,
		SourceRegister: 0,
	}

	// Set the length of the data based upon the type of IP being used
	if IPv6 {
		payload.Offset = 8
		payload.Len = 16
	} else {
		payload.Offset = 12
		payload.Len = 4
	}
	lookup := &expr.Lookup{
		SourceRegister: 1,
		DestRegister:   0,
		SetID:          set.ID,
	}
	// Add expressions
	expression = append(expression, payload)
	expression = append(expression, lookup)

	// Add expression to the rule
	rule.Exprs = append(rule.Exprs, expression...)

	for _, cidr := range ignoreCIDR {
		start, end, err := nftables.NetFirstAndLastIP(cidr)
		if err != nil {
			return nil, err
		}
		expression = []expr.Any{}

		payload := &expr.Payload{
			DestRegister: 1,
			Base:         expr.PayloadBaseNetworkHeader,
		}
		notEqualRange := &expr.Range{
			Op:       expr.CmpOpNeq,
			Register: 1,
		}

		if IPv6 {
			payload.Len = 16
			payload.Offset = 24
			notEqualRange.FromData = start.To16()
			notEqualRange.ToData = end.To16()
		} else {
			payload.Offset = 16
			payload.Len = 4
			notEqualRange.FromData = start.To4()
			notEqualRange.ToData = end.To4()
		}
		// Add expressions
		expression = append(expression, payload)
		expression = append(expression, notEqualRange)

		// // Add expression to the rule
		rule.Exprs = append(rule.Exprs, expression...)
	}

	// Create SNAT rule
	if net.ParseIP(vipIP) == nil {
		return nil, errors.New("output_ip is not a valid ip")
	}

	expression = []expr.Any{}

	immediate := &expr.Immediate{
		Register: 1,
		//Data:     net.ParseIP(vipIP).To4(),
	}
	nat := &expr.NAT{
		Type:        expr.NATTypeSourceNAT,
		RegAddrMin:  1,
		RegAddrMax:  1,
		RegProtoMin: 0,
		RegProtoMax: 0,
		Random:      false, FullyRandom: false, Persistent: false, Prefix: false,
	}
	if IPv6 {
		immediate.Data = net.ParseIP(vipIP).To16()
		nat.Family = unix.NFPROTO_IPV6
	} else {
		immediate.Data = net.ParseIP(vipIP).To4()
		nat.Family = unix.NFPROTO_IPV4
	}
	// https://github.com/google/nftables/blob/main/nftables_test.go#L5375
	// Add expressions
	expression = append(expression, immediate)
	expression = append(expression, nat)
	rule.Exprs = append(rule.Exprs, expression...)

	return rule, nil
}
