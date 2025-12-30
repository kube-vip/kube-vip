package vip

// DHCP client implementation that refers to https://www.rfc-editor.org/rfc/rfc2131.html

import (
	"context"
	"fmt"
	"net"
	"time"

	log "log/slog"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/nclient4"
	"github.com/jpillora/backoff"
)

const dhcpClientPort = "68"
const defaultDHCPRenew = time.Hour

// DHCPv4Client is responsible for maintaining ipv4 lease for one specified interface
type DHCPv4Client struct {
	iface           *net.Interface
	ddnsHostName    string
	lease           *nclient4.Lease
	initRebootFlag  bool
	requestedIP     net.IP
	stopChan        chan struct{} // used as a signal to release the IP and stop the dhcp client daemon
	releasedChan    chan struct{} // indicate that the IP has been released
	errorChan       chan error    // indicates there was an error on the IP request
	ipChan          chan string
	backoffAttempts uint
}

// NewDHCPv4Client returns a new DHCP Client.
func NewDHCPv4Client(iface *net.Interface, initRebootFlag bool, requestedIP string, backoffAttempts uint) *DHCPv4Client {
	return &DHCPv4Client{
		iface:           iface,
		stopChan:        make(chan struct{}),
		releasedChan:    make(chan struct{}),
		errorChan:       make(chan error),
		initRebootFlag:  initRebootFlag,
		requestedIP:     net.ParseIP(requestedIP),
		ipChan:          make(chan string),
		backoffAttempts: backoffAttempts,
	}
}

func (c *DHCPv4Client) WithHostName(hostname string) DHCPClient {
	c.ddnsHostName = hostname
	return c
}

// Stop state-transition process and close dhcp client
func (c *DHCPv4Client) Stop() {
	close(c.ipChan)
	close(c.stopChan)
	<-c.releasedChan
}

// Gets the IPChannel for consumption
func (c *DHCPv4Client) IPChannel() chan string {
	return c.ipChan
}

