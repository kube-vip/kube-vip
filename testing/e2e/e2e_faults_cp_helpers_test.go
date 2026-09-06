//go:build e2e

package e2e_test

import (
	"strings"
	"testing"
)

func TestControlPlaneElectionMode(t *testing.T) {
	arp := controlPlaneElectionMode(ModeARP)
	if !arp.enabled || arp.leaseName != faultLeaseName {
		t.Fatalf("ARP election = %+v, want enabled lease %q", arp, faultLeaseName)
	}

	rt := controlPlaneElectionMode(ModeRT)
	if rt.enabled || rt.leaseName != "" {
		t.Fatalf("RT election = %+v, want disabled", rt)
	}
}

func TestValidateARPFailover(t *testing.T) {
	tests := []struct {
		name      string
		oldLeader string
		newLeader string
		owners    []string
		wantError string
	}{
		{name: "complete", oldLeader: "node-a", newLeader: "node-b", owners: []string{"node-b"}},
		{name: "same leader recovered", oldLeader: "node-a", newLeader: "node-a", owners: []string{"node-a"}},
		{name: "stale old owner", oldLeader: "node-a", newLeader: "node-b", owners: []string{"node-a", "node-b"}, wantError: "stale VIP ownership"},
		{name: "VIP missing", oldLeader: "node-a", newLeader: "node-b", wantError: "has no owner"},
		{name: "wrong owner", oldLeader: "node-a", newLeader: "node-b", owners: []string{"node-c"}, wantError: "does not own VIP"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateARPFailover(test.oldLeader, test.newLeader, test.owners)
			if test.wantError == "" && err != nil {
				t.Fatalf("validateARPFailover() error = %v", err)
			}
			if test.wantError != "" && (err == nil || !strings.Contains(err.Error(), test.wantError)) {
				t.Fatalf("validateARPFailover() error = %v, want substring %q", err, test.wantError)
			}
		})
	}
}

func TestValidateARPTransfer(t *testing.T) {
	tests := []struct {
		name      string
		newLeader string
		owners    []string
		wantError string
	}{
		{name: "sole replacement owner", newLeader: "node-b", owners: []string{"node-b"}},
		{name: "stale former owner allowed while kubelet stopped", newLeader: "node-b", owners: []string{"node-a", "node-b"}},
		{name: "replacement missing", newLeader: "node-b", owners: []string{"node-a"}, wantError: "does not own VIP"},
		{name: "VIP missing", newLeader: "node-b", wantError: "does not own VIP"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateARPTransfer(test.newLeader, test.owners)
			if test.wantError == "" && err != nil {
				t.Fatalf("validateARPTransfer() error = %v", err)
			}
			if test.wantError != "" && (err == nil || !strings.Contains(err.Error(), test.wantError)) {
				t.Fatalf("validateARPTransfer() error = %v, want substring %q", err, test.wantError)
			}
		})
	}
}
