package nftables

import (
	"testing"

	googlenftables "github.com/google/nftables"
	"github.com/google/nftables/expr"
)

func TestRuleEqualRejectsRulesThatDiffer(t *testing.T) {
	// DEFECT: ruleEqual indexes the second expression slice without checking its length and ignores expression types it does not enumerate (pkg/nftables/client.go:324).
	table := &googlenftables.Table{Name: "kube_vip_v4"}
	chain := &googlenftables.Chain{Name: "kube_vip_snat_service", Table: table}

	tests := []struct {
		name string
		a    *googlenftables.Rule
		b    *googlenftables.Rule
	}{
		{
			name: "different expression lengths",
			a: &googlenftables.Rule{
				Table: table,
				Chain: chain,
				Exprs: []expr.Any{&expr.Meta{Key: expr.MetaKeyL4PROTO}},
			},
			b: &googlenftables.Rule{Table: table, Chain: chain},
		},
		{
			name: "unsupported expression values",
			a: &googlenftables.Rule{
				Table: table,
				Chain: chain,
				Exprs: []expr.Any{&expr.NAT{Type: expr.NATTypeSourceNAT}},
			},
			b: &googlenftables.Rule{
				Table: table,
				Chain: chain,
				Exprs: []expr.Any{&expr.NAT{Type: expr.NATTypeDestNAT}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if recovered := recover(); recovered != nil {
					t.Fatalf("ruleEqual panicked for unequal rules: %v", recovered)
				}
			}()

			if ruleEqual(tt.a, tt.b) {
				t.Fatal("ruleEqual reported unequal rules as equal")
			}
		})
	}
}
