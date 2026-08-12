package utils

import (
	"fmt"
	"log/slog"
	"net"
	"strings"

	"github.com/pkg/errors"
)

const (
	IPv4Family = "IPv4"
	IPv6Family = "IPv6"
	DualFamily = "dual"
	// unused, but kept for documentation purposes, as it is used as user input
	FirstFamily = "first"
)

// LookupHost resolves dnsName and return an IP or an error
func LookupHost(dnsName, dnsMode string, requireDualStack bool) ([]string, error) {
	result, err := net.LookupHost(dnsName)
	if err != nil {
		return nil, err
	}
	if len(result) == 0 {
		return nil, errors.Errorf("empty address for %s", dnsName)
	}
	addrs := []string{}
	// we need to lowercase the dnsMode as in the end it is expected by internal functions
	// that expects the family to be lowercase, but we want to keep the original case for logging
	lowerDNSMode := strings.ToLower(dnsMode)
	switch lowerDNSMode {
	case strings.ToLower(IPv4Family), strings.ToLower(IPv6Family), strings.ToLower(DualFamily):
		a, err := getIPbyFamily(result, lowerDNSMode, requireDualStack)
		if err != nil {
			return nil, err
		}
		addrs = append(addrs, a...)
	// if the dnsMode is not one of the expected values,
	// we will return the `first` address found
	default:
		addrs = append(addrs, result[0])
	}

	return addrs, nil
}

func getIPbyFamily(addresses []string, family string, requireDualStack bool) ([]string, error) {
	var checkers []func(string) bool
	families := []string{}
	if strings.EqualFold(family, DualFamily) || strings.EqualFold(family, IPv4Family) {
		checkers = append(checkers, IsIPv4)
		families = append(families, IPv4Family)
	}
	if strings.EqualFold(family, DualFamily) || strings.EqualFold(family, IPv6Family) {
		checkers = append(checkers, IsIPv6)
		families = append(families, IPv6Family)
	}

	addrs := []string{}
	for i, c := range checkers {
		addr, err := getIPbyChecker(addresses, c)
		if err != nil {
			if len(checkers) > 1 && !requireDualStack {
				slog.Warn("no address found", "family", families[i])
				continue
			}
			return nil, fmt.Errorf("error getting %s address: %w", families[i], err)
		}
		addrs = append(addrs, addr)
	}

	if len(addrs) == 0 {
		return nil, fmt.Errorf("no addresses found")
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
