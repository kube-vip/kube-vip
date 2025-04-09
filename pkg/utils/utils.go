package utils

import (
	"fmt"
	"net"
	"os"
	"strconv"
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

	// Check if the input is empty
	if rawIP == "" {
		return "", fmt.Errorf("no IP address provided")
	}
	if subnetMask == "" {
		return "", fmt.Errorf("no subnetMask provided")
	}
	ip := net.ParseIP(rawIP)
	if ip == nil {
		return "", fmt.Errorf("invalid IP address: %s", rawIP)
	}

	if value, err := strconv.Atoi(subnetMask); err != nil || value < 0 || value > 128 {
		return "", fmt.Errorf("invalid subnet mask: %s", subnetMask)
	}

	// Check if the input is valid
	return fmt.Sprintf("%s/%s", ip.String(), subnetMask), nil
}
