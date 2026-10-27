package e2e

import "testing"

func TestNftExcludesCIDR(t *testing.T) {
	chain := `chain kube_vip_snat_x {
		ip saddr 10.244.1.5 ip daddr != 10.244.0.0-10.244.0.255 ip daddr != 10.96.0.0/12 snat to 172.18.255.10
	}`
	tests := map[string]bool{
		"10.244.0.0/24": true,  // listed as a range
		"10.96.0.0/16":  true,  // covered by a wider prefix
		"10.244.1.0/24": false, // not excluded
		"10.244.0.0/16": false, // only partly excluded
		"garbage":       false,
	}
	for cidr, want := range tests {
		if got := NftExcludesCIDR(chain, cidr); got != want {
			t.Errorf("NftExcludesCIDR(%q) = %t, want %t", cidr, got, want)
		}
	}
}
