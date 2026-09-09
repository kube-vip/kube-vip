//go:build e2e

package e2e

import "testing"

func TestBGPPeerValuesString(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		peer BGPPeerValues
		want string
	}{
		{name: "IPv4 explicit port", peer: BGPPeerValues{IP: "192.0.2.1", AS: 65000, Port: 1179}, want: "192.0.2.1:65000::false:1179"},
		{name: "IPv6 explicit port", peer: BGPPeerValues{IP: "2001:db8::1", AS: 65000, Port: 1179}, want: "[2001:db8::1]:65000::false:1179"},
		{name: "default port", peer: BGPPeerValues{IP: "192.0.2.1", AS: 65000}, want: "192.0.2.1:65000::false:179"},
		{name: "MP-BGP", peer: BGPPeerValues{IP: "192.0.2.1", AS: 65000, Port: 1179, MPBGP: "fixed"}, want: "192.0.2.1:65000::false:1179:mpbgp_nexthop=fixed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.peer.String(); got != test.want {
				t.Fatalf("String() = %q, want %q", got, test.want)
			}
		})
	}
}
