package vip

import (
	"testing"
)

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
