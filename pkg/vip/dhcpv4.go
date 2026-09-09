package vip

// DHCP client implementation that refers to https://www.rfc-editor.org/rfc/rfc2131.html

import (
	"context"
	"fmt"
	"net"
	"sync"
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
	broadcastFlag   bool
	stopChan        chan struct{} // is used by external clients to stop DHCP
	errorChan       chan error    // indicates there was an error on the IP request
	ipChan          chan string
	backoffAttempts uint
	stopOnce        sync.Once
	mtx             sync.RWMutex
}

func (c *DHCPv4Client) storeLease(lease *nclient4.Lease) {
	c.mtx.Lock()
	defer c.mtx.Unlock()
	c.lease = lease
}

func (c *DHCPv4Client) loadLease() *nclient4.Lease {
	c.mtx.RLock()
	defer c.mtx.RUnlock()
	return c.lease
}

// NewDHCPv4Client returns a new DHCP Client.
func NewDHCPv4Client(iface *net.Interface, initRebootFlag bool, requestedIP string, backoffAttempts uint, broadcastFlag bool) *DHCPv4Client {
	return &DHCPv4Client{
		iface:           iface,
		stopChan:        make(chan struct{}),
		errorChan:       make(chan error),
		initRebootFlag:  initRebootFlag,
		requestedIP:     net.ParseIP(requestedIP),
		broadcastFlag:   broadcastFlag,
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
	c.close()
}

func (c *DHCPv4Client) close() {
	c.stopOnce.Do(func() {
		close(c.ipChan)
		close(c.stopChan)
	})
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
	lease, err := c.requestWithBackoff(ctx)
	if err != nil {
		return fmt.Errorf("DHCPv4 client failed: %w", err)
	}

	c.initRebootFlag = false

	c.storeLease(lease)

	// Set up two timers to renew/rebind regularly
	t1Timeout, t2Timeout := getLeaseTimeouts(lease)
	log.Debug("[DHCPv4] timeouts", "timeout1", t1Timeout, "timeout2", t2Timeout)
	t1, t2 := time.NewTimer(t1Timeout), time.NewTimer(t2Timeout)

	for {
		select {
		case <-c.stopChan:
			return c.killProcessing(t1, t2)
		case <-ctx.Done():
			c.close()
			return c.killProcessing(t1, t2)
		case <-t1.C:
			// renew is a unicast request of the IP renewal
			// A point on renew is: the library does not return the right message (NAK)
			// on renew error due to IP Change, but instead it returns a different error
			// This way there's not much to do other than log and continue, as the renew error
			// may be an offline server, or may be an incorrect package match
			lease, err := c.renew(ctx)
			if err == nil {
				c.storeLease(lease)
				t1Timeout, t2Timeout = getLeaseTimeouts(lease)
				log.Info("[DHCPv4] renew", "lease", lease)
				t2.Reset(t2Timeout)
			} else {
				log.Error("[DHCPv4] renew failed", "err", err)
			}
			t1.Reset(t1Timeout)
		case <-t2.C:
			// rebind is just like a request, but forcing to provide a new IP address
			lease, err := c.request(ctx, true)
			if err == nil {
				c.storeLease(lease)
				t1Timeout, t2Timeout = getLeaseTimeouts(lease)
				log.Info("[DHCPv4] rebind", "lease", lease)
			} else {
				if _, ok := err.(*nclient4.ErrNak); !ok {
					log.Error("[DHCPv4] rebind failed", "err", err)
				}
				lease = c.loadLease()
				log.Warn("[DHCPv4] ip may have changed", "ip", lease.ACK.YourIPAddr, "err", err)
				c.initRebootFlag = false
				lease, backoffErr := c.requestWithBackoff(ctx)
				if backoffErr != nil {
					log.Error("[DHCPv4] failed to reacquire lease", "err", backoffErr)
					continue
				}
				c.storeLease(lease)
				t1Timeout, t2Timeout = getLeaseTimeouts(lease)
			}
			t1.Reset(t1Timeout)
			t2.Reset(t2Timeout)
		}
	}
}

func getLeaseTimeouts(lease *nclient4.Lease) (time.Duration, time.Duration) {
	t1Timeout, t2Timeout := lease.ACK.IPAddressLeaseTime(defaultDHCPRenew)/2, (lease.ACK.IPAddressLeaseTime(defaultDHCPRenew)/8)*7
	log.Debug("[DHCPv4] timeouts", "address", lease.ACK.YourIPAddr.String(), "T1", t1Timeout, "T2", t2Timeout)
	return t1Timeout, t2Timeout
}

func (c *DHCPv4Client) killProcessing(t1, t2 *time.Timer) error {
	// release is a unicast request of the IP release.
	var err error
	lease := c.loadLease()
	if lease != nil {
		if err = c.release(); err != nil {
			log.Error("[DHCPv4] release lease failed", "lease", lease, "err", err)
		} else {
			log.Info("[DHCPv4] release", "lease", lease)
		}
	}
	t1.Stop()
	t2.Stop()
	return err
}

// --------------------------------------------------------
// |              |INIT-REBOOT  | RENEWING     |REBINDING |
// --------------------------------------------------------
// |broad/unicast |broadcast    | unicast      |broadcast |
// |server-ip     |MUST NOT     | MUST NOT     |MUST NOT  |
// |requested-ip  |MUST         | MUST NOT     |MUST NOT  |
// |ciaddr        |zero         | IP address   |IP address|
// --------------------------------------------------------

func (c *DHCPv4Client) requestWithBackoff(ctx context.Context) (*nclient4.Lease, error) {
	backoff := backoff.Backoff{
		Factor: 2,
		Jitter: true,
		Min:    10 * time.Second,
		Max:    1 * time.Minute,
	}

	var lease *nclient4.Lease
	var err error

	log.Debug("[DHCPv4]", "attempts", c.backoffAttempts)

RequestLoop:
	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("[DHCPv4] context error: %w", ctx.Err())
		default:
			log.Debug("[DHCPv4] trying to get a new IP", "attempt", backoff.Attempt()+1)
			lease, err = c.request(ctx, false)
			if err != nil {
				dur := backoff.Duration()

				if c.backoffAttempts > 0 && backoff.Attempt() > float64(c.backoffAttempts)-1 {
					errMsg := fmt.Errorf("failed to get an IPv4 address after %d attempt(s), giving up, error: %s", c.backoffAttempts, err.Error())
					log.Error(fmt.Sprintf("[DHCPv4] %s", errMsg.Error()))
					c.errorChan <- errMsg
					return nil, errMsg
				}
				log.Error("[DHCPv4] request failed", "attempt", backoff.Attempt(), "err", err.Error(), "waiting", dur)
				t := time.NewTimer(dur)
				select {
				case <-t.C:
					t.Stop()
				case <-ctx.Done():
				}
				continue RequestLoop
			}
			backoff.Reset()
			break RequestLoop
		}
	}

	if c.ipChan != nil {
		c.ipChan <- lease.ACK.YourIPAddr.String()
	}

	return lease, nil
}

func (c *DHCPv4Client) request(ctx context.Context, rebind bool) (*nclient4.Lease, error) {
	dhclient, err := nclient4.New(c.iface.Name)
	if err != nil {
		return nil, fmt.Errorf("create a client for iface %s failed, error: %w", c.iface.Name, err)
	}

	defer dhclient.Close()

	modifiers := make([]dhcpv4.Modifier, 0)

	if c.broadcastFlag {
		modifiers = append(modifiers, func(d *dhcpv4.DHCPv4) { d.SetBroadcast() })
	}

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
	err = dhclient.Release(c.lease)
	if err != nil {
		return fmt.Errorf("DHCPv4 release failed: %w", err)
	}

	return nil
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
