package e2e

import "fmt"

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
