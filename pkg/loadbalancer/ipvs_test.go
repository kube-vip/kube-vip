package loadbalancer

import (
	"net/netip"
	"reflect"
	"testing"

	"github.com/cloudflare/ipvs"
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
			name: "IPv4",
			args: args{
				address: "192.168.0.20",
			},
			want:  netip.AddrFrom4([4]byte{192, 168, 0, 20}),
			want1: ipvs.INET,
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
