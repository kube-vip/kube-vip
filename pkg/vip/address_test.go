package vip

import (
	"testing"

	"github.com/kube-vip/kube-vip/pkg/networkinterface"
)

func TestNewConfigTagsStaticAndUpdatedAddresses(t *testing.T) {
	const protocol = 248
	networks, err := NewConfig("192.0.2.10", "lo", false, "32", false, "", false, false, 0, 0, protocol,
		"", "", "", false, 0, false, networkinterface.NewManager(), false, false)
	if err != nil {
		t.Fatalf("NewConfig() error = %v", err)
	}
	configured, ok := networks[0].(*network)
	if !ok {
		t.Fatalf("network type = %T, want *network", networks[0])
	}
	if configured.address.Protocol != protocol {
		t.Fatalf("static address protocol = %d, want %d", configured.address.Protocol, protocol)
	}
	if err := configured.SetIP("192.0.2.11"); err != nil {
		t.Fatalf("SetIP() error = %v", err)
	}
	if configured.address.Protocol != protocol {
		t.Fatalf("updated address protocol = %d, want %d", configured.address.Protocol, protocol)
	}
}

func TestShouldSkipDAD(t *testing.T) {
	cases := []struct {
		name     string
		dadSkip  bool
		override bool
		want     bool
	}{
		{name: "default: DAD runs", dadSkip: false, override: false, want: false},
		{name: "DAD skip enabled", dadSkip: true, override: false, want: true},
		{name: "per-call override skips DAD", dadSkip: false, override: true, want: true},
		{name: "both set", dadSkip: true, override: true, want: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := &network{dadSkip: tc.dadSkip}
			if got := n.shouldSkipDAD(tc.override); got != tc.want {
				t.Fatalf("shouldSkipDAD(%v) with dadSkip=%v = %v, want %v", tc.override, tc.dadSkip, got, tc.want)
			}
		})
	}
}
