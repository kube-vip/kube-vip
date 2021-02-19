package vip

import (
	"fmt"
	"net"

	"github.com/mdlayher/ndp"

	log "github.com/sirupsen/logrus"
)

type NdpResponder struct {
	intf         string
	hardwareAddr net.HardwareAddr
	conn         *ndp.Conn
}

func NewNDPResponder(ifaceName string) (*NdpResponder, error) {
	iface, err := net.InterfaceByName(ifaceName)
	if err != nil {
		return nil, fmt.Errorf("failed to get interface %q: %v", ifaceName, err)
	}

	// Use link-local address as the source IPv6 address for NDP communications.
	conn, _, err := ndp.Dial(iface, ndp.LinkLocal)
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

func (n *NdpResponder) Close() error {
	return n.conn.Close()
}

func (n *NdpResponder) SendGratuitous(address string) error {
	ip := net.ParseIP(address)
	if ip == nil {
		return fmt.Errorf("failed to parse address %s", ip)
	}

	log.Infof("Broadcasting NDP update for %s (%s) via %s", address, n.hardwareAddr, n.intf)
	return n.advertise(net.IPv6linklocalallnodes, ip, true)
}

func (n *NdpResponder) advertise(dst, target net.IP, gratuitous bool) error {
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
	log.Infof("ndp: %v", m)
	return n.conn.WriteTo(m, nil, dst)
}
