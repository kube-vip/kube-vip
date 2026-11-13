package vip

// DHCP client implementation that refers to https://www.rfc-editor.org/rfc/rfc2131.html

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	log "log/slog"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/nclient4"
	"github.com/jpillora/backoff"
	"github.com/mdlayher/packet"
	"github.com/vishvananda/netlink"
	"golang.org/x/net/ipv4"
	"golang.org/x/sys/unix"
)

const dhcpClientPort = "68"
const defaultDHCPRenew = time.Hour

// DHCPv4Client is responsible for maintaining ipv4 lease for one specified interface
type DHCPv4Client struct {
	iface           *net.Interface
	interfaceAlias  *string
	ddnsHostName    string
	vendorClass     string
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

// WithVendorClass sets the vendor class identifier (DHCP option 60) that the
// client sends when it requests, renews and rebinds its lease.
func (c *DHCPv4Client) WithVendorClass(vendorClass string) *DHCPv4Client {
	c.vendorClass = vendorClass
	return c
}

// Stop state-transition process and close dhcp client
func (c *DHCPv4Client) Stop() {
	c.close()
}

func (c *DHCPv4Client) close() {
	c.stopOnce.Do(func() {
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
	select {
	case <-c.stopChan:
		return nil
	default:
	}
	stopCtx, cancelStop := context.WithCancel(ctx)
	defer cancelStop()
	go func() {
		select {
		case <-c.stopChan:
			cancelStop()
		case <-stopCtx.Done():
		}
	}()
	ctx = stopCtx
	lease, err := c.requestWithBackoff(ctx)
	if err != nil {
		return fmt.Errorf("DHCPv4 client failed: %w", err)
	}

	c.initRebootFlag = false

	c.storeLease(lease)
	parentCtx := ctx
	ctx, cancelLease := context.WithDeadline(parentCtx, lease.CreationTime.Add(lease.ACK.IPAddressLeaseTime(defaultDHCPRenew)))
	defer func() { cancelLease() }()
	resetLeaseDeadline := func(lease *nclient4.Lease) {
		cancelLease()
		ctx, cancelLease = context.WithDeadline(parentCtx, lease.CreationTime.Add(lease.ACK.IPAddressLeaseTime(defaultDHCPRenew)))
	}

	// Set up two timers to renew/rebind regularly
	t1Timeout, t2Timeout := getLeaseTimeouts(lease)
	log.Debug("[DHCPv4] timeouts", "timeout1", t1Timeout, "timeout2", t2Timeout)
	t1, t2 := time.NewTimer(t1Timeout), time.NewTimer(t2Timeout)

	for {
		select {
		case <-c.stopChan:
			return c.killProcessing(t1, t2)
		case <-ctx.Done():
			leaseExpired := parentCtx.Err() == nil
			c.close()
			cleanupErr := c.killProcessing(t1, t2)
			if leaseExpired {
				return errors.Join(fmt.Errorf("DHCPv4 active lease expired: %w", ctx.Err()), cleanupErr)
			}
			return cleanupErr
		case <-t1.C:
			// renew is a unicast request of the IP renewal
			// A point on renew is: the library does not return the right message (NAK)
			// on renew error due to IP Change, but instead it returns a different error
			// This way there's not much to do other than log and continue, as the renew error
			// may be an offline server, or may be an incorrect package match
			lease, err := c.renew(ctx)
			if err == nil {
				c.storeLease(lease)
				resetLeaseDeadline(lease)
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
				resetLeaseDeadline(lease)
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
				resetLeaseDeadline(lease)
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
					select {
					case c.errorChan <- errMsg:
					case <-c.stopChan:
					case <-ctx.Done():
					}
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
		// A delayed consumer must not keep an expired lease alive.
		deliveryCtx, cancelDelivery := context.WithDeadline(ctx, lease.CreationTime.Add(lease.ACK.IPAddressLeaseTime(defaultDHCPRenew)))
		defer cancelDelivery()
		select {
		case c.ipChan <- lease.ACK.YourIPAddr.String():
		case <-c.stopChan:
		case <-deliveryCtx.Done():
			return nil, fmt.Errorf("[DHCPv4] lease delivery ended: %w", deliveryCtx.Err())
		}
	}

	return lease, nil
}

func (c *DHCPv4Client) request(ctx context.Context, rebind bool) (*nclient4.Lease, error) {
	if c.iface == nil || c.iface.Index <= 0 || c.iface.Name == "" || len(c.iface.HardwareAddr) == 0 {
		return nil, errors.New("refusing DHCPv4 request without original interface identity")
	}
	link, err := netlink.LinkByIndex(c.iface.Index)
	if err != nil {
		return nil, fmt.Errorf("inspect original DHCPv4 request interface: %w", err)
	}
	attrs := link.Attrs()
	if attrs.Index != c.iface.Index || attrs.Name != c.iface.Name || !bytes.Equal(attrs.HardwareAddr, c.iface.HardwareAddr) {
		return nil, fmt.Errorf("refusing DHCPv4 request on changed interface %q", c.iface.Name)
	}
	if c.interfaceAlias == nil {
		// ponytail: capture once before DORA; an empty native alias is valid.
		alias := attrs.Alias
		c.interfaceAlias = &alias
	}
	if attrs.Alias != *c.interfaceAlias {
		return nil, fmt.Errorf("refusing DHCPv4 request after interface owner changed on %q", c.iface.Name)
	}

	// ponytail: pin DORA/rebind to the original index, not a reopened name.
	rawConn, err := packet.Listen(c.iface, packet.Datagram, unix.ETH_P_IP, nil)
	if err != nil {
		return nil, fmt.Errorf("open original DHCPv4 request interface: %w", err)
	}
	conn := nclient4.NewBroadcastUDPConn(rawConn, &net.UDPAddr{Port: nclient4.ClientPort})
	dhclient, err := nclient4.NewWithConn(conn, c.iface.HardwareAddr)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("create a client for iface %s failed, error: %w", c.iface.Name, err)
	}

	defer dhclient.Close()

	modifiers := make([]dhcpv4.Modifier, 0)

	if c.broadcastFlag {
		modifiers = append(modifiers, func(d *dhcpv4.DHCPv4) { d.SetBroadcast() })
	}

	modifiers = append(modifiers, c.requestOptions()...)

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

	lease, err := dhclient.Request(ctx, modifiers...)
	if err != nil {
		return nil, err
	}
	// ponytail: fresh DORA may change IP; rebinding must extend the bound IP.
	if rebind && !lease.ACK.YourIPAddr.Equal(c.lease.ACK.YourIPAddr) {
		return nil, fmt.Errorf("refusing DHCPv4 rebind ACK for changed address %s (current %s)", lease.ACK.YourIPAddr, c.lease.ACK.YourIPAddr)
	}
	return lease, nil
}

func (c *DHCPv4Client) release() error {
	if c.iface == nil || c.iface.Index <= 0 || c.iface.Name == "" || len(c.iface.HardwareAddr) == 0 || c.interfaceAlias == nil {
		return fmt.Errorf("refusing DHCPv4 release without original interface identity")
	}
	iface, err := net.InterfaceByIndex(c.iface.Index)
	if err != nil {
		return fmt.Errorf("inspect original DHCPv4 release interface: %w", err)
	}
	if iface.Name != c.iface.Name || !bytes.Equal(iface.HardwareAddr, c.iface.HardwareAddr) {
		return fmt.Errorf("refusing DHCPv4 release on changed interface %q", c.iface.Name)
	}
	link, err := netlink.LinkByIndex(c.iface.Index)
	if err != nil {
		return fmt.Errorf("inspect original DHCPv4 release owner: %w", err)
	}
	if link.Attrs().Alias != *c.interfaceAlias {
		return fmt.Errorf("refusing DHCPv4 release after interface owner changed on %q", c.iface.Name)
	}

	// ponytail: bind the original index; never reopen a reused interface name.
	rawConn, err := packet.Listen(iface, packet.Datagram, unix.ETH_P_IP, nil)
	if err != nil {
		return fmt.Errorf("open original DHCPv4 release interface: %w", err)
	}
	conn := nclient4.NewBroadcastUDPConn(rawConn, &net.UDPAddr{Port: nclient4.ClientPort})
	dhclient, err := nclient4.NewWithConn(conn, iface.HardwareAddr)
	if err != nil {
		_ = conn.Close()
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
	if c.iface == nil || c.iface.Index <= 0 || c.iface.Name == "" || len(c.iface.HardwareAddr) == 0 || c.interfaceAlias == nil {
		return nil, errors.New("refusing DHCPv4 renewal without original interface identity")
	}
	link, err := netlink.LinkByIndex(c.iface.Index)
	if err != nil {
		return nil, fmt.Errorf("inspect original DHCPv4 renewal interface: %w", err)
	}
	attrs := link.Attrs()
	if attrs.Name != c.iface.Name || !bytes.Equal(attrs.HardwareAddr, c.iface.HardwareAddr) || attrs.Alias != *c.interfaceAlias {
		return nil, fmt.Errorf("refusing DHCPv4 renewal on changed interface %q", c.iface.Name)
	}

	// Keep the lease source address required by servers such as dnsmasq.
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: c.lease.ACK.YourIPAddr, Port: nclient4.ClientPort})
	if err != nil {
		return nil, fmt.Errorf("open original DHCPv4 renewal socket: %w", err)
	}
	dhclient, err := nclient4.NewWithConn(&dhcpv4RenewalConn{UDPConn: conn, interfaceIndex: attrs.Index}, attrs.HardwareAddr)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("create renew client failed, error: %w, server ip: %v", err, c.lease.ACK.ServerIPAddr)
	}
	defer dhclient.Close()

	renewed, err := dhclient.Renew(ctx, c.lease, c.requestOptions()...)
	if err != nil {
		return nil, err
	}
	// ponytail: another address's ACK cannot extend this bound lease.
	if !renewed.ACK.YourIPAddr.Equal(c.lease.ACK.YourIPAddr) {
		return nil, fmt.Errorf("refusing DHCPv4 renewal ACK for changed address %s (current %s)", renewed.ACK.YourIPAddr, c.lease.ACK.YourIPAddr)
	}
	return renewed, nil
}

// requestOptions returns the options that identify the client in a
// DHCPDISCOVER or DHCPREQUEST. They are sent again when the lease is renewed or
// rebound: RFC 2131 requires a client to keep using the client identifier it
// started with, and servers that pick the address pool by vendor class
// evaluate the class on every DHCPREQUEST.
func (c *DHCPv4Client) requestOptions() []dhcpv4.Modifier {
	var modifiers []dhcpv4.Modifier
	if c.ddnsHostName != "" {
		modifiers = append(modifiers,
			dhcpv4.WithOption(dhcpv4.OptHostName(c.ddnsHostName)),
			dhcpv4.WithOption(dhcpv4.OptClientIdentifier([]byte(c.ddnsHostName))),
		)
	}
	if c.vendorClass != "" {
		modifiers = append(modifiers, dhcpv4.WithOption(dhcpv4.OptClassIdentifier(c.vendorClass)))
	}
	return modifiers
}

type dhcpv4RenewalConn struct {
	*net.UDPConn
	interfaceIndex int
}

func (c *dhcpv4RenewalConn) WriteTo(packet []byte, peer net.Addr) (int, error) {
	// ponytail: pin renewal writes by kernel index; keep ordinary UDP reads.
	return ipv4.NewPacketConn(c.UDPConn).WriteTo(packet, &ipv4.ControlMessage{IfIndex: c.interfaceIndex}, peer)
}
