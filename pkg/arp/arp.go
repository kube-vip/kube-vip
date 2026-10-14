package arp

import (
	"context"
	"fmt"
	log "log/slog"
	"sync"
	"time"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/utils"
	"github.com/kube-vip/kube-vip/pkg/vip"
	"github.com/vishvananda/netlink"
)

const linkSubscriptionBuffer = 64

type Manager struct {
	mu        sync.Mutex
	instances map[string]*Instance
	config    *kubevip.Config
}

type Instance struct {
	network vip.Network
	ndp     *vip.NdpResponder
	counter int
}

func NewManager(config *kubevip.Config) *Manager {
	if config.ArpBroadcastRate < 500 {
		log.Warn("[ARP manager] arp broadcast rate is too low", "rate (ms)", config.ArpBroadcastRate, "setting to (ms)", "3000")
		config.ArpBroadcastRate = 3000
	}
	return &Manager{
		instances: make(map[string]*Instance),
		config:    config,
	}
}

func NewInstance(network vip.Network, ndp *vip.NdpResponder) *Instance {
	return &Instance{
		ndp:     ndp,
		network: network,
		counter: 1,
	}
}

func (i *Instance) Name() string {
	return i.network.ARPName()
}

func (m *Manager) Insert(instance *Instance) {
	m.mu.Lock()
	defer m.mu.Unlock()

	existing := m.instances[instance.Name()]
	if existing == nil {
		m.instances[instance.Name()] = instance
		log.Info("[ARP manager] inserting ARP/NDP instance", "name", instance.Name())
		return
	}
	existing.counter++
}

func (m *Manager) Remove(instance *Instance) {
	m.RemoveWithIPDelete(instance, true)
}

func (m *Manager) RemoveWithIPDelete(instance *Instance, deleteIP bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	i := m.instances[instance.Name()]
	if i != nil {
		i.counter--
		if i.counter == 0 {
			log.Info("[ARP manager] removing ARP/NDP instance", "name", instance.Name())
			if deleteIP {
				if _, err := instance.network.DeleteIP(); err != nil {
					log.Error("failed to delete IP", "address", instance.network.IP(), "err", err)
				}
			}
			delete(m.instances, instance.Name())
		}
	} else {
		log.Warn("[ARP manager] unable to remove the instance - instance not found", "name", instance.Name())
	}
}

func (m *Manager) Count(name string) int {
	m.mu.Lock()
	defer m.mu.Unlock()

	i := m.instances[name]
	if i != nil {
		return i.counter
	}
	return 0
}

func (m *Manager) StartAdvertisement(ctx context.Context, killFunc func()) {
	if m.config.LoseLeadership {
		var wg sync.WaitGroup
		defer wg.Wait()

		log.Info("[ARP manager] starting watching network device", "interface", m.config.Interface)

		duration := time.Duration(m.config.LoseLeadershipTimeoutSeconds) * time.Second
		timeout := time.NewTimer(duration)
		timeout.Stop()

		wg.Go(func() {
			select {
			case <-timeout.C:
				killFunc()
			case <-ctx.Done():
				return
			}
		})

		wg.Go(func() {
			if err := watch(ctx, m.config.Interface, func(s netlink.LinkOperState) {
				if isUp(s) {
					timeout.Stop()
					return
				}
				timeout.Reset(duration)
			}); err != nil {
				log.Error("[ARP manager] stopped watching interface", "err", err)
				killFunc()
			}
		})
	}

	log.Info("[ARP manager] starting ARP/NDP advertisement")

	ticker := time.NewTicker(time.Duration(m.config.ArpBroadcastRate) * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done(): // if cancel() execute
			return
		case <-ticker.C: // send gratuitous ARP/NDP on each tick
			m.advertiseAll()
		}
	}
}

func (m *Manager) advertiseAll() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, instance := range m.instances {
		if instance.counter > 0 {
			ensureIPAndSendGratuitous(instance)
		} else if _, err := instance.network.DeleteIP(); err != nil {
			log.Error("[ARP manager] failed to delete IP", "address", instance.network.IP(), "err", err)
		}
	}
}

