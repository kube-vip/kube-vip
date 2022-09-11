package vip

// DHCP client implementation that refers to https://www.rfc-editor.org/rfc/rfc2131.html

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/nclient4"
	"github.com/jpillora/backoff"
	log "github.com/sirupsen/logrus"
)

// Callback is a function called on certain events
type Callback func(*nclient4.Lease)

// DHCPClient is responsible for maintaining ipv4 lease for one specified interface
type DHCPClient struct {
	iface          *net.Interface
	ddnsHostName   string
	lease          *nclient4.Lease
	initRebootFlag bool
	requestedIP    net.IP
	stopChan       chan struct{} // used as a signal to release the IP and stop the dhcp client daemon
	releasedChan   chan struct{} // indicate that the IP has been released
	onBound        Callback
}

// NewDHCPClient returns a new DHCP Client.
func NewDHCPClient(iface *net.Interface, initRebootFlag bool, requestedIP string, onBound Callback) *DHCPClient {
	return &DHCPClient{
		iface:          iface,
		stopChan:       make(chan struct{}),
		releasedChan:   make(chan struct{}),
		initRebootFlag: initRebootFlag,
		requestedIP:    net.ParseIP(requestedIP),
		onBound:        onBound,
	}
}

func (c *DHCPClient) WithHostName(hostname string) *DHCPClient {
	c.ddnsHostName = hostname
	return c
}

// Stop state-transition process and close dhcp client
func (c *DHCPClient) Stop() {
	close(c.stopChan)

	<-c.releasedChan
}

// Start state-transition process of dhcp client
//
//	--------                               -------
//
// |        | +-------------------------->|       |<-------------------+
// | INIT-  | |     +-------------------->| INIT  |                    |
// | REBOOT |DHCPNAK/         +---------->|       |<---+               |
// |        |Restart|         |            -------     |               |
//
//	--------  |  DHCPNAK/     |               |                        |
//	   |      Discard offer   |      -/Send DHCPDISCOVER               |
//
// -/Send DHCPREQUEST         |               |                        |
//
//	   |      |     |      DHCPACK            v        |               |
//	-----------     |   (not accept.)/   -----------   |               |
//
// |           |    |  Send DHCPDECLINE |           |                  |
// | REBOOTING |    |         |         | SELECTING |<----+            |
// |           |    |        /          |           |     |DHCPOFFER/  |
//
//	-----------     |       /            -----------   |  |Collect     |
//	   |            |      /                  |   |       |  replies   |
//
// DHCPACK/         |     /  +----------------+   +-------+            |
// Record lease, set|    |   v   Select offer/                         |
// timers T1, T2   ------------  send DHCPREQUEST      |               |
//
//	  |   +----->|            |             DHCPNAK, Lease expired/   |
//	  |   |      | REQUESTING |                  Halt network         |
//	  DHCPOFFER/ |            |                       |               |
//	  Discard     ------------                        |               |
//	  |   |        |        |                   -----------           |
//	  |   +--------+     DHCPACK/              |           |          |
//	  |              Record lease, set    -----| REBINDING |          |
//	  |                timers T1, T2     /     |           |          |
//	  |                     |        DHCPACK/   -----------           |
//	  |                     v     Record lease, set   ^               |
//	  +----------------> -------      /timers T1,T2   |               |
//	             +----->|       |<---+                |               |
//	             |      | BOUND |<---+                |               |
//	DHCPOFFER, DHCPACK, |       |    |            T2 expires/   DHCPNAK/
//	 DHCPNAK/Discard     -------     |             Broadcast  Halt network
//	             |       | |         |            DHCPREQUEST         |
//	             +-------+ |        DHCPACK/          |               |
//	                  T1 expires/   Record lease, set |               |
//	               Send DHCPREQUEST timers T1, T2     |               |
//	               to leasing server |                |               |
//	                       |   ----------             |               |
//	                       |  |          |------------+               |
//	                       +->| RENEWING |                            |
//	                          |          |----------------------------+
//	                           ----------
//	        Figure: State-transition diagram for DHCP clients
func (c *DHCPClient) Start() {
	backoff := backoff.Backoff{
		Factor: 2,
		Jitter: true,
		Min:    10 * time.Second,
		Max:    1 * time.Minute,
	}
	for {
		var lease *nclient4.Lease
		var err error
		if c.initRebootFlag {
			// DHCP State-transition: INIT-REBOOT --> BOUND
			lease, err = c.initReboot()
		} else {
			// DHCP State-transition: INIT --> BOUND
			lease, err = c.request()
		}
		if err != nil {
			dur := backoff.Duration()
			log.Errorf("request failed, error: %s (waiting %v)", err.Error(), dur)
			time.Sleep(dur)
			continue
		} else {
			backoff.Reset()
		}

		c.initRebootFlag = false
		c.lease = lease
		c.onBound(lease)

		// Set up two ticker to renew/rebind regularly
		t1Timeout := c.lease.ACK.IPAddressLeaseTime(0) / 2
		t2Timeout := c.lease.ACK.IPAddressLeaseTime(0) / 8 * 7

		t1, t2 := time.NewTicker(t1Timeout), time.NewTicker(t2Timeout)

		for {
			select {
			case <-t1.C:
				// renew
				lease, err := c.renew()
				if err == nil {
					c.lease = lease
					log.Infof("renew, lease: %+v", lease)
					t2.Reset(t2Timeout)
				} else {
					log.Errorf("renew failed, error: %s", err.Error())
				}
			case <-t2.C:
				// rebind
				lease, err := c.rebind()
				if err == nil {
					c.lease = lease
					log.Infof("rebind, lease: %+v", lease)
					t1.Reset(t1Timeout)
				} else {
					log.Errorf("rebind failed, error: %s", err.Error())
					t1.Stop()
					t2.Stop()
					break
				}
			case <-c.stopChan:
				// release
				if err := c.release(); err != nil {
					log.Errorf("release lease failed, error: %s, lease: %+v", err.Error(), c.lease)
				} else {
					log.Infof("release, lease: %+v", c.lease)
				}
				t1.Stop()
				t2.Stop()

				close(c.releasedChan)
				return
			}
		}
	}
}

