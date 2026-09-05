package nftables

import (
	"testing"

	googlenftables "github.com/google/nftables"
	"github.com/google/nftables/expr"
)

func TestRuleEqualExpressions(t *testing.T) {
	table := &googlenftables.Table{Name: "kube_vip_v4"}
	chain := &googlenftables.Chain{Name: "kube_vip_snat_service", Table: table}
	rule := func(expressions ...expr.Any) *googlenftables.Rule {
		return &googlenftables.Rule{Table: table, Chain: chain, Exprs: expressions}
	}

	tests := []struct {
		name  string
		a     *googlenftables.Rule
		b     *googlenftables.Rule
		equal bool
	}{
		{
			name: "first rule has more expressions",
			a:    rule(&expr.Meta{Key: expr.MetaKeyL4PROTO}),
			b:    rule(),
		},
		{
			name: "second rule has more expressions",
			a:    rule(),
			b:    rule(&expr.Meta{Key: expr.MetaKeyL4PROTO}),
		},
		{
			name:  "counter runtime state is ignored",
			a:     rule(&expr.Counter{Bytes: 10, Packets: 1}),
			b:     rule(&expr.Counter{Bytes: 20, Packets: 2}),
			equal: true,
		},
		{
			name: "counter type mismatch",
			a:    rule(&expr.Counter{}),
			b:    rule(&expr.Meta{}),
		},
		{
			name:  "lookup transient set ID is ignored",
			a:     rule(&expr.Lookup{SourceRegister: 1, DestRegister: 2, IsDestRegSet: true, SetID: 10, SetName: "service-map", Invert: true}),
			b:     rule(&expr.Lookup{SourceRegister: 1, DestRegister: 2, IsDestRegSet: true, SetID: 20, SetName: "service-map", Invert: true}),
			equal: true,
		},
		{
			name: "lookup semantic fields differ",
			a:    rule(&expr.Lookup{SourceRegister: 1, SetID: 10, SetName: "service-set"}),
			b:    rule(&expr.Lookup{SourceRegister: 2, SetID: 10, SetName: "service-set"}),
		},
		{
			name: "lookup type mismatch",
			a:    rule(&expr.Lookup{}),
			b:    rule(&expr.Meta{}),
		},
		{
			name: "unknown expression values differ",
			a:    rule(&expr.NAT{Type: expr.NATTypeSourceNAT}),
			b:    rule(&expr.NAT{Type: expr.NATTypeDestNAT}),
		},
		{
			name:  "unknown expression values match",
			a:     rule(&expr.NAT{Type: expr.NATTypeSourceNAT}),
			b:     rule(&expr.NAT{Type: expr.NATTypeSourceNAT}),
			equal: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if recovered := recover(); recovered != nil {
					t.Fatalf("ruleEqual panicked for unequal rules: %v", recovered)
				}
			}()

			if got := ruleEqual(tt.a, tt.b); got != tt.equal {
				t.Fatalf("ruleEqual() = %t, want %t", got, tt.equal)
			}
		})
	}
}