// ensureIPAndSendGratuitous - adds IP to the interface if missing, and send
// either a gratuitous ARP or gratuitous NDP. Re-adds the interface if it is IPv6
// and in a dadfailed state.
func ensureIPAndSendGratuitous(instance *Instance) {
	iface := instance.network.Interface()
	ipString := instance.network.IP()

	// Check if IP is dadfailed
	if instance.network.IsDADFAIL() {
		log.Warn("IP address is in dadfailed state, removing config", "ip", ipString, "interface", iface)
		deleted, err := instance.network.DeleteIP()
		if err != nil {
			log.Warn(err.Error())
		}
		if deleted {
			log.Info("deleted and recreating address with NODAD flag to skip DAD", "IP", ipString, "interface", iface)
			// Re-add immediately without DAD check since we're recovering from DADFAILED
			// The AddIP function will set IFA_F_NODAD flag for IPv6 addresses when skipDAD=true
			if _, err := instance.network.AddIP(false, true); err != nil {
				log.Error("failed to recreate address after DADFAILED", "IP", ipString, "interface", iface, "err", err)
			} else {
				log.Info("successfully recreated address after DADFAILED recovery", "IP", ipString, "interface", iface)
			}
		}
		// Return early after DADFAILED recovery to avoid double IP addition
		return
	}

	// Normal case: add IP with precheck and normal DAD process
	if added, err := instance.network.AddIP(true, false); err != nil {
		log.Warn(err.Error())
	} else if added {
		log.Warn("Re-applied the VIP configuration", "ip", ipString, "interface", iface)
	}

	if utils.IsIPv6(ipString) {
		// Gratuitous NDP, will broadcast new MAC <-> IPv6 address
		if instance.ndp == nil {
			log.Error("NDP responder was not created")
		} else {
			err := instance.ndp.SendGratuitous(ipString)
			if err != nil {
				log.Warn(err.Error())
			}
		}
	} else {
		// Gratuitous ARP, will broadcast to new MAC <-> IPv4 address
		err := vip.ARPSendGratuitous(ipString, iface)
		if err != nil {
			log.Warn(err.Error())
		}
	}
}

// watch subscribing to the network interface events and calls handler
func watch(ctx context.Context, interfaceName string, operStateHandler func(netlink.LinkOperState)) error {
	ifname, err := netlink.LinkByName(interfaceName)
	if err != nil {
		return fmt.Errorf("failed to watch interface %q: %w", interfaceName, err)
	}

	// verify if this interface is physical device
	if _, ok := ifname.(*netlink.Device); !ok {
		return fmt.Errorf("interface %s is not physical, ignoring", interfaceName)
	}

	// The subscription is buffered and drained on exit: netlink parks its reader
	// goroutine on an unread send, which closing done alone does not release.
	events := make(chan netlink.LinkUpdate, linkSubscriptionBuffer)
	done := make(chan struct{})

	if err := netlink.LinkSubscribe(events, done); err != nil {
		return fmt.Errorf("failed to subscribe to the interface events: %w", err)
	}
	defer func() {
		close(done)
		drainLinkUpdates(events)
	}()

	//  handle initial state
	operStateHandler(ifname.Attrs().OperState)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event, ok := <-events:
			if !ok {
				return fmt.Errorf("interface events channel closed")
			}

			attrs := event.Attrs()
			// LinkSubscribe captures events for all network devices found
			// so we only care about vip interface
			if ifname.Attrs().Name != attrs.Name {
				continue
			}
			log.Debug("handling device change", "state", attrs.OperState)
			operStateHandler(attrs.OperState)
		}
	}
}

func isUp(operState netlink.LinkOperState) bool {
	return operState == netlink.OperUp
}

// drainLinkUpdates releases a netlink sender that is parked on an unread update
// so its goroutine can observe the closed subscription and exit.
func drainLinkUpdates(events <-chan netlink.LinkUpdate) {
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()

	for {
		select {
		case _, ok := <-events:
			if !ok {
				return
			}
		case <-timer.C:
			return
		}
	}
}
