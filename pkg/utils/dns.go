package utils

import (
	"fmt"
	"net"
	"strings"

	"github.com/pkg/errors"
)

const (
	IPv4Family = "IPv4"
	IPv6Family = "IPv6"
	DualFamily = "dual"
)

// LookupHost resolves dnsName and return an IP or an error
func LookupHost(dnsName, dnsMode string) ([]string, error) {
	result, err := net.LookupHost(dnsName)
	if err != nil {
		return nil, err
	}
	if len(result) == 0 {
		return nil, errors.Errorf("empty address for %s", dnsName)
	}
	addrs := []string{}
	switch dnsMode {
	case strings.ToLower(IPv4Family), strings.ToLower(IPv6Family), DualFamily:
		a, err := getIPbyFamily(result, dnsMode)
		if err != nil {
			return nil, err
		}
		addrs = append(addrs, a...)
	default:
		addrs = append(addrs, result[0])
	}

	return addrs, nil
}

func getIPbyFamily(addresses []string, family string) ([]string, error) {
	var checkers []func(string) bool
	families := []string{}
	if family == DualFamily || family == strings.ToLower(IPv4Family) {
		checkers = append(checkers, IsIPv4)
		families = append(families, IPv4Family)
	}
	if family == DualFamily || family == strings.ToLower(IPv6Family) {
		checkers = append(checkers, IsIPv6)
		families = append(families, IPv6Family)
	}

	addrs := []string{}
	for i, c := range checkers {
		addr, err := getIPbyChecker(addresses, c)
		if err != nil {
			return nil, fmt.Errorf("error getting %s address: %w", families[i], err)
		}
		addrs = append(addrs, addr)
	}

	return addrs, nil
}

func getIPbyChecker(addresses []string, checker func(string) bool) (string, error) {
	for _, addr := range addresses {
		if checker(addr) {
			return addr, nil
		}
	}
	return "", fmt.Errorf("address not found")
}
