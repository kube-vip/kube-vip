package vip

import (
	"context"
	"fmt"
	log "log/slog"
	"net"
	"sync/atomic"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv6"
	"github.com/insomniacslk/dhcp/dhcpv6/nclient6"
	"github.com/insomniacslk/dhcp/iana"
	"github.com/jpillora/backoff"
	"github.com/vishvananda/netlink"
)

var dhcpv6ClientManager *DHCPv6ClientManager

func init() {
	dhcpv6ClientManager = NewDHCPv6ClientManager()
}

type DHCPv6ClientManager struct {
	clients map[string]*DHCPv6InternalClient
}

func NewDHCPv6ClientManager() *DHCPv6ClientManager {
	return &DHCPv6ClientManager{
		clients: map[string]*DHCPv6InternalClient{},
	}
}

func (m *DHCPv6ClientManager) Get(iface string) *DHCPv6InternalClient {
	c, exists := m.clients[iface]
	if !exists {
		return nil
	}
	return c
}

func (m *DHCPv6ClientManager) Add(iface string) (*DHCPv6InternalClient, error) {
	c := m.Get(iface)

	if c != nil {
		c.references.Add(1)
		return c, nil
	}
	client, err := NewDHCPv6InternalClient(iface)
	if err != nil {
		return nil, err
	}

	m.clients[iface] = client
	return client, nil
}

func (m *DHCPv6ClientManager) Delete(iface string) {
	c := m.Get(iface)

	if c != nil {
		c.references.Add(-1)
		ref := c.references.Load()
		if ref < 1 {
			c.client.Close()
			delete(m.clients, iface)
		}
	}
}

type DHCPv6InternalClient struct {
	client     *nclient6.Client
	references *atomic.Int32
}

func NewDHCPv6InternalClient(iface string) (*DHCPv6InternalClient, error) {
	client, err := nclient6.New(iface)
	if err != nil {
		return nil, fmt.Errorf("failed to create DHCPv6 client for interface %q: %w", iface, err)
	}

	ref := &atomic.Int32{}
	ref.Store(1)
	return &DHCPv6InternalClient{
		client:     client,
		references: ref,
	}, nil
}

type DHCPv6Client struct {
	iface           *net.Interface
	ddnsHostName    string
	initRebootFlag  bool
	requestedIP     net.IP
	stopChan        chan struct{} // used as a signal to release the IP and stop the dhcp client daemon
	releasedChan    chan struct{} // indicate that the IP has been released
	errorChan       chan error    // indicates there was an error on the IP request
	ipChan          chan string
	ic              *DHCPv6InternalClient
	addr            *dhcpv6.OptIAAddress
	backoffAttempts uint
}

// NewDHCPv6Client returns a new DHCP6 Client.
func NewDHCPv6Client(iface *net.Interface, parent netlink.Link, initRebootFlag bool, requestedIP string, backoffAttempts uint) (*DHCPv6Client, error) {
	name := iface.Name
	if parent != nil {
		name = parent.Attrs().Name
	}

	client, err := dhcpv6ClientManager.Add(name)
	if err != nil {
		return nil, fmt.Errorf("failed to create DHCPv6 client: %w", err)
	}

	return &DHCPv6Client{
		iface:           iface,
		stopChan:        make(chan struct{}),
		releasedChan:    make(chan struct{}),
		errorChan:       make(chan error),
		initRebootFlag:  initRebootFlag,
		requestedIP:     net.ParseIP(requestedIP),
		ipChan:          make(chan string),
		ic:              client,
		backoffAttempts: backoffAttempts,
	}, nil
}

func (c *DHCPv6Client) WithHostName(hostname string) DHCPClient {
	c.ddnsHostName = hostname
	return c
}

// Stop state-transition process and close dhcp client
func (c *DHCPv6Client) Stop() {
	close(c.ipChan)
	close(c.stopChan)
	<-c.releasedChan
	dhcpv6ClientManager.Delete(c.iface.Name)
}

// Gets the IPChannel for consumption
func (c *DHCPv6Client) IPChannel() chan string {
	return c.ipChan
}

// Gets the ErrorChannel for consumption
func (c *DHCPv6Client) ErrorChannel() chan error {
	return c.errorChan
}