// Gets the ErrorChannel for consumption
func (c *DHCPv4Client) ErrorChannel() chan error {
	return c.errorChan
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
func (c *DHCPv4Client) Start(ctx context.Context) error {
	dhcpCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	lease := c.requestWithBackoff(dhcpCtx)

	c.initRebootFlag = false
	c.lease = lease

	// Set up two ticker to renew/rebind regularly
	t1Timeout := c.lease.ACK.IPAddressLeaseTime(defaultDHCPRenew) / 2
	t2Timeout := (c.lease.ACK.IPAddressLeaseTime(defaultDHCPRenew) / 8) * 7
	log.Debug("[DHCPv4] timeouts", "timeout1", t1Timeout, "timeout2", t2Timeout)
	t1, t2 := time.NewTicker(t1Timeout), time.NewTicker(t2Timeout)

	for {
		select {
		case <-t1.C:
			// renew is a unicast request of the IP renewal
			// A point on renew is: the library does not return the right message (NAK)
			// on renew error due to IP Change, but instead it returns a different error
			// This way there's not much to do other than log and continue, as the renew error
			// may be an offline server, or may be an incorrect package match
			lease, err := c.renew(dhcpCtx)
			if err == nil {
				c.lease = lease
				log.Info("[DHCPv4] renew", "lease", lease)
				t2.Reset(t2Timeout)
			} else {
				log.Error("[DHCPv4] renew failed", "err", err)
			}
		case <-t2.C:
			// rebind is just like a request, but forcing to provide a new IP address
			lease, err := c.request(dhcpCtx, true)
			if err == nil {
				c.lease = lease
				log.Info("[DHCPv4] rebind", "lease", lease)
			} else {
				if _, ok := err.(*nclient4.ErrNak); !ok {
					t1.Stop()
					t2.Stop()
					log.Error("[DHCPv4] rebind failed", "err", err)
				}
				log.Warn("[DHCPv4] ip may have changed", "ip", c.lease.ACK.YourIPAddr, "err", err)
				c.initRebootFlag = false
				c.lease = c.requestWithBackoff(dhcpCtx)
			}
			t1.Reset(t1Timeout)
			t2.Reset(t2Timeout)

		case <-c.stopChan:
			// release is a unicast request of the IP release.
			var err error
			if err = c.release(); err != nil {
				log.Error("[DHCPv4] release lease failed", "lease", lease, "err", err)
			} else {
				log.Info("[DHCPv4] release", "lease", lease)
			}
			t1.Stop()
			t2.Stop()

			close(c.releasedChan)
			return err
		}
	}
}

// --------------------------------------------------------
// |              |INIT-REBOOT  | RENEWING     |REBINDING |
// --------------------------------------------------------
// |broad/unicast |broadcast    | unicast      |broadcast |
// |server-ip     |MUST NOT     | MUST NOT     |MUST NOT  |
// |requested-ip  |MUST         | MUST NOT     |MUST NOT  |
// |ciaddr        |zero         | IP address   |IP address|
// --------------------------------------------------------

func (c *DHCPv4Client) requestWithBackoff(ctx context.Context) *nclient4.Lease {
	backoff := backoff.Backoff{
		Factor: 2,
		Jitter: true,
		Min:    10 * time.Second,
		Max:    1 * time.Minute,
	}

	var lease *nclient4.Lease
	var err error

	log.Debug("[DHCPv4]", "attempts", c.backoffAttempts)

	for {
		log.Debug("[DHCPv4] trying to get a new IP", "attempt", backoff.Attempt()+1)
		lease, err = c.request(ctx, false)
		if err != nil {
			dur := backoff.Duration()
			if c.backoffAttempts > 0 && backoff.Attempt() > float64(c.backoffAttempts)-1 {
				errMsg := fmt.Errorf("failed to get an IPv4 address after %d attempt(s), giving up, error: %s", c.backoffAttempts, err.Error())
				log.Error(fmt.Sprintf("[DHCPv4] %s", errMsg.Error()))
				c.errorChan <- errMsg
				c.Stop()
				return nil
			}
			log.Error("[DHCPv4] request failed", "attempt", backoff.Attempt(), "err", err.Error(), "waiting", dur)
			time.Sleep(dur)
			continue
		}
		backoff.Reset()
		break
	}

	if c.ipChan != nil {
		log.Debug("[DHCPv4] using channel")
		c.ipChan <- lease.ACK.YourIPAddr.String()
	}

	return lease
}

func (c *DHCPv4Client) request(ctx context.Context, rebind bool) (*nclient4.Lease, error) {
	dhclient, err := nclient4.New(c.iface.Name)
	if err != nil {
		return nil, fmt.Errorf("create a client for iface %s failed, error: %w", c.iface.Name, err)
	}

	defer dhclient.Close()

	modifiers := make([]dhcpv4.Modifier, 0)

	if c.ddnsHostName != "" {
		modifiers = append(modifiers,
			dhcpv4.WithOption(dhcpv4.OptHostName(c.ddnsHostName)),
			dhcpv4.WithOption(dhcpv4.OptClientIdentifier([]byte(c.ddnsHostName))),
		)
	}

	// if initRebootFlag is set, this means we have an IP already set on c.requestedIP that should be used
	if c.initRebootFlag {
		log.Debug("[DHCPv4] init-reboot", "ip", c.requestedIP)
		modifiers = append(modifiers, dhcpv4.WithOption(dhcpv4.OptRequestedIPAddress(c.requestedIP)))
	}

	// if this is a rebind, then the IP we should set is the one that already exists in lease
	if rebind {
		log.Debug("[DHCPv4] rebinding", "ip", c.lease.ACK.YourIPAddr)
		modifiers = append(modifiers, dhcpv4.WithOption(dhcpv4.OptRequestedIPAddress(c.lease.ACK.YourIPAddr)))
	}

	return dhclient.Request(ctx, modifiers...)
}

func (c *DHCPv4Client) release() error {
	dhclient, err := nclient4.New(c.iface.Name)
	if err != nil {
		return fmt.Errorf("create release client failed, error: %w, iface: %s, server ip: %v", err, c.iface.Name, c.lease.ACK.ServerIPAddr)
	}
	defer dhclient.Close()

	// TODO modify lease
	return dhclient.Release(c.lease)
}

func (c *DHCPv4Client) renew(ctx context.Context) (*nclient4.Lease, error) {
	// renew needs a unicast client. This is due to some servers (like dnsmasq) require the exact request coming from the vip interface
	dhclient, err := nclient4.New(c.iface.Name,
		nclient4.WithUnicast(&net.UDPAddr{IP: c.lease.ACK.YourIPAddr, Port: nclient4.ClientPort}))
	if err != nil {
		return nil, fmt.Errorf("create renew client failed, error: %w, server ip: %v", err, c.lease.ACK.ServerIPAddr)
	}
	defer dhclient.Close()

	return dhclient.Renew(ctx, c.lease)
}
