package manager

import (
	"fmt"
	"net"
	"slices"
	"strings"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
)

// FormatAddressWithSubnetMask formats an IP address with the appropriate CIDR based on the input.
// The input SubnetMasks can be "32,128" (dual-stack), "32", "128" (SingleStack).
func FormatAddressWithSubnetMask(rawIP string, subnetMasks string) (string, error) {
	// Split the SubnetMasks input into DualStack or SingleStack
	// If the input is "32,128", it will be split into ["32", "128"]
	subnetMasksParts := strings.Split(subnetMasks, ",")
	if len(subnetMasksParts) == 0 {
		return "", fmt.Errorf("no subnetMasks provided got: %q", subnetMasks)
	} else if len(subnetMasksParts) > 2 {
		return "", fmt.Errorf("invalid subnetMasks provided got: %q", subnetMasks)
	}
	if slices.Contains(subnetMasksParts, kubevip.Auto) {
		return "", fmt.Errorf("subnetMasks provided only works in ARP mode: %q", subnetMasks)
	}

	// Parse the raw IP address
	ip := net.ParseIP(rawIP)
	if ip == nil {
		return "", fmt.Errorf("invalid IP address: %s", rawIP)
	}
	if ip.To4() != nil {
		subnetMask := subnetMasksParts[0]
		return fmt.Sprintf("%s/%s", ip.String(), subnetMask), nil
	}
	if ip.To16() != nil {
		subnetMask := subnetMasksParts[0]
		if len(subnetMasksParts) == 2 {
			subnetMask = subnetMasksParts[1]
		}
		return fmt.Sprintf("%s/%s", ip.String(), subnetMask), nil
	}
	return "", fmt.Errorf("invalid IP address: %s", rawIP)
}
