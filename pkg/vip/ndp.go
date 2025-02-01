package vip

import (
	"fmt"
	"net"
	"net/netip"

	"github.com/mdlayher/ndp"

	log "log/slog"
)

// NdpResponder defines the parameters for the NDP connection.
type NdpResponder struct {
	intf         string
	hardwareAddr net.HardwareAddr
	conn         *ndp.Conn
}

// NewNDPResponder takes an ifaceName and returns a new NDP responder and error if encountered.
func NewNDPResponder(ifaceName string) (*NdpResponder, error) {
	iface, err := net.InterfaceByName(ifaceName)
	if err != nil {
		return nil, fmt.Errorf("failed to get interface %q: %v", ifaceName, err)
	}

	// Use link-local address as the source IPv6 address for NDP communications.
	conn, _, err := ndp.Listen(iface, ndp.LinkLocal)
	if err != nil {
		return nil, fmt.Errorf("creating NDP responder for %q: %s", iface.Name, err)
	}

	ret := &NdpResponder{
		intf:         iface.Name,
		hardwareAddr: iface.HardwareAddr,
		conn:         conn,
	}
	return ret, nil
}

// Close closes the NDP responder connection.
func (n *NdpResponder) Close() error {
	return n.conn.Close()
}

// SendGratuitous broadcasts an NDP update or returns error if encountered.
func (n *NdpResponder) SendGratuitous(address string) error {
	ip, err := netip.ParseAddr(address)
	if err != nil {
		return fmt.Errorf("failed to parse address %s", ip)
	}

	log.Info("Broadcasting NDP update", "ip", address, "hwaddr", n.hardwareAddr, "interface", n.intf)
	return n.advertise(netip.IPv6LinkLocalAllNodes(), ip, true)
}

func (n *NdpResponder) advertise(dst, target netip.Addr, gratuitous bool) error {
	m := &ndp.NeighborAdvertisement{
		Solicited:     !gratuitous,
		Override:      gratuitous, // Should clients replace existing cache entries
		TargetAddress: target,
		Options: []ndp.Option{
			&ndp.LinkLayerAddress{
				Direction: ndp.Target,
				Addr:      n.hardwareAddr,
			},
		},
	}
  
	log.Debug("ndp", "advertisement", m)
	return n.conn.WriteTo(m, nil, dst)
}
