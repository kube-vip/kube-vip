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

type Manager struct {
	instances sync.Map
	config    *kubevip.Config
}

type Instance struct {
	network vip.Network
	ndp     *vip.NdpResponder
	mu      sync.Mutex
	counter int
}

func NewManager(config *kubevip.Config) *Manager {
	if config.ArpBroadcastRate < 500 {
		log.Warn("[ARP manager] arp broadcast rate is too low", "rate (ms)", config.ArpBroadcastRate, "setting to (ms)", "3000")
		config.ArpBroadcastRate = 3000
	}
	return &Manager{
		config: config,
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
	i, err := m.get(instance.Name())
	if err != nil {
		log.Error("[ARP manager] unable to insert instance", "err", err)
		return
	}
	if i == nil {
		log.Info("[ARP manager] inserting ARP/NDP instance", "name", instance.Name())
		m.instances.Store(instance.Name(), instance)
	} else {
		i.mu.Lock()
		defer i.mu.Unlock()
		i.counter++
	}
}

func (m *Manager) Remove(instance *Instance) {
	m.RemoveWithIPDelete(instance, true)
}

// RemoveOnLeadershipLoss removes an ARP instance when leadership is lost
func (m *Manager) RemoveOnLeadershipLoss(instance *Instance) {
	// Use the inverse of PreserveVIPOnLeadershipLoss to decide whether to delete the IP
	// If preserve is true, don't delete IP (deleteIP = false)
	// If preserve is false, delete IP (deleteIP = true), This is the legacy behavior
	deleteIP := !m.config.PreserveVIPOnLeadershipLoss
	m.RemoveWithIPDelete(instance, deleteIP)
}

func (m *Manager) RemoveWithIPDelete(instance *Instance, deleteIP bool) {
	i, err := m.get(instance.Name())
	if err != nil {
		log.Error("[ARP manager] unable to remove the instance", "err", err)
		return
	}
	if i != nil {
		i.mu.Lock()
		defer i.mu.Unlock()
		i.counter--
		if i.counter == 0 {
			log.Info("[ARP manager] removing ARP/NDP instance", "name", instance.Name())
			if deleteIP {
				if _, err := instance.network.DeleteIP(); err != nil {
					log.Error("failed to delete IP", "address", instance.network.IP(), "err", err)
				}
			}
			m.instances.Delete(instance.Name())
		}
	} else {
		log.Warn("[ARP manager] unable to remove the instance - instance not found", "name", instance.Name())
	}
}

func (m *Manager) Count(name string) int {
	i, err := m.get(name)
	if err != nil {
		log.Error("[ARP manager] unable to count instance", "err", err)
		return -1
	}
	if i != nil {
		i.mu.Lock()
		defer i.mu.Unlock()
		return i.counter
	}
	return 0
}

func (m *Manager) StartAdvertisement(ctx context.Context, killFunc func()) {
	if m.config.LoseLeadership {
		var wg sync.WaitGroup
		defer wg.Wait()

		watchCtx, cancelWatch := context.WithCancel(ctx)
		defer cancelWatch()

		log.Info("[ARP manager] starting watching network device", "interface", m.config.Interface)

		duration := time.Duration(m.config.LoseLeadershipTimeoutSeconds) * time.Second
		timeout := time.NewTimer(duration)
		timeout.Stop()

		wg.Go(func() {
			select {
			case <-timeout.C:
				killFunc()
			case <-watchCtx.Done():
				return
			}
		})

		wg.Go(func() {
			defer cancelWatch()
			if err := watch(watchCtx, m.config.Interface, func(s netlink.LinkOperState) {
				if isUp(s) {
					timeout.Stop()
					return
				}
				timeout.Reset(duration)
			}); err != nil {
				log.Warn("[ARP manager] stopped watching interface", "err", err)
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
			m.instances.Range(func(_ any, instance any) bool {
				if i, ok := instance.(*Instance); ok {
					i.mu.Lock()
					defer i.mu.Unlock()
					if i.counter > 0 {
						ensureIPAndSendGratuitous(i)
					} else {
						// this instance should not be advertised - delete the IP just in case...
						if _, err := i.network.DeleteIP(); err != nil {
							log.Error("[ARP manager] failed to delete IP", "address", i.network.IP(), "err", err)
						}
					}
				}
				return true
			})
		}
	}
}

func (m *Manager) get(name string) (*Instance, error) {
	i, exists := m.instances.Load(name)
	if !exists {
		return nil, nil
	}
	inst, ok := i.(*Instance)
	if !ok {
		return nil, fmt.Errorf("value for name %q is not of Instance pointer type", name)
	}
	return inst, nil
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

	events := make(chan netlink.LinkUpdate)
	done := make(chan struct{})

	if err := netlink.LinkSubscribe(events, done); err != nil {
		return fmt.Errorf("failed to subscribe to the interface events: %w", err)
	}
	defer close(done)

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
