package nftables

import (
	"testing"

	googlenftables "github.com/google/nftables"
	"github.com/google/nftables/expr"
)

func TestEgressTableName(t *testing.T) {
	tests := []struct {
		name     string
		baseName string
		ipv6     bool
		want     string
	}{
		{name: "default IPv4", want: "kube_vip_v4"},
		{name: "default IPv6", ipv6: true, want: "kube_vip_v6"},
		{name: "release IPv4", baseName: "kube_vip_release_a", want: "kube_vip_release_a_v4"},
		{name: "namespace IPv6", baseName: "kube-vip-networking", ipv6: true, want: "kube-vip-networking_v6"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := egressTableName(tt.baseName, tt.ipv6); got != tt.want {
				t.Fatalf("egressTableName(%q, %t) = %q, want %q", tt.baseName, tt.ipv6, got, tt.want)
			}
		})
	}
}

func TestGetEgressTable(t *testing.T) {
	table := GetEgressTable(false, "release_a")
	if table.Name != "release_a_v4" {
		t.Fatalf("table name = %q, want %q", table.Name, "release_a_v4")
	}
}

func TestEgressTableBaseName(t *testing.T) {
	if got := EgressTableBaseName(""); got != DefaultEgressTableName {
		t.Fatalf("EgressTableBaseName(\"\") = %q, want %q", got, DefaultEgressTableName)
	}
	if got := EgressTableBaseName("release_a"); got != "release_a" {
		t.Fatalf("EgressTableBaseName(\"release_a\") = %q, want %q", got, "release_a")
	}
}

func TestEgressTableBaseNameForInstance(t *testing.T) {
	if got := EgressTableBaseNameForInstance(""); got != DefaultEgressTableName {
		t.Fatalf("EgressTableBaseNameForInstance(\"\") = %q, want %q", got, DefaultEgressTableName)
	}
	if got := EgressTableBaseNameForInstance("release_a"); got != "kube_vip_release_a" {
		t.Fatalf("EgressTableBaseNameForInstance(\"release_a\") = %q, want %q", got, "kube_vip_release_a")
	}
}

func TestEgressTableNameForInstance(t *testing.T) {
	baseName := EgressTableBaseNameForInstance("release_a")
	if got := egressTableName(baseName, false); got != "kube_vip_release_a_v4" {
		t.Fatalf("IPv4 table name = %q, want %q", got, "kube_vip_release_a_v4")
	}
	if got := egressTableName(baseName, true); got != "kube_vip_release_a_v6" {
		t.Fatalf("IPv6 table name = %q, want %q", got, "kube_vip_release_a_v6")
	}
}

func TestShouldDeleteSNATChain(t *testing.T) {
	tests := []struct {
		name      string
		chain     *googlenftables.Chain
		keepTable string
		want      bool
	}{
		{
			name: "stale table for matching Service UID",
			chain: &googlenftables.Chain{
				Name:  "kube_vip_snat_service-a",
				Table: &googlenftables.Table{Name: "release_a_v4"},
			},
			keepTable: "release_b_v4",
			want:      true,
		},
		{
			name: "current table for matching Service UID",
			chain: &googlenftables.Chain{
				Name:  "kube_vip_snat_service-a",
				Table: &googlenftables.Table{Name: "release_b_v4"},
			},
			keepTable: "release_b_v4",
			want:      false,
		},
		{
			name: "different Service UID in stale table",
			chain: &googlenftables.Chain{
				Name:  "kube_vip_snat_service-b",
				Table: &googlenftables.Table{Name: "release_a_v4"},
			},
			keepTable: "release_b_v4",
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldDeleteSNATChain(tt.chain, "kube_vip_snat_service-a", tt.keepTable); got != tt.want {
				t.Fatalf("shouldDeleteSNATChain() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestBuildSNATRulesUsesDistinctPortSets(t *testing.T) {
	conn, err := googlenftables.New()
	if err != nil {
		t.Fatalf("failed to create nftables connection: %v", err)
	}
	rules, err := buildSNATRules(
		conn,
		"10.244.0.10",
		"192.0.2.10",
		"service-uid",
		"tcp:5060,udp:5060",
		nil,
		[]string{"198.51.100.0/24", "203.0.113.0/24"},
		false,
		"",
	)
	if err != nil {
		t.Fatalf("buildSNATRules() returned an error: %v", err)
	}
	if len(rules) != 4 {
		t.Fatalf("buildSNATRules() returned %d rules, want 4", len(rules))
	}

	setIDs := make(map[uint32]struct{}, len(rules))
	for i, rule := range rules {
		portSetID := destinationPortSetID(t, rule)
		if portSetID == 0 {
			t.Fatalf("rule %d does not contain a destination-port set lookup", i)
		}
		if _, exists := setIDs[portSetID]; exists {
			t.Fatalf("destination-port set ID %d is reused by multiple rules", portSetID)
		}
		setIDs[portSetID] = struct{}{}
	}
}

func destinationPortSetID(t *testing.T, rule *googlenftables.Rule) uint32 {
	t.Helper()

	for i, expression := range rule.Exprs {
		payload, ok := expression.(*expr.Payload)
		if !ok || payload.Base != expr.PayloadBaseTransportHeader || payload.Offset != 2 || payload.Len != 2 {
			continue
		}
		if i+1 >= len(rule.Exprs) {
			t.Fatal("destination-port payload is not followed by a lookup expression")
		}
		lookup, ok := rule.Exprs[i+1].(*expr.Lookup)
		if !ok {
			t.Fatal("destination-port payload is not followed by a lookup expression")
		}
		return lookup.SetID
	}

	t.Fatal("rule does not contain a destination-port payload")
	return 0
}
