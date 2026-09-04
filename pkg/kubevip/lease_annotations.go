package kubevip

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
)

const LeaseVIPsVersion = "v1"

type LeaseVIPsValue struct {
	Version      string     `json:"version"`
	InstanceName string     `json:"instance_name"`
	IFAProto     int        `json:"ifa_proto"`
	VIPs         []LeaseVIP `json:"vips"`
}

type LeaseVIP struct {
	Index int    `json:"index"`
	Value string `json:"value"`
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
		address, err := parseLeaseVIP(vip.Value)
		if err != nil {
			return LeaseVIPsValue{}, fmt.Errorf("invalid %s VIP at index %d: %w", LeaseVIPs, vip.Index, err)
		}
		parsed.VIPs[index].Value = address
	}
	return parsed, nil
}

func normalizeLeaseVIPs(values []string) []LeaseVIP {
	unique := make(map[string]struct{})
	result := make([]LeaseVIP, 0, len(values))
	for _, value := range values {
		for candidate := range strings.SplitSeq(value, ",") {
			candidate = strings.TrimSpace(candidate)
			address, err := parseLeaseVIP(candidate)
			if err != nil {
				continue
			}
			if _, exists := unique[address]; exists {
				continue
			}
			unique[address] = struct{}{}
			result = append(result, LeaseVIP{Index: len(result), Value: address})
		}
	}
	return result
}

func parseLeaseVIP(value string) (string, error) {
	address, err := netip.ParseAddr(value)
	if err == nil {
		return address.Unmap().String(), nil
	}
	prefix, prefixErr := netip.ParsePrefix(value)
	if prefixErr != nil {
		return "", fmt.Errorf("parse address %q: %w", value, err)
	}
	return prefix.Addr().Unmap().String(), nil
}