func (c *DHCPClient) request() (*nclient4.Lease, error) {
	broadcast, err := nclient4.New(c.iface.Name)
	if err != nil {
		return nil, fmt.Errorf("create a broadcast client for iface %s failed, error: %w", c.iface.Name, err)
	}

	defer broadcast.Close()

	if c.ddnsHostName != "" {
		return broadcast.Request(context.TODO(),
			dhcpv4.WithOption(dhcpv4.OptHostName(c.ddnsHostName)),
			dhcpv4.WithOption(dhcpv4.OptClientIdentifier([]byte(c.ddnsHostName))))
	}

	return broadcast.Request(context.TODO())
}

func (c *DHCPClient) release() error {
	unicast, err := nclient4.New(c.iface.Name, nclient4.WithUnicast(c.iface.Name, &net.UDPAddr{IP: c.lease.ACK.YourIPAddr, Port: nclient4.ClientPort}),
		nclient4.WithServerAddr(&net.UDPAddr{IP: c.lease.ACK.ServerIPAddr, Port: nclient4.ServerPort}))
	if err != nil {
		return fmt.Errorf("create unicast client failed, error: %w, iface: %s, server ip: %v", err, c.iface.Name, c.lease.ACK.ServerIPAddr)
	}
	defer unicast.Close()

	// TODO modify lease
	return unicast.Release(c.lease)
}

// --------------------------------------------------------
// |              |INIT-REBOOT  | RENEWING     |REBINDING |
// --------------------------------------------------------
// |broad/unicast |broadcast    | unicast      |broadcast |
// |server-ip     |MUST NOT     | MUST NOT     |MUST NOT  |
// |requested-ip  |MUST         | MUST NOT     |MUST NOT  |
// |ciaddr        |zero         | IP address   |IP address|
// --------------------------------------------------------
func (c *DHCPClient) initReboot() (*nclient4.Lease, error) {
	broadcast, err := nclient4.New(c.iface.Name)
	if err != nil {
		return nil, fmt.Errorf("create a broadcast client for iface %s failed, error: %w", c.iface.Name, err)
	}
	defer broadcast.Close()
	message, err := dhcpv4.New(
		dhcpv4.WithMessageType(dhcpv4.MessageTypeRequest),
		dhcpv4.WithHwAddr(c.iface.HardwareAddr),
		dhcpv4.WithOption(dhcpv4.OptRequestedIPAddress(c.requestedIP)))
	if err != nil {
		return nil, fmt.Errorf("new dhcp message failed, error: %w", err)
	}

	return sendMessage(broadcast, message)
}

func (c *DHCPClient) renew() (*nclient4.Lease, error) {
	unicast, err := nclient4.New(c.iface.Name, nclient4.WithUnicast(c.iface.Name, &net.UDPAddr{IP: c.lease.ACK.YourIPAddr, Port: nclient4.ClientPort}),
		nclient4.WithServerAddr(&net.UDPAddr{IP: c.lease.ACK.ServerIPAddr, Port: nclient4.ServerPort}))
	if err != nil {
		return nil, fmt.Errorf("create unicast client failed, error: %w, server ip: %v", err, c.lease.ACK.ServerIPAddr)
	}
	defer unicast.Close()

	message, err := dhcpv4.New(
		dhcpv4.WithMessageType(dhcpv4.MessageTypeRequest),
		dhcpv4.WithHwAddr(c.iface.HardwareAddr),
		dhcpv4.WithClientIP(c.lease.ACK.ClientIPAddr))
	if err != nil {
		return nil, fmt.Errorf("new dhcp message failed, error: %w", err)
	}

	return sendMessage(unicast, message)
}

func (c *DHCPClient) rebind() (*nclient4.Lease, error) {
	broadcast, err := nclient4.New(c.iface.Name)
	if err != nil {
		return nil, fmt.Errorf("create a broadcast client for iface %s failed, error: %s", c.iface.Name, err)
	}
	defer broadcast.Close()
	message, err := dhcpv4.New(
		dhcpv4.WithMessageType(dhcpv4.MessageTypeRequest),
		dhcpv4.WithHwAddr(c.iface.HardwareAddr),
		dhcpv4.WithClientIP(c.lease.ACK.ClientIPAddr))
	if err != nil {
		return nil, fmt.Errorf("new dhcp message failed, error: %w", err)
	}

	return sendMessage(broadcast, message)
}

func sendMessage(client *nclient4.Client, message *dhcpv4.DHCPv4) (*nclient4.Lease, error) {
	response, err := client.SendAndRead(context.TODO(), client.RemoteAddr(), message,
		nclient4.IsMessageType(dhcpv4.MessageTypeAck, dhcpv4.MessageTypeNak))
	if err != nil {
		return nil, fmt.Errorf("got an error while processing the request: %w", err)
	}
	if response.MessageType() == dhcpv4.MessageTypeNak {
		return nil, &nclient4.ErrNak{
			Offer: message,
			Nak:   response,
		}
	}

	lease := &nclient4.Lease{}
	lease.ACK = response
	lease.Offer = message
	lease.CreationTime = time.Now()

	return lease, nil
}
