package loadbalancer

import (
	"net/netip"
	"reflect"
	"testing"

	"github.com/cloudflare/ipvs"
	"github.com/kube-vip/kube-vip/pkg/backend"
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

func TestBackendMapFindUsesCanonicalIdentity(t *testing.T) {
	first, err := backend.New(&backend.Config{
		Type:    backend.Discovery,
		Address: "::1",
		Port:    6443,
	})
	if err != nil {
		t.Fatalf("creating first backend: %v", err)
	}
	backends := backend.Map{}
	backends.Add(first, false)
	if got := backends.Find(backend.Discovery, "[::1]", 6443); got == nil || got.Backend != first {
		t.Fatalf("Find() = %v, want the existing backend", got)
	}
	if got := backends.Find(backend.Discovery, "::1", 6444); got != nil {
		t.Errorf("Find() with a different port = %v, want nil", got)
	}
	if got := backends.Find(backend.HTTP, "::1", 6443); got != nil {
		t.Errorf("Find() with a different backend type = %v, want nil", got)
	}
}

func TestBackendKeyPreservesHostnameAndURL(t *testing.T) {
	tests := []struct {
		name        string
		address     string
		wantAddress string
	}{
		{
			name:        "hostname",
			address:     "localhost",
			wantAddress: "localhost",
		},
		{
			name:        "URL with path",
			address:     "https://localhost:6443/livez",
			wantAddress: "https://localhost:6443/livez",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key := backend.NewKey(backend.HTTP, tt.address, 0)
			if key.Address != tt.wantAddress {
				t.Errorf("key address = %q, want %q", key.Address, tt.wantAddress)
			}
		})
	}
}