func (c *DHCPv6Client) Start(ctx context.Context) error {
	dhcpContext, cancel := context.WithCancel(ctx)
	defer cancel()

	addr, err := c.requestWithBackoff(dhcpContext)

	if err != nil {
		return fmt.Errorf("DHCPv6 client failed: %w", err)
	}

	c.addr = addr

	c.initRebootFlag = false

	// Set up two ticker to renew/rebind regularly
	t1Timeout := c.addr.PreferredLifetime / 2
	t2Timeout := (c.addr.ValidLifetime / 8) * 7
	log.Debug("[DHCPv6] timeouts", "timeout1", t1Timeout, "timeout2", t2Timeout)
	t1, t2 := time.NewTicker(t1Timeout), time.NewTicker(t2Timeout)

	for {
		select {
		case <-t1.C:
			// renew is a unicast request of the IP renewal
			// A point on renew is: the library does not return the right message (NAK)
			// on renew error due to IP Change, but instead it returns a different error
			// This way there's not much to do other than log and continue, as the renew error
			// may be an offline server, or may be an incorrect package match

			addr, err := c.renew(dhcpContext)
			if err == nil {
				c.addr = addr
				log.Info("[DHCPv6] renew", "addr", addr.IPv6Addr.String())
				t2.Reset(t2Timeout)
			} else {
				log.Error("[DHCPv6] renew failed", "err", err)
			}
		case <-t2.C:
			// rebind is just like a request, but forcing to provide a new IP address
			addr, err := c.request(dhcpContext, true)
			if err == nil {
				c.addr = addr
				log.Info("[DHCPv6] rebind", "lease", addr)
			} else {
				log.Warn("[DHCPv6] ip may have changed", "ip", addr.IPv6Addr.String(), "err", err)
				c.initRebootFlag = false
				c.addr, err = c.requestWithBackoff(dhcpContext)
				log.Error("[DHCPv6] rebind failed", "err", err)
			}
			t1.Reset(t1Timeout)
			t2.Reset(t2Timeout)

		case <-c.stopChan:
			dhcpStopContext, cancel := context.WithCancel(context.Background())
			defer cancel()
			// IP address release.
			var err error
			if err = c.release(dhcpStopContext); err != nil {
				log.Error("[DHCPv6] release failed", "err", err)
			} else {
				log.Info("[DHCPv6] released", "address", c.addr.String())
			}
			t1.Stop()
			t2.Stop()

			close(c.releasedChan)
			return err
		}
	}
}

func (c *DHCPv6Client) requestWithBackoff(ctx context.Context) (*dhcpv6.OptIAAddress, error) {
	backoff := backoff.Backoff{
		Factor: 2,
		Jitter: true,
		Min:    10 * time.Second,
		Max:    1 * time.Minute,
	}

	var err error
	var addr *dhcpv6.OptIAAddress

	for {
		log.Debug("[DHCPv6] trying to get a new IP", "attempt", backoff.Attempt()+1)

		addr, err = c.request(ctx, false)

		if err != nil {
			dur := backoff.Duration()
			if c.backoffAttempts > 0 && backoff.Attempt() > float64(c.backoffAttempts)-1 {
				errMsg := fmt.Errorf("failed to get an IPv4 address after %d attempt(s), giving up, error: %s", c.backoffAttempts, err.Error())
				log.Error(fmt.Sprintf("[DHCPv6] %s", errMsg.Error()))
				c.errorChan <- errMsg
				c.Stop()
				return nil, fmt.Errorf("failed to get IPv6 address: %w", err)
			}
			log.Error("[DHCPv6] request failed", "attempt", backoff.Attempt(), "err", err.Error(), "waiting", dur)
			time.Sleep(dur)
			continue
		}
		backoff.Reset()
		break
	}

	if c.ipChan != nil {
		log.Debug("[DHCPv6] using channel")
		c.ipChan <- addr.IPv6Addr.String()
	}

	return addr, nil
}

