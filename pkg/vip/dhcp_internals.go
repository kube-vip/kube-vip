package vip

import (
	"bytes"
	"fmt"
	"syscall"
	"time"

	"github.com/google/gopacket/layers"
	"github.com/mdlayher/raw"
	"github.com/pkg/errors"
	"github.com/rtr7/dhcp4"
	"golang.org/x/sys/unix"
)

func (c *DHCPClient) release() error {
	release := c.packet(c.generateXID(), append([]layers.DHCPOption{
		dhcp4.MessageTypeOpt(layers.DHCPMsgTypeRelease),
	}, serverID(c.Ack)...))
	release.ClientIP = c.Ack.YourClientIP
	if err := dhcp4.Write(c.connection, release); err != nil {
		return err
	}

	c.Ack = nil
	return nil
}

func serverID(pkt *layers.DHCPv4) []layers.DHCPOption {
	for _, o := range pkt.Options {
		if o.Type == layers.DHCPOptServerID {
			return []layers.DHCPOption{o}
		}
	}
	return nil
}

func (c *DHCPClient) packet(xid uint32, opts []layers.DHCPOption) *layers.DHCPv4 {
	return &layers.DHCPv4{
		Operation:    layers.DHCPOpRequest,
		HardwareType: layers.LinkTypeEthernet,
		HardwareLen:  uint8(len(layers.EthernetBroadcast)),
		HardwareOpts: 0, // clients set this to zero (used by relay agents)
		Xid:          xid,
		Secs:         0, // TODO: fill in?
		Flags:        0, // we can receive IP packets via unicast
		ClientHWAddr: c.hardwareAddr,
		ServerName:   nil,
		File:         nil,
		Options:      opts,
	}
}

var errNAK = errors.New("received DHCPNAK")

// ObtainOrRenew returns false when encountering a permanent error.
func (c *DHCPClient) obtainOrRenew() bool {
	c.once.Do(func() {
		if c.timeNow == nil {
			c.timeNow = time.Now
		}
		if c.connection == nil && c.Interface != nil {
			conn, err := raw.ListenPacket(c.Interface, syscall.ETH_P_IP, &raw.Config{
				LinuxSockDGRAM: true,
			})
			if err != nil {
				c.onceErr = err
				return
			}
			c.connection = conn
		}
		if c.connection == nil && c.Interface == nil {
			c.onceErr = fmt.Errorf("c.Interface is nil")
			return
		}
		if c.hardwareAddr == nil && c.HWAddr != nil {
			c.hardwareAddr = c.HWAddr
		}
		if c.hardwareAddr == nil {
			c.hardwareAddr = c.Interface.HardwareAddr
		}
		if c.generateXID == nil {
			c.generateXID = dhcp4.XIDGenerator(c.hardwareAddr)
		}
		if c.Hostname == "" {
			var utsname unix.Utsname
			if err := unix.Uname(&utsname); err != nil {
				c.onceErr = err
				return
			}
			c.Hostname = string(utsname.Nodename[:bytes.IndexByte(utsname.Nodename[:], 0)])
		}
	})
	if c.onceErr != nil {
		c.err = c.onceErr
		return false // permanent error
	}
	c.err = nil // clear previous error
	ack, err := c.dhcpRequest()
	if err != nil {
		if errno, ok := err.(syscall.Errno); ok && errno == syscall.EAGAIN {
			c.err = fmt.Errorf("DHCP: timeout (server(s) unreachable)")
			return true // temporary error
		}
		if err == errNAK {
			c.Ack = nil // start over at DHCPDISCOVER
		}
		c.err = fmt.Errorf("DHCP: %v", err)
		return true // temporary error
	}
	c.Ack = ack
	c.Lease.ClientIP = ack.YourClientIP.String()
	lease := dhcp4.LeaseFromACK(ack)
	if mask := lease.Netmask; len(mask) > 0 {
		c.Lease.SubnetMask = fmt.Sprintf("%d.%d.%d.%d", mask[0], mask[1], mask[2], mask[3])
	}
	if len(lease.Router) > 0 {
		c.Lease.Router = lease.Router.String()
	}
	if len(lease.DNS) > 0 {
		c.Lease.DNS = make([]string, len(lease.DNS))
		for idx, ip := range lease.DNS {
			c.Lease.DNS[idx] = ip.String()
		}
	}
	c.Lease.RenewAfter = c.timeNow().Add(lease.RenewalTime)
	return true
}

func (c *DHCPClient) dhcpRequest() (*layers.DHCPv4, error) {
	var last *layers.DHCPv4

	if c.Ack != nil {
		last = c.Ack
	} else {
		discover := c.packet(c.generateXID(), []layers.DHCPOption{
			dhcp4.MessageTypeOpt(layers.DHCPMsgTypeDiscover),
			dhcp4.HostnameOpt(c.Hostname),
			dhcp4.ClientIDOpt(layers.LinkTypeEthernet, c.hardwareAddr),
			dhcp4.ParamsRequestOpt(
				layers.DHCPOptDNS,
				layers.DHCPOptRouter,
				layers.DHCPOptSubnetMask),
		})
		if err := dhcp4.Write(c.connection, discover); err != nil {
			return nil, err
		}

		// Look for DHCPOFFER packet (described in RFC2131 4.3.1):
		c.connection.SetReadDeadline(time.Now().Add(10 * time.Second))
		for {
			offer, err := dhcp4.Read(c.connection)
			if err != nil {
				return nil, err
			}
			if offer == nil {
				continue // not a DHCPv4 packet
			}
			if offer.Xid != discover.Xid {
				continue // broadcast reply for different DHCP transaction
			}
			if !dhcp4.HasMessageType(offer.Options, layers.DHCPMsgTypeOffer) {
				continue
			}
			last = offer
			break
		}
	}

	// Build a DHCPREQUEST packet:
	request := c.packet(last.Xid, append([]layers.DHCPOption{
		dhcp4.MessageTypeOpt(layers.DHCPMsgTypeRequest),
		dhcp4.RequestIPOpt(last.YourClientIP),
		dhcp4.HostnameOpt(c.Hostname),
		dhcp4.ClientIDOpt(layers.LinkTypeEthernet, c.hardwareAddr),
		dhcp4.ParamsRequestOpt(
			layers.DHCPOptDNS,
			layers.DHCPOptRouter,
			layers.DHCPOptSubnetMask),
	}, serverID(last)...))
	if err := dhcp4.Write(c.connection, request); err != nil {
		return nil, err
	}

	c.connection.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		// Look for DHCPACK packet (described in RFC2131 4.3.1):
		ack, err := dhcp4.Read(c.connection)
		if err != nil {
			return nil, err
		}
		if ack == nil {
			continue // not a DHCPv4 packet
		}
		if ack.Xid != request.Xid {
			continue // broadcast reply for different DHCP transaction
		}
		if !dhcp4.HasMessageType(ack.Options, layers.DHCPMsgTypeAck) {
			if dhcp4.HasMessageType(ack.Options, layers.DHCPMsgTypeNak) {
				return nil, errNAK
			}
			continue
		}
		return ack, nil
	}
}
