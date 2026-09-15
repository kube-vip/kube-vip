package kubevip

import "testing"

func TestWithLeaseVIPsEncodesVersionedInstanceOwnership(t *testing.T) {
	base := map[string]string{"example.test/preserved": "true", LeaseVIPs: "stale"}
	annotations, err := WithLeaseVIPs(base, "release_a", 248, []string{
		"2001:db8::10/128", "192.0.2.10", "192.0.2.10/32", "api.example.test",
	})
	if err != nil {
		t.Fatalf("WithLeaseVIPs() error = %v", err)
	}
	if annotations["example.test/preserved"] != "true" {
		t.Fatal("WithLeaseVIPs() dropped an existing annotation")
	}
	if base[LeaseVIPs] != "stale" {
		t.Fatal("WithLeaseVIPs() mutated the input annotations")
	}

	value, err := ParseLeaseVIPs(annotations[LeaseVIPs])
	if err != nil {
		t.Fatalf("ParseLeaseVIPs() error = %v", err)
	}
	if value.Version != LeaseVIPsVersion || value.InstanceName != "release_a" || value.IFAProto != 248 {
		t.Fatalf("Lease VIP metadata = %+v", value)
	}
	if len(value.VIPs) != 2 ||
		value.VIPs[0] != (LeaseVIP{Index: 0, Value: "192.0.2.10"}) ||
		value.VIPs[1] != (LeaseVIP{Index: 1, Value: "2001:db8::10"}) {
		t.Fatalf("Lease VIPs = %v, want indexed VIPs in canonical address order", value.VIPs)
	}
}

// The annotation is rewritten whenever a node starts campaigning, so the encoding
// has to be stable even when callers collect the same VIPs in a different order.
func TestWithLeaseVIPsIsIndependentOfInputOrder(t *testing.T) {
	first, err := WithLeaseVIPs(nil, "release_a", 248, []string{
		"2001:db8::10", "192.0.2.10", "10.0.0.2", "10.0.0.10",
	})
	if err != nil {
		t.Fatalf("WithLeaseVIPs() error = %v", err)
	}
	second, err := WithLeaseVIPs(nil, "release_a", 248, []string{
		"10.0.0.10", "192.0.2.10", "2001:db8::10", "10.0.0.2",
	})
	if err != nil {
		t.Fatalf("WithLeaseVIPs() error = %v", err)
	}
	if first[LeaseVIPs] != second[LeaseVIPs] {
		t.Fatalf("annotation changed with input order:\n%s\n%s", first[LeaseVIPs], second[LeaseVIPs])
	}

	value, err := ParseLeaseVIPs(first[LeaseVIPs])
	if err != nil {
		t.Fatalf("ParseLeaseVIPs() error = %v", err)
	}
	want := []string{"10.0.0.2", "10.0.0.10", "192.0.2.10", "2001:db8::10"}
	if len(value.VIPs) != len(want) {
		t.Fatalf("Lease VIPs = %v, want %v", value.VIPs, want)
	}
	for index, address := range want {
		if value.VIPs[index] != (LeaseVIP{Index: index, Value: address}) {
			t.Fatalf("Lease VIPs = %v, want %v", value.VIPs, want)
		}
	}
}

func TestParseLeaseVIPsRejectsUnknownVersion(t *testing.T) {
	if _, err := ParseLeaseVIPs(`{"version":"v2","instance_name":"release_a","ifa_proto":248,"vips":[]}`); err == nil {
		t.Fatal("ParseLeaseVIPs() accepted an unknown version")
	}
}

func TestParseLeaseVIPsRejectsOutOfOrderIndexes(t *testing.T) {
	if _, err := ParseLeaseVIPs(`{"version":"v1","instance_name":"release_a","ifa_proto":248,"vips":[{"index":1,"value":"192.0.2.10"}]}`); err == nil {
		t.Fatal("ParseLeaseVIPs() accepted an out-of-order VIP index")
	}
}
