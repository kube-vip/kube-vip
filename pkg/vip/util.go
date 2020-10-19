package vip

import (
	"net"
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