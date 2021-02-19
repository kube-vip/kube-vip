package vip

import (
	"fmt"
	"net"
	"strings"

	"github.com/pkg/errors"
)

// LookupHost resolves dnsName and return an IP or an error
func lookupHost(dnsName string) (string, error) {
	addrs, err := net.LookupHost(dnsName)
	if err != nil {
		return "", err
	}
	if len(addrs) == 0 {
		return "", errors.Errorf("empty address for %s", dnsName)
	}
	return addrs[0], nil
}

// IsIP returns if address is an IP or not
func IsIP(address string) bool {
	ip := net.ParseIP(address)
	return ip != nil
}

// getHostName return the hostname from the fqdn
func getHostName(dnsName string) string {
	if dnsName == "" {
		return ""
	}

	fields := strings.Split(dnsName, ".")
	return fields[0]
}

// IsIPv4 returns true only if address is a valid IPv4 address
func IsIPv4(address string) bool {
	ip := net.ParseIP(address)
	if ip == nil {
		return false
	}
	return ip.To4() != nil
}

// IsIPv6 returns true only if address is a valid IPv6 address
func IsIPv6(address string) bool {
	ip := net.ParseIP(address)
	if ip == nil {
		return false
	}
	return ip.To4() == nil
}

// GetFullMask returns /32 for an IPv4 address and /128 for an IPv6 address
func GetFullMask(address string) (string, error) {
	if IsIPv4(address) {
		return "/32", nil
	}
	if IsIPv6(address) {
		return "/128", nil
	}
	return "", fmt.Errorf("failed to parse %s as either IPv4 or IPv6", address)
}
