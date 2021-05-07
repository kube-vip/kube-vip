package vip

import (
	"encoding/binary"
	"fmt"
	"log"
	"math/rand"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/containernetworking/cni/pkg/types"
	"github.com/d2g/dhcp4"
	"github.com/d2g/dhcp4client"
	"github.com/pkg/errors"
	"github.com/vishvananda/netlink"
)

// DHCPOpt represents a DHCP option or parameter from RFC-2132
type DHCPOpt byte

// Constants for the DHCPOpt options.
const (
	DHCPOptPad                   DHCPOpt = 0
	DHCPOptSubnetMask            DHCPOpt = 1   // 4, net.IP
	DHCPOptTimeOffset            DHCPOpt = 2   // 4, int32 (signed seconds from UTC)
	DHCPOptRouter                DHCPOpt = 3   // n*4, [n]net.IP
	DHCPOptTimeServer            DHCPOpt = 4   // n*4, [n]net.IP
	DHCPOptNameServer            DHCPOpt = 5   // n*4, [n]net.IP
	DHCPOptDNS                   DHCPOpt = 6   // n*4, [n]net.IP
	DHCPOptLogServer             DHCPOpt = 7   // n*4, [n]net.IP
	DHCPOptCookieServer          DHCPOpt = 8   // n*4, [n]net.IP
	DHCPOptLPRServer             DHCPOpt = 9   // n*4, [n]net.IP
	DHCPOptImpressServer         DHCPOpt = 10  // n*4, [n]net.IP
	DHCPOptResLocServer          DHCPOpt = 11  // n*4, [n]net.IP
	DHCPOptHostname              DHCPOpt = 12  // n, string
	DHCPOptBootfileSize          DHCPOpt = 13  // 2, uint16
	DHCPOptMeritDumpFile         DHCPOpt = 14  // >1, string
	DHCPOptDomainName            DHCPOpt = 15  // n, string
	DHCPOptSwapServer            DHCPOpt = 16  // n*4, [n]net.IP
	DHCPOptRootPath              DHCPOpt = 17  // n, string
	DHCPOptExtensionsPath        DHCPOpt = 18  // n, string
	DHCPOptIPForwarding          DHCPOpt = 19  // 1, bool
	DHCPOptSourceRouting         DHCPOpt = 20  // 1, bool
	DHCPOptPolicyFilter          DHCPOpt = 21  // 8*n, [n]{net.IP/net.IP}
	DHCPOptDatagramMTU           DHCPOpt = 22  // 2, uint16
	DHCPOptDefaultTTL            DHCPOpt = 23  // 1, byte
	DHCPOptPathMTUAgingTimeout   DHCPOpt = 24  // 4, uint32
	DHCPOptPathPlateuTableOption DHCPOpt = 25  // 2*n, []uint16
	DHCPOptInterfaceMTU          DHCPOpt = 26  // 2, uint16
	DHCPOptAllSubsLocal          DHCPOpt = 27  // 1, bool
	DHCPOptBroadcastAddr         DHCPOpt = 28  // 4, net.IP
	DHCPOptMaskDiscovery         DHCPOpt = 29  // 1, bool
	DHCPOptMaskSupplier          DHCPOpt = 30  // 1, bool
	DHCPOptRouterDiscovery       DHCPOpt = 31  // 1, bool
	DHCPOptSolicitAddr           DHCPOpt = 32  // 4, net.IP
	DHCPOptStaticRoute           DHCPOpt = 33  // n*8, [n]{net.IP/net.IP} -- note the 2nd is router not mask
	DHCPOptARPTrailers           DHCPOpt = 34  // 1, bool
	DHCPOptARPTimeout            DHCPOpt = 35  // 4, uint32
	DHCPOptEthernetEncap         DHCPOpt = 36  // 1, bool
	DHCPOptTCPTTL                DHCPOpt = 37  // 1, byte
	DHCPOptTCPKeepAliveInt       DHCPOpt = 38  // 4, uint32
	DHCPOptTCPKeepAliveGarbage   DHCPOpt = 39  // 1, bool
	DHCPOptNISDomain             DHCPOpt = 40  // n, string
	DHCPOptNISServers            DHCPOpt = 41  // 4*n,  [n]net.IP
	DHCPOptNTPServers            DHCPOpt = 42  // 4*n, [n]net.IP
	DHCPOptVendorOption          DHCPOpt = 43  // n, [n]byte // may be encapsulated.
	DHCPOptNetBIOSTCPNS          DHCPOpt = 44  // 4*n, [n]net.IP
	DHCPOptNetBIOSTCPDDS         DHCPOpt = 45  // 4*n, [n]net.IP
	DHCPOptNETBIOSTCPNodeType    DHCPOpt = 46  // 1, magic byte
	DHCPOptNetBIOSTCPScope       DHCPOpt = 47  // n, string
	DHCPOptXFontServer           DHCPOpt = 48  // n, string
	DHCPOptXDisplayManager       DHCPOpt = 49  // n, string
	DHCPOptRequestIP             DHCPOpt = 50  // 4, net.IP
	DHCPOptLeaseTime             DHCPOpt = 51  // 4, uint32
	DHCPOptExtOptions            DHCPOpt = 52  // 1, 1/2/3
	DHCPOptMessageType           DHCPOpt = 53  // 1, 1-7
	DHCPOptServerID              DHCPOpt = 54  // 4, net.IP
	DHCPOptParamsRequest         DHCPOpt = 55  // n, []byte
	DHCPOptMessage               DHCPOpt = 56  // n, 3
	DHCPOptMaxMessageSize        DHCPOpt = 57  // 2, uint16
	DHCPOptT1                    DHCPOpt = 58  // 4, uint32
	DHCPOptT2                    DHCPOpt = 59  // 4, uint32
	DHCPOptClassID               DHCPOpt = 60  // n, []byte
	DHCPOptClientID              DHCPOpt = 61  // n >=  2, []byte
	DHCPOptDomainSearch          DHCPOpt = 119 // n, string
	DHCPOptSIPServers            DHCPOpt = 120 // n, url
	DHCPOptClasslessStaticRoute  DHCPOpt = 121 //
	DHCPOptEnd                   DHCPOpt = 255
)

