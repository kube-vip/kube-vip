package arp

import (
	"context"
	"fmt"
	log "log/slog"
	"sync"
	"time"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/vip"
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
	i, err := m.get(instance.Name())
	if err != nil {
		log.Error("[ARP manager] unable to remove the instance", "err", err)
		return
	}
	if i != nil {
		i.mu.Lock()
		defer i.mu.Unlock()
		if i.counter > 1 {
			i.counter--
		} else {
			log.Info("[ARP manager] removing ARP/NDP instance", "name", instance.Name())
			if _, err := instance.network.DeleteIP(); err != nil {
				log.Error("failed to delete IP", "address", instance.network.IP(), "err", err)
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

func (m *Manager) StartAdvertisement(ctx context.Context) {
	log.Info("[ARP manager] starting ARP/NDP advertisement")
	for {
		select {
		case <-ctx.Done(): // if cancel() execute
			return
		default:
			m.instances.Range(func(_ any, instance any) bool {
				if i, ok := instance.(*Instance); ok {
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
		if m.config.ArpBroadcastRate < 500 {
			log.Warn("[ARP manager] arp broadcast rate is too low", "rate (ms)", m.config.ArpBroadcastRate, "setting to (ms)", "3000")
			m.config.ArpBroadcastRate = 3000
		}
		time.Sleep(time.Duration(m.config.ArpBroadcastRate) * time.Millisecond)
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
			log.Info("deleted and recreating address", "IP", ipString, "interface", iface)
		}
	}

	if added, err := instance.network.AddIP(true); err != nil {
		log.Warn(err.Error())
	} else if added {
		log.Warn("Re-applied the VIP configuration", "ip", ipString, "interface", iface)
	}

	if vip.IsIPv6(ipString) {
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
