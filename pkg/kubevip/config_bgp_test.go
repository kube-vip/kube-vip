package kubevip

import (
	"reflect"
	"testing"
)

func TestParseBGPPeerConfig(t *testing.T) {
	type args struct {
		config string
	}
	tests := []struct {
		name         string
		args         args
		wantBgpPeers []BGPPeer
		wantErr      bool
	}{

		{
			name: "IPv4, default port",
			args: args{config: "192.168.0.10:65000::false,192.168.0.11:65000::false"},
			wantBgpPeers: []BGPPeer{
				{Address: "192.168.0.10", Port: 179, AS: 65000, MultiHop: false, BFDEnabled: false, BFDReceiveInterval: 300, BFDTransmitInterval: 300, BFDDetectMultiplier: 3},
				{Address: "192.168.0.11", Port: 179, AS: 65000, MultiHop: false, BFDEnabled: false, BFDReceiveInterval: 300, BFDTransmitInterval: 300, BFDDetectMultiplier: 3},
			},
		},
		{
			name: "IPv4, different port",
			args: args{config: "192.168.0.10:65000::false:180,192.168.0.11:65000::false:190"},
			wantBgpPeers: []BGPPeer{
				{Address: "192.168.0.10", Port: 180, AS: 65000, MultiHop: false, BFDEnabled: false, BFDReceiveInterval: 300, BFDTransmitInterval: 300, BFDDetectMultiplier: 3},
				{Address: "192.168.0.11", Port: 190, AS: 65000, MultiHop: false, BFDEnabled: false, BFDReceiveInterval: 300, BFDTransmitInterval: 300, BFDDetectMultiplier: 3},
			},
		},
		{
			name: "IPv6, multi-protocol",
			args: args{config: "[fd00:1111:2222:3333:c7d9:7235:6bf7:5d52]:65501::false::mpbgp_nexthop=auto_sourceif"},
			wantBgpPeers: []BGPPeer{
				{Address: "fd00:1111:2222:3333:c7d9:7235:6bf7:5d52", Port: 179, AS: 65501, MultiHop: false, MpbgpNexthop: "auto_sourceif", BFDEnabled: false, BFDReceiveInterval: 300, BFDTransmitInterval: 300, BFDDetectMultiplier: 3},
			},
		},
		{
			name: "IPv6, multi-protocol, BFD, no multi-protocol options",
			args: args{config: "[fd00:1111:2222:3333:c7d9:7235:6bf7:5d52]:65501::false:::true;300;300;3"},
			wantBgpPeers: []BGPPeer{
				{Address: "fd00:1111:2222:3333:c7d9:7235:6bf7:5d52", Port: 179, AS: 65501, MultiHop: false, BFDEnabled: true, BFDReceiveInterval: 300, BFDTransmitInterval: 300, BFDDetectMultiplier: 3},
			},
		},
		{
			name: "IPv6, multi-protocol, BFD",
			args: args{config: "[fd00:1111:2222:3333:c7d9:7235:6bf7:5d52]:65501::false::mpbgp_nexthop=auto_sourceif:true;300;300;3"},
			wantBgpPeers: []BGPPeer{
				{Address: "fd00:1111:2222:3333:c7d9:7235:6bf7:5d52", Port: 179, AS: 65501, MultiHop: false, MpbgpNexthop: "auto_sourceif", BFDEnabled: true, BFDReceiveInterval: 300, BFDTransmitInterval: 300, BFDDetectMultiplier: 3},
			},
		},
		{
			name: "IPv6 bracketed, with password and multihop",
			args: args{config: "[fd00:100:64::2]:65000:secret:true"},
			wantBgpPeers: []BGPPeer{
				{Address: "fd00:100:64::2", Port: 179, AS: 65000, Password: "secret", MultiHop: true, BFDEnabled: false, BFDReceiveInterval: 300, BFDTransmitInterval: 300, BFDDetectMultiplier: 3},
			},
		},
		{
			name: "IPv6 bracketed, empty fields",
			args: args{config: "[fd00:100:64::2]:65000::false"},
			wantBgpPeers: []BGPPeer{
				{Address: "fd00:100:64::2", Port: 179, AS: 65000, MultiHop: false, BFDEnabled: false, BFDReceiveInterval: 300, BFDTransmitInterval: 300, BFDDetectMultiplier: 3},
			},
		},
		{
			name: "Unnumbered",
			args: args{config: "unnumbered:eth0,unnumbered:eth1:65000::true::mpbgp_nexthop=auto_sourceif"},
			wantBgpPeers: []BGPPeer{
				{Interface: "eth0", MultiHop: false, BFDEnabled: false, BFDReceiveInterval: 300, BFDTransmitInterval: 300, BFDDetectMultiplier: 3},
				{Interface: "eth1", Port: 179, AS: 65000, MultiHop: true, MpbgpNexthop: "auto_sourceif", BFDEnabled: false, BFDReceiveInterval: 300, BFDTransmitInterval: 300, BFDDetectMultiplier: 3},
			},
		},
		{
			name:    "Completely empty config",
			args:    args{config: ""},
			wantErr: true,
		},
		{
			name:    "Completely empty config (but with the seperators)",
			args:    args{config: ":::::::"},
			wantErr: true,
		},
		{
			name:    "Malformed parameter (no value)",
			args:    args{config: "1.2.3.4:65000/mpbgp_nexthop"},
			wantErr: true,
		},
		{
			name:    "Unsupported parameter",
			args:    args{config: "1.2.3.4:65000;unknown=value"},
			wantErr: true,
		},
		{
			name:    "Malformed IPv6 (no matching bracket)",
			args:    args{config: "[fd00:100:64::2:65000"},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotBgpPeers, err := ParseBGPPeerConfig(tt.args.config)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseBGPPeerConfig() error = \n%v, wantErr \n%v %v", err, tt.wantErr, gotBgpPeers)
				return
			}
			if !reflect.DeepEqual(gotBgpPeers, tt.wantBgpPeers) {
				t.Errorf("ParseBGPPeerConfig() = \n%v, want \n%v", gotBgpPeers, tt.wantBgpPeers)
			}
		})
	}
}
