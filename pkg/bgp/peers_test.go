package bgp

import (
	"reflect"
	"testing"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
)

func TestParseBGPPeerConfig(t *testing.T) {
	type args struct {
		config string
	}
	tests := []struct {
		name         string
		args         args
		wantBgpPeers []kubevip.BGPPeer
		wantErr      bool
	}{
		{
			name: "IPv4, default port",
			args: args{config: "192.168.0.10:65000::false,192.168.0.11:65000::false"},
			wantBgpPeers: []kubevip.BGPPeer{
				{Address: "192.168.0.10", Port: 179, AS: 65000, MultiHop: false},
				{Address: "192.168.0.11", Port: 179, AS: 65000, MultiHop: false},
			},
		},
		{
			name: "IPv4, different port",
			args: args{config: "192.168.0.10:65000::false:180,192.168.0.11:65000::false:190"},
			wantBgpPeers: []kubevip.BGPPeer{
				{Address: "192.168.0.10", Port: 180, AS: 65000, MultiHop: false},
				{Address: "192.168.0.11", Port: 190, AS: 65000, MultiHop: false},
			},
		},
		{
			name: "IPv6, multi-protocol",
			args: args{config: "[fd00:1111:2222:3333:c7d9:7235:6bf7:5d52]:65501::false/mpbgp_nexthop=auto_sourceif"},
			wantBgpPeers: []kubevip.BGPPeer{
				{Address: "fd00:1111:2222:3333:c7d9:7235:6bf7:5d52", Port: 179, AS: 65501, MultiHop: false, MpbgpNexthop: "auto_sourceif"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotBgpPeers, err := kubevip.ParseBGPPeerConfig(tt.args.config)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseBGPPeerConfig() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(gotBgpPeers, tt.wantBgpPeers) {
				t.Errorf("ParseBGPPeerConfig() = %v, want %v", gotBgpPeers, tt.wantBgpPeers)
			}
		})
	}
}