func (c *DHCPv6Client) request(ctx context.Context, rebind bool) (*dhcpv6.OptIAAddress, error) {
	modifiers := []dhcpv6.Modifier{}
	modifiers = append(modifiers, dhcpv6.WithClientID(&dhcpv6.DUIDEN{EnterpriseNumber: 1, EnterpriseIdentifier: []byte(c.ddnsHostName)}))
	modifiers = append(modifiers, dhcpv6.WithFQDN(4, c.ddnsHostName))

	// if initRebootFlag is set, this means we have an IP already set on c.requestedIP that should be used
	if c.initRebootFlag {
		log.Debug("[DHCPv6] init-reboot", "ip", c.requestedIP)
		addr := dhcpv6.OptIAAddress{
			IPv6Addr: c.requestedIP,
		}
		modifiers = append(modifiers, dhcpv6.WithIANA(addr))
	} else if rebind {
		if c.addr == nil {
			return nil, fmt.Errorf("unable to rebind - current IP unknown")
		}
		log.Debug("[DHCPv6] rebinding", "ip", c.addr.IPv6Addr)
		modifiers = append(modifiers, dhcpv6.WithIANA(*c.addr))
	}

	var reply *dhcpv6.Message
	if rebind || c.initRebootFlag {
		request, err := dhcpv6.NewMessage(modifiers...)
		if err != nil {
			return nil, fmt.Errorf("failed to create rebind message: %w", err)
		}

		request.MessageType = dhcpv6.MessageTypeRebind

		reply, err = c.ic.client.SendAndRead(ctx, c.ic.client.RemoteAddr(), request, nil)
		if err != nil {
			return nil, fmt.Errorf("rebind error: %w", err)
		}
	} else {
		adv, err := c.ic.client.Solicit(ctx, modifiers...)
		if err != nil {
			return nil, fmt.Errorf("solicit error: %w", err)
		}

		request, err := dhcpv6.NewRequestFromAdvertise(adv, modifiers...)
		if err != nil {
			return nil, fmt.Errorf("unable to create request message: %w", err)
		}

		request.MessageType = dhcpv6.MessageTypeAdvertise

		reply, err = c.ic.client.Request(ctx, request, modifiers...)
		if err != nil {
			return nil, fmt.Errorf("request error: %w", err)
		}
	}

	if reply == nil {
		return nil, fmt.Errorf("invalid request")
	}

	return getAddress(reply.Options.IANA())
}

func (c *DHCPv6Client) renew(ctx context.Context) (*dhcpv6.OptIAAddress, error) {
	modifiers := []dhcpv6.Modifier{}
	modifiers = append(modifiers, dhcpv6.WithClientID(&dhcpv6.DUIDEN{EnterpriseNumber: 1, EnterpriseIdentifier: []byte(c.ddnsHostName)}))
	modifiers = append(modifiers, dhcpv6.WithFQDN(4, c.ddnsHostName))
	modifiers = append(modifiers, dhcpv6.WithOption(&dhcpv6.OptionGeneric{OptionCode: dhcpv6.OptionUnicast}))

	adv, err := c.ic.client.Solicit(ctx, modifiers...)
	if err != nil {
		return nil, fmt.Errorf("solicit error: %w", err)
	}

	request, err := dhcpv6.NewRequestFromAdvertise(adv)
	if err != nil {
		return nil, fmt.Errorf("failed to create request message: %w", err)
	}

	request.MessageType = dhcpv6.MessageTypeRenew

	reply, err := c.ic.client.SendAndRead(ctx, c.ic.client.RemoteAddr(), request, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to send renew: %w", err)
	}

	return getAddress(reply.Options.IANA())
}

func (c *DHCPv6Client) release(ctx context.Context) error {
	modifiers := []dhcpv6.Modifier{}
	modifiers = append(modifiers, dhcpv6.WithClientID(&dhcpv6.DUIDEN{EnterpriseNumber: 1, EnterpriseIdentifier: []byte(c.ddnsHostName)}))
	modifiers = append(modifiers, dhcpv6.WithFQDN(4, c.ddnsHostName))

	adv, err := c.ic.client.Solicit(ctx, modifiers...)
	if err != nil {
		return fmt.Errorf("solicit error: %w", err)
	}

	request, err := dhcpv6.NewRequestFromAdvertise(adv)
	if err != nil {
		return fmt.Errorf("failed to create release message: %w", err)
	}

	request.MessageType = dhcpv6.MessageTypeRelease

	reply, err := c.ic.client.SendAndRead(ctx, c.ic.client.RemoteAddr(), request, nil)
	if err != nil {
		return fmt.Errorf("failed to send release: %w", err)
	}

	if reply.Options.Status().StatusCode != iana.StatusSuccess {
		return fmt.Errorf("release failed with code %d: %s", reply.Options.Status().StatusCode, reply.Options.Status().StatusMessage)
	}

	return nil
}

func getAddress(iana []*dhcpv6.OptIANA) (*dhcpv6.OptIAAddress, error) {
	if len(iana) < 1 {
		return nil, fmt.Errorf("failed to get IANA")
	}

	if len(iana) < 1 {
		return nil, fmt.Errorf("failed to get addresses data")
	}

	return iana[0].Options.Addresses()[0], nil
}
