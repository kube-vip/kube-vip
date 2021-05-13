package vip

import (
	"net"
	"sync"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/jpillora/backoff"
	log "github.com/sirupsen/logrus"
)

// Callback is a function called on certain events
type Callback func(*Lease)

// Lease is
type Lease struct {
	RenewAfter time.Time
	ClientIP   string
	SubnetMask string
	Router     string
	DNS        []string
}

//DHCPClient is
type DHCPClient struct {
	Interface *net.Interface // e.g. net.InterfaceByName("eth0")
	HWAddr    net.HardwareAddr
	Hostname  string
	Lease     Lease

	err          error
	once         sync.Once
	onceErr      error
	connection   net.PacketConn
	hardwareAddr net.HardwareAddr
	timeNow      func() time.Time
	generateXID  func() uint32
	stop         chan struct{}

	// last DHCPACK packet for renewal/release
	Ack *layers.DHCPv4

	OnBound Callback // On renew or rebound
}

// Err returns any errors from the DHCP client
func (c *DHCPClient) Err() error {
	return c.err
}

// Start will begin the DHCP client
func (c *DHCPClient) Start() error {

	var ack *layers.DHCPv4
	pkt := gopacket.NewPacket(nil, layers.LayerTypeDHCPv4, gopacket.DecodeOptions{})
	if dhcp, ok := pkt.Layer(layers.LayerTypeDHCPv4).(*layers.DHCPv4); ok {
		ack = dhcp
	}
	// Set ack packet
	c.Ack = ack
	// Get Hardware address
	c.HWAddr = c.Interface.HardwareAddr
	// Create a channel for notifications
	c.stop = make(chan struct{})

	// Configure the backoff
	backoff := backoff.Backoff{
		Factor: 2,
		Jitter: true,
		Min:    10 * time.Second,
		Max:    1 * time.Minute,
	}
	// Start renew loop
	for {
		for c.obtainOrRenew() {
			if err := c.Err(); err != nil {
				dur := backoff.Duration()
				log.Printf("Temporary error: %v (waiting %v)", err, dur)
				time.Sleep(dur)
				continue
			} else {
				backoff.Reset()
				break
			}

		}

		// call the handler
		if cb := c.OnBound; cb != nil {
			cb(&c.Lease)
		}
		select {
		case <-c.stop:
			return c.err
		case <-time.After(time.Until(c.Lease.RenewAfter)):
			// remove lease and request a new one
			continue
		}
	}
}

// Stop will stop the DHCP client
func (c *DHCPClient) Stop() error {
	err := c.release()
	if err != nil {
		log.Errorf("Unable ro return lease: %v", err)
	}
	close(c.stop)
	return nil
}
