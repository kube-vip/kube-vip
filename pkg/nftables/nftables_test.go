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

func TestBuildSNATRules(t *testing.T) {
	tests := []struct {
		name             string
		destinationPorts string
		allowedCIDRs     []string
		ipv6             bool
		tableName        string
		wantTableName    string
		wantRules        int
		wantPortSets     bool
	}{
		{
			name:      "no filters",
			wantRules: 1,
		},
		{
			name:         "multiple allowed CIDRs without destination ports",
			allowedCIDRs: []string{"198.51.100.0/24", "203.0.113.0/24"},
			wantRules:    2,
		},
		{
			name:             "multiple protocols without allowed CIDRs",
			destinationPorts: "tcp:5060,udp:5060",
			wantRules:        2,
			wantPortSets:     true,
		},
		{
			name:             "multiple allowed CIDRs with a destination port",
			destinationPorts: "tcp:5060",
			allowedCIDRs:     []string{"198.51.100.0/24", "203.0.113.0/24"},
			wantRules:        2,
			wantPortSets:     true,
		},
		{
			name:             "IPv6 custom table with multiple CIDRs and protocols",
			destinationPorts: "tcp:5060,udp:5060,sctp:5060",
			allowedCIDRs:     []string{"2001:db8:1::/64", "2001:db8:2::/64"},
			ipv6:             true,
			tableName:        "kube_vip_test",
			wantTableName:    "kube_vip_test_v6",
			wantRules:        6,
			wantPortSets:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			podIP, vipIP := "10.244.0.10", "192.0.2.10"
			if tt.ipv6 {
				podIP, vipIP = "fd00::10", "2001:db8::10"
			}

			conn, err := googlenftables.New()
			if err != nil {
				t.Fatalf("failed to create nftables connection: %v", err)
			}
			rules, err := buildSNATRules(
				conn,
				podIP,
				vipIP,
				"service-uid",
				tt.destinationPorts,
				nil,
				tt.allowedCIDRs,
				tt.ipv6,
				tt.tableName,
			)
			if err != nil {
				t.Fatalf("buildSNATRules() returned an error: %v", err)
			}
			if len(rules) != tt.wantRules {
				t.Fatalf("buildSNATRules() returned %d rules, want %d", len(rules), tt.wantRules)
			}

			setIDs := make(map[uint32]struct{}, len(rules))
			for i, rule := range rules {
				if tt.wantTableName != "" && rule.Table.Name != tt.wantTableName {
					t.Errorf("rule %d uses table %q, want %q", i, rule.Table.Name, tt.wantTableName)
				}

				portSetID, hasPortSet := destinationPortSetID(t, rule)
				if hasPortSet != tt.wantPortSets {
					t.Errorf("rule %d destination-port set presence = %t, want %t", i, hasPortSet, tt.wantPortSets)
				}
				if !hasPortSet {
					continue
				}
				if portSetID == 0 {
					t.Fatalf("rule %d has a destination-port set with ID 0", i)
				}
				if _, exists := setIDs[portSetID]; exists {
					t.Fatalf("destination-port set ID %d is reused by multiple rules", portSetID)
				}
				setIDs[portSetID] = struct{}{}
			}
		})
	}
}

func destinationPortSetID(t *testing.T, rule *googlenftables.Rule) (uint32, bool) {
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
		return lookup.SetID, true
	}

	return 0, false
}
