package kubevip

import (
	"net/netip"
	"reflect"
	"strings"
	"testing"

	api "github.com/osrg/gobgp/v4/api"
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

func TestParseBMPServers(t *testing.T) {
	tests := []struct {
		name    string
		servers []string
		want    []netip.AddrPort
		wantErr string
	}{
		{name: "none", servers: nil, want: []netip.AddrPort{}},
		{name: "IPv4 with default port", servers: []string{"192.0.2.20"}, want: []netip.AddrPort{netip.MustParseAddrPort("192.0.2.20:11019")}},
		{name: "IPv4 with port", servers: []string{"192.0.2.20:5000"}, want: []netip.AddrPort{netip.MustParseAddrPort("192.0.2.20:5000")}},
		{name: "IPv6 with default port", servers: []string{"[2001:db8::20]"}, want: []netip.AddrPort{netip.MustParseAddrPort("[2001:db8::20]:11019")}},
		{name: "IPv6 with port", servers: []string{"[2001:db8::20]:5000"}, want: []netip.AddrPort{netip.MustParseAddrPort("[2001:db8::20]:5000")}},
		{name: "surrounding whitespace", servers: []string{" 192.0.2.20 "}, want: []netip.AddrPort{netip.MustParseAddrPort("192.0.2.20:11019")}},
		{name: "same address on different ports", servers: []string{"192.0.2.20", "192.0.2.20:5000"}, want: []netip.AddrPort{
			netip.MustParseAddrPort("192.0.2.20:11019"), netip.MustParseAddrPort("192.0.2.20:5000"),
		}},
		{name: "empty", servers: []string{""}, wantErr: `BMP server "" is invalid`},
		{name: "host name", servers: []string{"bmp.example.com:11019"}, wantErr: `BMP server "bmp.example.com:11019" is invalid`},
		{name: "IPv6 without brackets", servers: []string{"2001:db8::20"}, wantErr: "must be enclosed in square brackets"},
		{name: "IPv4 in brackets", servers: []string{"[192.0.2.20]"}, wantErr: "must be enclosed in square brackets"},
		{name: "port zero", servers: []string{"192.0.2.20:0"}, wantErr: "port must be between 1 and 65535"},
		{name: "port out of range", servers: []string{"192.0.2.20:65536"}, wantErr: `BMP server "192.0.2.20:65536" is invalid`},
		{name: "duplicate", servers: []string{"192.0.2.20", "192.0.2.20:11019"}, wantErr: "BMP server 192.0.2.20:11019 is configured more than once"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseBMPServers(tt.servers)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ParseBMPServers() error = %v, want error containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseBMPServers() error = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("ParseBMPServers() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseBMPPolicy(t *testing.T) {
	tests := []struct {
		policy  string
		want    api.AddBmpRequest_MonitoringPolicy
		wantErr bool
	}{
		{policy: "", want: api.AddBmpRequest_MONITORING_POLICY_LOCAL},
		{policy: BMPPolicyPrePolicy, want: api.AddBmpRequest_MONITORING_POLICY_PRE},
		{policy: BMPPolicyPostPolicy, want: api.AddBmpRequest_MONITORING_POLICY_POST},
		{policy: BMPPolicyLocalRIB, want: api.AddBmpRequest_MONITORING_POLICY_LOCAL},
		{policy: BMPPolicyAll, want: api.AddBmpRequest_MONITORING_POLICY_ALL},
		{policy: " Local-RIB ", want: api.AddBmpRequest_MONITORING_POLICY_LOCAL},
		{policy: "both", wantErr: true},
		{policy: "local", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.policy, func(t *testing.T) {
			got, err := ParseBMPPolicy(tt.policy)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseBMPPolicy(%q) error = %v, wantErr %t", tt.policy, err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Fatalf("ParseBMPPolicy(%q) = %v, want %v", tt.policy, got, tt.want)
			}
		})
	}
}
