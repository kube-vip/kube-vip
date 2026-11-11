package bgp

import (
	"context"
	"reflect"
	"testing"

	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	api "github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/apiutil"
	bgp "github.com/osrg/gobgp/v4/pkg/packet/bgp"
)

func TestNewBGPServerRejectsInvalidCommunities(t *testing.T) {
	tests := []struct {
		name   string
		config kubevip.BGPConfig
	}{
		{name: "community", config: kubevip.BGPConfig{Communities: []string{"65000:65536"}}},
		{name: "large community", config: kubevip.BGPConfig{LargeCommunities: []string{"65000:1"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.config.AS = 65000
			tt.config.Peers = []kubevip.BGPPeer{{Address: "192.0.2.10", AS: 65001}}
			if _, err := NewBGPServer(tt.config, log.LevelError); err == nil {
				t.Fatal("NewBGPServer() error = nil, want an invalid community error")
			}
		})
	}
}

func TestAdvertisedPathsCarryConfiguredCommunities(t *testing.T) {
	tests := []struct {
		name                 string
		communities          []string
		largeCommunities     []string
		wantCommunities      []uint32
		wantLargeCommunities []*bgp.LargeCommunity
	}{
		{
			name: "no communities configured",
		},
		{
			name:             "communities and large communities",
			communities:      []string{"65000:100", "no-export"},
			largeCommunities: []string{"4200000000:1:2"},
			wantCommunities:  []uint32{65000<<16 | 100, uint32(bgp.COMMUNITY_NO_EXPORT)},
			wantLargeCommunities: []*bgp.LargeCommunity{
				bgp.NewLargeCommunity(4200000000, 1, 2),
			},
		},
		{
			name:             "large communities only",
			largeCommunities: []string{"65000:1:2", "65000:3:4"},
			wantLargeCommunities: []*bgp.LargeCommunity{
				bgp.NewLargeCommunity(65000, 1, 2), bgp.NewLargeCommunity(65000, 3, 4),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := newStartedTestBGPServer(t, kubevip.BGPConfig{
				AS:               65000,
				RouterID:         "192.0.2.1",
				Peers:            []kubevip.BGPPeer{{Address: "192.0.2.10", AS: 65001}},
				Communities:      tt.communities,
				LargeCommunities: tt.largeCommunities,
			})

			for _, route := range []struct {
				cidr   string
				family bgp.Family
			}{
				{cidr: "198.51.100.10/32", family: bgp.RF_IPv4_UC},
				{cidr: "2001:db8::10/128", family: bgp.RF_IPv6_UC},
			} {
				if err := server.AddHost(context.Background(), route.cidr, "default/service"); err != nil {
					t.Fatalf("AddHost(%s) error = %v", route.cidr, err)
				}

				attrs := listGlobalPathAttrs(t, server, route.family)
				var gotCommunities []uint32
				var gotLargeCommunities []*bgp.LargeCommunity
				for _, attr := range attrs {
					switch a := attr.(type) {
					case *bgp.PathAttributeCommunities:
						gotCommunities = a.Value
					case *bgp.PathAttributeLargeCommunities:
						gotLargeCommunities = a.Values
					}
				}
				if !reflect.DeepEqual(gotCommunities, tt.wantCommunities) {
					t.Errorf("%s communities = %v, want %v", route.cidr, gotCommunities, tt.wantCommunities)
				}
				if !reflect.DeepEqual(gotLargeCommunities, tt.wantLargeCommunities) {
					t.Errorf("%s large communities = %v, want %v", route.cidr, gotLargeCommunities, tt.wantLargeCommunities)
				}

				if err := server.DelHost(context.Background(), route.cidr, "default/service"); err != nil {
					t.Fatalf("DelHost(%s) error = %v", route.cidr, err)
				}
				if attrs := listGlobalPathAttrs(t, server, route.family); attrs != nil {
					t.Fatalf("%s is still advertised after DelHost: %v", route.cidr, attrs)
				}
			}
		})
	}
}

// listGlobalPathAttrs returns the attributes of the only path in the global
// RIB for family, or nil when the RIB holds no path for it.
func listGlobalPathAttrs(t *testing.T, server *Server, family bgp.Family) []bgp.PathAttributeInterface {
	t.Helper()

	var paths []*apiutil.Path
	if err := server.s.ListPath(apiutil.ListPathRequest{
		TableType: api.TableType_TABLE_TYPE_GLOBAL,
		Family:    family,
	}, func(_ bgp.NLRI, destinationPaths []*apiutil.Path) {
		paths = append(paths, destinationPaths...)
	}); err != nil {
		t.Fatalf("ListPath() error = %v", err)
	}
	switch len(paths) {
	case 0:
		return nil
	case 1:
		return paths[0].Attrs
	default:
		t.Fatalf("ListPath() returned %d paths, want 1", len(paths))
		return nil
	}
}
