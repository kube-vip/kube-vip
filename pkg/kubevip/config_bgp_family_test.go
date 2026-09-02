package kubevip

import (
	"testing"

	api "github.com/osrg/gobgp/v4/api"
)

func TestFindMpbgpAddressesRejectsFixedAddressFamilyMismatches(t *testing.T) {
	tests := []struct {
		name string
		peer BGPPeer
	}{
		{
			name: "IPv6 value in IPv4 field",
			peer: BGPPeer{
				MpbgpNexthop: "fixed",
				MpbgpIPv4:    "2001:db8::20",
			},
		},
		{
			name: "IPv4 value in IPv6 field",
			peer: BGPPeer{
				MpbgpNexthop: "fixed",
				MpbgpIPv6:    "192.0.2.20",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := tt.peer.FindMpbgpAddresses(&api.Peer{Transport: &api.Transport{}}, &BGPConfig{})
			if err == nil {
				t.Fatal("FindMpbgpAddresses() error = nil, want address-family error")
			}
		})
	}
}
