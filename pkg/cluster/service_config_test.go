package cluster

import (
	"testing"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
)

func TestShouldAddServiceIP(t *testing.T) {
	tests := []struct {
		name   string
		config *kubevip.Config
		want   bool
	}{
		{
			name:   "BGP default does not attach IP",
			config: &kubevip.Config{EnableBGP: true},
			want:   false,
		},
		{
			name: "BGP opt-in attaches IP",
			config: &kubevip.Config{
				EnableBGP:              true,
				BGPAttachIPToInterface: true,
			},
			want: true,
		},
		{
			name: "routing table takes precedence",
			config: &kubevip.Config{
				EnableBGP:              true,
				BGPAttachIPToInterface: true,
				EnableRoutingTable:     true,
			},
			want: false,
		},
		{
			name: "WireGuard takes precedence",
			config: &kubevip.Config{
				EnableBGP:              true,
				BGPAttachIPToInterface: true,
				EnableWireguard:        true,
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldAddServiceIP(tt.config); got != tt.want {
				t.Fatalf("shouldAddServiceIP() = %t, want %t", got, tt.want)
			}
		})
	}
}