// RFC 2131 suggests using exponential backoff, starting with 4sec
// and randomized to +/- 1sec
const resendDelay0 = 4 * time.Second
const resendDelayMax = 32 * time.Second
const resendCount = 3

var errNoMoreTries = errors.New("no more tries")

const (
	leaseStateBound = iota
	leaseStateRenewing
	leaseStateRebinding
)

// This implementation uses 1 OS thread per lease. This is because
// all the network operations have to be done in network namespace
// of the interface. This can be improved by switching to the proper
// namespace for network ops and using fewer threads. However, this
// needs to be done carefully as dhcp4client ops are blocking.

// DHCPLease is the actual lease from a DHCP server
type DHCPLease struct {
	clientID      string
	ack           *dhcp4.Packet
	opts          dhcp4.Options
	link          netlink.Link
	renewalTime   time.Time
	rebindingTime time.Time
	expireTime    time.Time
	stopping      uint32
	stop          chan struct{}
	wg            sync.WaitGroup
}

// AcquireLease gets an DHCP lease and then maintains it in the background
// by periodically renewing it. The acquired lease can be released by
// calling DHCPLease.Stop()
func AcquireLease(clientID, ifName string, opts dhcp4.Options) (*DHCPLease, error) {
	errCh := make(chan error, 1)
	l := &DHCPLease{
		clientID: clientID,
		stop:     make(chan struct{}),
	}

	log.Printf("%v: acquiring lease", clientID)

	l.wg.Add(1)
	go func() error {
		defer l.wg.Done()

		link, err := netlink.LinkByName(ifName)
		if err != nil {
			errCh <- fmt.Errorf("error looking up %q: %v", ifName, err)
		}

		l.link = link

		if err = l.acquire(); err != nil {
			errCh <- err
		}

		log.Printf("%v: lease acquired, expiration is %v", l.clientID, l.expireTime)

		errCh <- nil

		l.maintain()
		return nil

	}()

	if err := <-errCh; err != nil {
		return nil, err
	}

	return l, nil
}

// Stop terminates the background task that maintains the lease
// and issues a DHCP Release
func (l *DHCPLease) Stop() {
	if atomic.CompareAndSwapUint32(&l.stopping, 0, 1) {
		close(l.stop)
	}
	l.wg.Wait()
}

func (l *DHCPLease) acquire() error {
	c, err := newDHCPClient(l.link)
	if err != nil {
		return err
	}
	defer c.Close()

	if (l.link.Attrs().Flags & net.FlagUp) != net.FlagUp {
		log.Printf("Link %q down. Attempting to set up", l.link.Attrs().Name)
		if err = netlink.LinkSetUp(l.link); err != nil {
			return err
		}
	}

	pkt, err := backoffRetry(func() (*dhcp4.Packet, error) {
		ok, ack, err := c.Request()
		switch {
		case err != nil:
			return nil, err
		case !ok:
			return nil, fmt.Errorf("DHCP server NACK'd own offer")
		default:
			return &ack, nil
		}
	})
	if err != nil {
		return err
	}

	return l.commit(pkt)
}

