package e2e

import (
	"errors"
	"testing"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/utils/ptr"
)

func TestCheckCommonLeaseOwnershipStableHolder(t *testing.T) {
	present := map[string]string{"192.0.2.10": "node-a", "2001:db8::10": "node-a"}
	holder, err := CheckCommonLeaseOwnership(
		func() (string, error) { return "node-a", nil },
		[]string{"node-a", "node-b"},
		[]string{"192.0.2.10", "2001:db8::10"},
		func(address, node string) bool { return present[address] == node },
	)
	if err != nil {
		t.Fatalf("checkCommonLeaseOwnership() error = %v", err)
	}
	if holder != "node-a" {
		t.Fatalf("checkCommonLeaseOwnership() holder = %q, want node-a", holder)
	}
}

func TestCheckCommonLeaseOwnershipHolderMigration(t *testing.T) {
	holderReads := 0
	getHolder := func() (string, error) {
		holderReads++
		if holderReads == 1 {
			return "node-a", nil
		}
		return "node-b", nil
	}
	present := map[string]string{"192.0.2.10": "node-a"}
	hasAddress := func(address, node string) bool { return present[address] == node }

	if _, err := CheckCommonLeaseOwnership(getHolder, []string{"node-a", "node-b"}, []string{"192.0.2.10"}, hasAddress); err == nil {
		t.Fatal("checkCommonLeaseOwnership() accepted an ownership snapshot while the holder changed")
	}

	present["192.0.2.10"] = "node-b"
	holder, err := CheckCommonLeaseOwnership(getHolder, []string{"node-a", "node-b"}, []string{"192.0.2.10"}, hasAddress)
	if err != nil {
		t.Fatalf("checkCommonLeaseOwnership() after migration error = %v", err)
	}
	if holder != "node-b" {
		t.Fatalf("checkCommonLeaseOwnership() holder = %q, want node-b", holder)
	}
}

func TestCheckCommonLeaseOwnershipRejectsMissingHolder(t *testing.T) {
	_, err := CheckCommonLeaseOwnership(
		func() (string, error) { return "", nil },
		nil,
		nil,
		func(string, string) bool { return false },
	)
	if err == nil {
		t.Fatal("checkCommonLeaseOwnership() accepted an empty holder")
	}

	want := errors.New("get lease")
	_, err = CheckCommonLeaseOwnership(
		func() (string, error) { return "", want },
		nil,
		nil,
		func(string, string) bool { return false },
	)
	if !errors.Is(err, want) {
		t.Fatalf("checkCommonLeaseOwnership() error = %v, want %v", err, want)
	}
}

func TestCheckCommonLeaseRetired(t *testing.T) {
	getError := errors.New("get lease")
	tests := []struct {
		name    string
		lease   *coordinationv1.Lease
		err     error
		wantErr bool
	}{
		{name: "absent", err: apierrors.NewNotFound(coordinationv1.Resource("leases"), "common-lease")},
		{name: "nil holder", lease: &coordinationv1.Lease{}},
		{name: "empty holder", lease: &coordinationv1.Lease{Spec: coordinationv1.LeaseSpec{HolderIdentity: ptr.To("")}}},
		{name: "nonempty holder", lease: &coordinationv1.Lease{Spec: coordinationv1.LeaseSpec{HolderIdentity: ptr.To("node-a")}}, wantErr: true},
		{name: "error", err: getError, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckCommonLeaseRetired(tt.lease, tt.err)
			if (err != nil) != tt.wantErr {
				t.Fatalf("CheckCommonLeaseRetired() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.name == "error" && !errors.Is(err, getError) {
				t.Fatalf("CheckCommonLeaseRetired() error = %v, want wrapped %v", err, getError)
			}
		})
	}
}
