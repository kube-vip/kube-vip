package bgp

import (
	"bufio"
	"context"
	"net"
	"testing"
	"time"

	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	bgp "github.com/osrg/gobgp/v4/pkg/packet/bgp"
	"github.com/osrg/gobgp/v4/pkg/packet/bmp"
)

func TestNewBGPServerRejectsInvalidBMPConfig(t *testing.T) {
	tests := []struct {
		name string
		bmp  kubevip.BMPConfig
	}{
		{name: "server", bmp: kubevip.BMPConfig{Servers: []string{"bmp.example.com"}}},
		{name: "duplicate server", bmp: kubevip.BMPConfig{Servers: []string{"192.0.2.20", "192.0.2.20:11019"}}},
		{name: "policy", bmp: kubevip.BMPConfig{Servers: []string{"192.0.2.20"}, Policy: "both"}},
		{name: "statistics timeout", bmp: kubevip.BMPConfig{Servers: []string{"192.0.2.20"}, StatisticsTimeout: 14}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := kubevip.BGPConfig{
				AS:    65000,
				Peers: []kubevip.BGPPeer{{Address: "192.0.2.10", AS: 65001}},
				BMP:   tt.bmp,
			}
			if _, err := NewBGPServer(config, log.LevelError); err == nil {
				t.Fatal("NewBGPServer() error = nil, want an invalid BMP configuration error")
			}
		})
	}
}

// TestBMPStationReceivesAdvertisedRoutes runs the BGP server against a BMP
// station listening on loopback and checks that the station is told about the
// routes kube-vip advertises and withdraws.
func TestBMPStationReceivesAdvertisedRoutes(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening for BMP connections: %v", err)
	}
	t.Cleanup(func() { listener.Close() })

	server, err := NewBGPServer(kubevip.BGPConfig{
		AS:       65000,
		RouterID: "192.0.2.1",
		Peers:    []kubevip.BGPPeer{{Address: "192.0.2.10", AS: 65001}},
		BMP: kubevip.BMPConfig{
			Servers: []string{listener.Addr().String()},
			Policy:  kubevip.BMPPolicyLocalRIB,
		},
	}, log.LevelError)
	if err != nil {
		t.Fatalf("NewBGPServer() error = %v", err)
	}
	if err := server.Start(context.Background(), nil); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { server.Close() })

	station := acceptBMPStation(t, listener)

	if msg := station.next(t); msg.Header.Type != bmp.BMP_MSG_INITIATION {
		t.Fatalf("first BMP message type = %d, want Initiation (%d)", msg.Header.Type, bmp.BMP_MSG_INITIATION)
	}
	msg := station.next(t)
	if msg.Header.Type != bmp.BMP_MSG_PEER_UP_NOTIFICATION || msg.PeerHeader.PeerType != bmp.BMP_PEER_TYPE_LOCAL_RIB {
		t.Fatalf("second BMP message = type %d peer type %d, want a Loc-RIB Peer Up", msg.Header.Type, msg.PeerHeader.PeerType)
	}

	if err := server.AddHost(context.Background(), "198.51.100.10/32", "default/service"); err != nil {
		t.Fatalf("AddHost() error = %v", err)
	}
	msg = station.next(t)
	if msg.Header.Type != bmp.BMP_MSG_ROUTE_MONITORING || msg.PeerHeader.PeerType != bmp.BMP_PEER_TYPE_LOCAL_RIB {
		t.Fatalf("BMP message after AddHost = type %d peer type %d, want Loc-RIB Route Monitoring", msg.Header.Type, msg.PeerHeader.PeerType)
	}
	update := msg.Body.(*bmp.BMPRouteMonitoring).BGPUpdate.Body.(*bgp.BGPUpdate)
	if len(update.NLRI) != 1 || update.NLRI[0].NLRI.String() != "198.51.100.10/32" {
		t.Fatalf("Route Monitoring NLRI = %v, want 198.51.100.10/32", update.NLRI)
	}

	// Withdrawing the route is reported to the station as well.
	if err := server.DelHost(context.Background(), "198.51.100.10/32", "default/service"); err != nil {
		t.Fatalf("DelHost() error = %v", err)
	}
	msg = station.next(t)
	if msg.Header.Type != bmp.BMP_MSG_ROUTE_MONITORING || msg.PeerHeader.PeerType != bmp.BMP_PEER_TYPE_LOCAL_RIB {
		t.Fatalf("BMP message after DelHost = type %d peer type %d, want Loc-RIB Route Monitoring", msg.Header.Type, msg.PeerHeader.PeerType)
	}
	update = msg.Body.(*bmp.BMPRouteMonitoring).BGPUpdate.Body.(*bgp.BGPUpdate)
	if len(update.WithdrawnRoutes) != 1 || update.WithdrawnRoutes[0].NLRI.String() != "198.51.100.10/32" {
		t.Fatalf("Route Monitoring withdrawn routes = %v, want 198.51.100.10/32", update.WithdrawnRoutes)
	}
}

type bmpStation struct {
	conn    net.Conn
	scanner *bufio.Scanner
}

func acceptBMPStation(t *testing.T, listener net.Listener) *bmpStation {
	t.Helper()

	if err := listener.(*net.TCPListener).SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("setting accept deadline: %v", err)
	}
	conn, err := listener.Accept()
	if err != nil {
		t.Fatalf("accepting the BMP connection: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	scanner := bufio.NewScanner(conn)
	scanner.Split(bmp.SplitBMP)
	return &bmpStation{conn: conn, scanner: scanner}
}

// next returns the next BMP message the station receives.
func (s *bmpStation) next(t *testing.T) *bmp.BMPMessage {
	t.Helper()

	if err := s.conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("setting read deadline: %v", err)
	}
	if !s.scanner.Scan() {
		t.Fatalf("reading BMP message: %v", s.scanner.Err())
	}
	// Loc-RIB Route Monitoring messages carry ADD-PATH path identifiers, which
	// the Loc-RIB Peer Up advertises for every family (RFC 9069).
	msg, err := bmp.ParseBMPMessageWithOptions(s.scanner.Bytes(), func(header bmp.BMPPeerHeader) []*bgp.MarshallingOption {
		if header.PeerType != bmp.BMP_PEER_TYPE_LOCAL_RIB {
			return nil
		}
		return []*bgp.MarshallingOption{{AddPath: map[bgp.Family]bgp.BGPAddPathMode{
			bgp.RF_IPv4_UC: bgp.BGP_ADD_PATH_BOTH,
			bgp.RF_IPv6_UC: bgp.BGP_ADD_PATH_BOTH,
		}}}
	})
	if err != nil {
		t.Fatalf("parsing BMP message: %v", err)
	}
	return msg
}