func (l *DHCPLease) commit(ack *dhcp4.Packet) error {
	opts := ack.ParseOptions()

	leaseTime, err := parseLeaseTime(opts)
	if err != nil {
		return err
	}

	rebindingTime, err := parseRebindingTime(opts)
	if err != nil || rebindingTime > leaseTime {
		// Per RFC 2131 Section 4.4.5, it should default to 85% of lease time
		rebindingTime = leaseTime * 85 / 100
	}

	renewalTime, err := parseRenewalTime(opts)
	if err != nil || renewalTime > rebindingTime {
		// Per RFC 2131 Section 4.4.5, it should default to 50% of lease time
		renewalTime = leaseTime / 2
	}

	now := time.Now()
	l.expireTime = now.Add(leaseTime)
	l.renewalTime = now.Add(renewalTime)
	l.rebindingTime = now.Add(rebindingTime)
	l.ack = ack
	l.opts = opts

	return nil
}

func (l *DHCPLease) maintain() {
	state := leaseStateBound

	for {
		var sleepDur time.Duration

		switch state {
		case leaseStateBound:
			sleepDur = l.renewalTime.Sub(time.Now())
			if sleepDur <= 0 {
				log.Printf("%v: renewing lease", l.clientID)
				state = leaseStateRenewing
				continue
			}

		case leaseStateRenewing:
			if err := l.renew(); err != nil {
				log.Printf("%v: %v", l.clientID, err)

				if time.Now().After(l.rebindingTime) {
					log.Printf("%v: renawal time expired, rebinding", l.clientID)
					state = leaseStateRebinding
				}
			} else {
				log.Printf("%v: lease renewed, expiration is %v", l.clientID, l.expireTime)
				state = leaseStateBound
			}

		case leaseStateRebinding:
			if err := l.acquire(); err != nil {
				log.Printf("%v: %v", l.clientID, err)

				if time.Now().After(l.expireTime) {
					log.Printf("%v: lease expired, bringing interface DOWN", l.clientID)
					l.downIface()
					return
				}
			} else {
				log.Printf("%v: lease rebound, expiration is %v", l.clientID, l.expireTime)
				state = leaseStateBound
			}
		}

		select {
		case <-time.After(sleepDur):

		case <-l.stop:
			if err := l.release(); err != nil {
				log.Printf("%v: failed to release DHCP lease: %v", l.clientID, err)
			}
			return
		}
	}
}

func (l *DHCPLease) downIface() {
	if err := netlink.LinkSetDown(l.link); err != nil {
		log.Printf("%v: failed to bring %v interface DOWN: %v", l.clientID, l.link.Attrs().Name, err)
	}
}

func (l *DHCPLease) renew() error {
	c, err := newDHCPClient(l.link)
	if err != nil {
		return err
	}
	defer c.Close()

	pkt, err := backoffRetry(func() (*dhcp4.Packet, error) {
		ok, ack, err := c.Renew(*l.ack)
		switch {
		case err != nil:
			return nil, err
		case !ok:
			return nil, fmt.Errorf("DHCP server did not renew lease")
		default:
			return &ack, nil
		}
	})
	if err != nil {
		return err
	}

	l.commit(pkt)
	return nil
}

func (l *DHCPLease) release() error {
	log.Printf("%v: releasing lease", l.clientID)

	c, err := newDHCPClient(l.link)
	if err != nil {
		return err
	}
	defer c.Close()

	if err = c.Release(*l.ack); err != nil {
		return fmt.Errorf("failed to send DHCPRELEASE")
	}

	return nil
}

func (l *DHCPLease) IPNet() (*net.IPNet, error) {
	mask := parseSubnetMask(l.opts)
	if mask == nil {
		return nil, fmt.Errorf("DHCP option Subnet Mask not found in DHCPACK")
	}

	return &net.IPNet{
		IP:   l.ack.YIAddr(),
		Mask: mask,
	}, nil
}

func (l *DHCPLease) Gateway() net.IP {
	return parseRouter(l.opts)
}

func (l *DHCPLease) Routes() []*types.Route {
	routes := []*types.Route{}

	// RFC 3442 states that if Classless Static Routes (option 121)
	// exist, we ignore Static Routes (option 33) and the Router/Gateway.
	opt121_routes := parseCIDRRoutes(l.opts)
	if len(opt121_routes) > 0 {
		return append(routes, opt121_routes...)
	}

	// Append Static Routes
	routes = append(routes, parseRoutes(l.opts)...)

	// The CNI spec says even if there is a gateway specified, we must
	// add a default route in the routes section.
	if gw := l.Gateway(); gw != nil {
		_, defaultRoute, _ := net.ParseCIDR("0.0.0.0/0")
		routes = append(routes, &types.Route{Dst: *defaultRoute, GW: gw})
	}

	return routes
}

