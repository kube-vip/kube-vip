package utils

import (
	"fmt"
	"net"
	"os"
)

func FileExists(filename string) bool {
	info, err := os.Stat(filename)
	if os.IsNotExist(err) {
		return false
	}
	return !info.IsDir()
}

// FormatIPWithSubnetMask takes a raw IP address and a subnet mask, and returns a formatted string in CIDR notation.
func FormatIPWithSubnetMask(rawIP string, subnetMask string) (string, error) {

	addr := fmt.Sprintf("%s/%s", rawIP, subnetMask)
	// Check if the input is valid
	_, _, err := net.ParseCIDR(addr)
	if err != nil {
		return "", fmt.Errorf("invalid CIDR: %q, %w", addr, err)
	}
	return addr, nil
}
