package kubevip

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"slices"
	"strings"
)

const LeaseVIPsVersion = "v1"

// LeaseVIPKind distinguishes literal addresses from names that resolve to one.
type LeaseVIPKind string

const (
	LeaseVIPKindAddress LeaseVIPKind = "address"
	LeaseVIPKindName    LeaseVIPKind = "name"
)

type LeaseVIPsValue struct {
	Version      string     `json:"version"`
	InstanceName string     `json:"instance_name"`
	IFAProto     int        `json:"ifa_proto"`
	VIPs         []LeaseVIP `json:"vips"`
}

type LeaseVIP struct {
	Index int          `json:"index"`
	Value string       `json:"value"`
	Kind  LeaseVIPKind `json:"kind"`
}

func WithLeaseVIPs(annotations map[string]string, instanceName string, ifaProto int, vips []string) (map[string]string, error) {
	result := make(map[string]string, len(annotations)+1)
	for key, value := range annotations {
		result[key] = value
	}

	encoded, err := json.Marshal(LeaseVIPsValue{
		Version:      LeaseVIPsVersion,
		InstanceName: instanceName,
		IFAProto:     ifaProto,
		VIPs:         normalizeLeaseVIPs(vips),
	})
	if err != nil {
		return nil, fmt.Errorf("encode %s annotation: %w", LeaseVIPs, err)
	}
	result[LeaseVIPs] = string(encoded)
	return result, nil
}

func ParseLeaseVIPs(value string) (LeaseVIPsValue, error) {
	var parsed LeaseVIPsValue
	if err := json.Unmarshal([]byte(value), &parsed); err != nil {
		return LeaseVIPsValue{}, fmt.Errorf("decode %s annotation: %w", LeaseVIPs, err)
	}
	if parsed.Version != LeaseVIPsVersion {
		return LeaseVIPsValue{}, fmt.Errorf("unsupported %s annotation version %q", LeaseVIPs, parsed.Version)
	}
	for index, vip := range parsed.VIPs {
		if vip.Index != index {
			return LeaseVIPsValue{}, fmt.Errorf("invalid %s VIP index %d at position %d", LeaseVIPs, vip.Index, index)
		}
		switch vip.Kind {
		case LeaseVIPKindAddress, LeaseVIPKindName:
		default:
			return LeaseVIPsValue{}, fmt.Errorf("invalid %s VIP kind %q at index %d", LeaseVIPs, vip.Kind, vip.Index)
		}
	}
	return parsed, nil
}

func normalizeLeaseVIPs(values []string) []LeaseVIP {
	unique := make(map[string]struct{}, len(values))
	addresses := make([]string, 0, len(values))
	for _, value := range values {
		for candidate := range strings.SplitSeq(value, ",") {
			candidate = strings.TrimSpace(candidate)
			if candidate == "" {
				continue
			}
			if _, exists := unique[candidate]; exists {
				continue
			}
			unique[candidate] = struct{}{}
			addresses = append(addresses, candidate)
		}
	}
	// Sorting keeps the annotation byte-identical however callers happen to order VIPs.
	slices.SortFunc(addresses, compareLeaseVIPs)

	result := make([]LeaseVIP, 0, len(addresses))
	for _, address := range addresses {
		kind := LeaseVIPKindName
		if _, isAddress := leaseVIPAddress(address); isAddress {
			kind = LeaseVIPKindAddress
		}
		result = append(result, LeaseVIP{Index: len(result), Value: address, Kind: kind})
	}
	return result
}

// compareLeaseVIPs orders addresses numerically and ahead of names, which keeps VIPs like
// 10.0.0.2 and 10.0.0.10 in the order an operator expects. Values that are not addresses,
// such as DNS records, are kept and ordered lexically.
func compareLeaseVIPs(a, b string) int {
	addressA, isAddressA := leaseVIPAddress(a)
	addressB, isAddressB := leaseVIPAddress(b)
	switch {
	case isAddressA && isAddressB:
		if order := addressA.Compare(addressB); order != 0 {
			return order
		}
		// Distinct spellings of one address still need a stable order.
		return strings.Compare(a, b)
	case isAddressA:
		return -1
	case isAddressB:
		return 1
	default:
		return strings.Compare(a, b)
	}
}

func leaseVIPAddress(value string) (netip.Addr, bool) {
	if address, err := netip.ParseAddr(value); err == nil {
		return address.Unmap(), true
	}
	if prefix, err := netip.ParsePrefix(value); err == nil {
		return prefix.Addr().Unmap(), true
	}
	return netip.Addr{}, false
}