// jitter returns a random value within [-span, span) range
func jitter(span time.Duration) time.Duration {
	return time.Duration(float64(span) * (2.0*rand.Float64() - 1.0))
}

func backoffRetry(f func() (*dhcp4.Packet, error)) (*dhcp4.Packet, error) {
	var baseDelay time.Duration = resendDelay0

	for i := 0; i < resendCount; i++ {
		pkt, err := f()
		if err == nil {
			return pkt, nil
		}

		log.Print(err)

		time.Sleep(baseDelay + jitter(time.Second))

		if baseDelay < resendDelayMax {
			baseDelay *= 2
		}
	}

	return nil, errNoMoreTries
}

func newDHCPClient(link netlink.Link) (*dhcp4client.Client, error) {
	pktsock, err := dhcp4client.NewPacketSock(link.Attrs().Index)
	if err != nil {
		return nil, err
	}

	return dhcp4client.New(
		dhcp4client.HardwareAddr(link.Attrs().HardwareAddr),
		dhcp4client.Timeout(5*time.Second),
		dhcp4client.Broadcast(false),
		dhcp4client.Connection(pktsock),
	)
}

func parseRouter(opts dhcp4.Options) net.IP {
	if opts, ok := opts[dhcp4.OptionRouter]; ok {
		if len(opts) == 4 {
			return net.IP(opts)
		}
	}
	return nil
}

func classfulSubnet(sn net.IP) net.IPNet {
	return net.IPNet{
		IP:   sn,
		Mask: sn.DefaultMask(),
	}
}

func parseRoutes(opts dhcp4.Options) []*types.Route {
	// StaticRoutes format: pairs of:
	// Dest = 4 bytes; Classful IP subnet
	// Router = 4 bytes; IP address of router

	routes := []*types.Route{}
	if opt, ok := opts[dhcp4.OptionStaticRoute]; ok {
		for len(opt) >= 8 {
			sn := opt[0:4]
			r := opt[4:8]
			rt := &types.Route{
				Dst: classfulSubnet(sn),
				GW:  r,
			}
			routes = append(routes, rt)
			opt = opt[8:]
		}
	}

	return routes
}

func parseCIDRRoutes(opts dhcp4.Options) []*types.Route {
	// See RFC4332 for format (http://tools.ietf.org/html/rfc3442)

	routes := []*types.Route{}
	if opt, ok := opts[dhcp4.OptionClasslessRouteFormat]; ok {
		for len(opt) >= 5 {
			width := int(opt[0])
			if width > 32 {
				// error: can't have more than /32
				return nil
			}
			// network bits are compacted to avoid zeros
			octets := 0
			if width > 0 {
				octets = (width-1)/8 + 1
			}

			if len(opt) < 1+octets+4 {
				// error: too short
				return nil
			}

			sn := make([]byte, 4)
			copy(sn, opt[1:octets+1])

			gw := net.IP(opt[octets+1 : octets+5])

			rt := &types.Route{
				Dst: net.IPNet{
					IP:   net.IP(sn),
					Mask: net.CIDRMask(width, 32),
				},
				GW: gw,
			}
			routes = append(routes, rt)

			opt = opt[octets+5:]
		}
	}
	return routes
}

func parseSubnetMask(opts dhcp4.Options) net.IPMask {
	mask, ok := opts[dhcp4.OptionSubnetMask]
	if !ok {
		return nil
	}

	return net.IPMask(mask)
}

func parseDuration(opts dhcp4.Options, code dhcp4.OptionCode, optName string) (time.Duration, error) {
	val, ok := opts[code]
	if !ok {
		return 0, fmt.Errorf("option %v not found", optName)
	}
	if len(val) != 4 {
		return 0, fmt.Errorf("option %v is not 4 bytes", optName)
	}

	secs := binary.BigEndian.Uint32(val)
	return time.Duration(secs) * time.Second, nil
}

func parseLeaseTime(opts dhcp4.Options) (time.Duration, error) {
	return parseDuration(opts, dhcp4.OptionIPAddressLeaseTime, "LeaseTime")
}

func parseRenewalTime(opts dhcp4.Options) (time.Duration, error) {
	return parseDuration(opts, dhcp4.OptionRenewalTimeValue, "RenewalTime")
}

func parseRebindingTime(opts dhcp4.Options) (time.Duration, error) {
	return parseDuration(opts, dhcp4.OptionRebindingTimeValue, "RebindingTime")
}
