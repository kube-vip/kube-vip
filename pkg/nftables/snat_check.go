package nftables

import (
	"net"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
)

// HasSNATRules checks that the Service chain is attached to the expected hook
// and that all of its programmed SNAT rules are still present for each VIP.
// Read errors leave the state unknown.
func HasSNATRules(ipv6 bool, uid, table string, expected map[string]int) (bool, error) {
	want := GetSNatChainForTable(ipv6, uid, table)
	conn := &nftables.Conn{}
	chain, err := conn.ListChain(want.Table, want.Name)
	if err != nil {
		// ListChain does not preserve the underlying errno. Use a successful
		// listing to distinguish an absent chain from a failed lookup.
		chains, listErr := conn.ListChainsOfTableFamily(want.Table.Family)
		if listErr != nil {
			return false, listErr
		}
		for _, candidate := range chains {
			if candidate.Table.Name == want.Table.Name && candidate.Name == want.Name {
				return false, err
			}
		}
		return false, nil
	}
	if chain.Type != want.Type || chain.Hooknum == nil || *chain.Hooknum != *want.Hooknum ||
		chain.Priority == nil || *chain.Priority != *want.Priority {
		return false, nil
	}
	rules, err := conn.GetRules(want.Table, chain)
	if err != nil {
		return false, err
	}
	actual := make(map[string]int, len(expected))
	for _, rule := range rules {
		if len(rule.Exprs) < 2 {
			continue
		}
		target, targetOK := rule.Exprs[len(rule.Exprs)-2].(*expr.Immediate)
		nat, natOK := rule.Exprs[len(rule.Exprs)-1].(*expr.NAT)
		if targetOK && natOK && nat.Type == expr.NATTypeSourceNAT && nat.RegAddrMin == target.Register {
			actual[net.IP(target.Data).String()]++
		}
	}
	for vip, count := range expected {
		if actual[vip] < count {
			return false, nil
		}
	}
	return true, nil
}
