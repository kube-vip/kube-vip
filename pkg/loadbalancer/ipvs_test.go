package loadbalancer

import (
	"net/netip"
	"reflect"
	"testing"

	"github.com/cloudflare/ipvs"
	"github.com/kube-vip/kube-vip/pkg/utils"
)

func Test_ipAndFamily(t *testing.T) {
	type args struct {
		address string
	}
	tests := []struct {
		name  string
		args  args
		want  netip.Addr
		want1 ipvs.AddressFamily
	}{
		{
			name: utils.IPv4Family,
			args: args{
				address: "192.168.0.20",
			},
			want:  netip.AddrFrom4([4]byte{192, 168, 0, 20}),
			want1: ipvs.INET,
		},
		{
			name: utils.IPv6Family,
			args: args{
				address: "ff02::3",
			},
			want:  netip.AddrFrom16([16]byte{0: 0xff, 1: 0x02, 15: 0x03}),
			want1: ipvs.INET6,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, got1 := ipAndFamily(tt.args.address)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ipAndFamily() got = %v, want %v", got, tt.want)
			}
			if !reflect.DeepEqual(got1, tt.want1) {
				t.Errorf("ipAndFamily() got1 = %v, want %v", got1, tt.want1)
			}
		})
	}
}
