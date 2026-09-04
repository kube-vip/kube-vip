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
		value.VIPs[0] != (LeaseVIP{Index: 0, Value: "2001:db8::10"}) ||
		value.VIPs[1] != (LeaseVIP{Index: 1, Value: "192.0.2.10"}) {
		t.Fatalf("Lease VIPs = %v, want indexed VIPs in configuration order", value.VIPs)
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
