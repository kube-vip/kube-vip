package e2e

import (
	"fmt"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// CheckCommonLeaseOwnership verifies a stable lease holder and its exclusive datapath ownership.
func CheckCommonLeaseOwnership(getHolder func() (string, error), nodes, addresses []string, hasAddress func(string, string) bool) (string, error) {
	holder, err := getHolder()
	if err != nil {
		return "", err
	}
	if holder == "" {
		return "", fmt.Errorf("common lease has no holder")
	}

	for _, node := range nodes {
		for _, address := range addresses {
			expected := node == holder
			if hasAddress(address, node) != expected {
				return "", fmt.Errorf("address %q presence on node %q does not match lease holder %q", address, node, holder)
			}
		}
	}

	currentHolder, err := getHolder()
	if err != nil {
		return "", err
	}
	if currentHolder != holder {
		return "", fmt.Errorf("common lease holder changed from %q to %q while checking ownership", holder, currentHolder)
	}

	return holder, nil
}

// CheckCommonLeaseRetired accepts a deleted lease or one without a holder.
func CheckCommonLeaseRetired(lease *coordinationv1.Lease, err error) error {
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get common lease: %w", err)
	}
	if lease.Spec.HolderIdentity != nil && *lease.Spec.HolderIdentity != "" {
		return fmt.Errorf("common lease is still held by %q", *lease.Spec.HolderIdentity)
	}

	return nil
}
