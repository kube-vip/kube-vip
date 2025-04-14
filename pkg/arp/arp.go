package arp

import (
	"context"
	log "log/slog"
	"sync"
	"time"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/vip"
)

type Manager struct {
	instances map[string]*Instance
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
	i, ok := m.instances[instance.Name()]
	if !ok {
		log.Info("inserting ARP/NDP instance", "name", instance.Name())
		m.instances[instance.Name()] = instance
	} else {
		i.mu.Lock()
		defer i.mu.Unlock()
		i.counter++
	}
}

func (m *Manager) Remove(instance *Instance) {
	if i, ok := m.instances[instance.Name()]; ok {
		i.mu.Lock()
		defer i.mu.Unlock()
		if i.counter > 1 {
			i.counter--
		} else {
			log.Info("removing ARP/NDP instance", "name", instance.Name())
			delete(m.instances, instance.Name())
		}
	}
}

func (m *Manager) Count(name string) int {
	if i, ok := m.instances[name]; ok {
		i.mu.Lock()
		defer i.mu.Unlock()
		return i.counter
	}
	return 0
}

func (m *Manager) StartAdvertisement(ctx context.Context) {
	log.Info("Starting ARP/NDP advertisement")
	for {
		select {
		case <-ctx.Done(): // if cancel() execute
			return
		default:
			for _, instance := range m.instances {
				if instance.counter > 0 {
					ensureIPAndSendGratuitous(instance)
				}
			}
		}
		if m.config.ArpBroadcastRate < 500 {
			log.Error("arp broadcast rate is too low", "rate (ms)", m.config.ArpBroadcastRate, "setting to (ms)", "3000")
			m.config.ArpBroadcastRate = 3000
		}
		time.Sleep(time.Duration(m.config.ArpBroadcastRate) * time.Millisecond)
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
			log.Info("deleted and recreating address", "IP", ipString, "interface", iface)
			// if _, err := instance.network.AddIP(false); err != nil {
			// 	log.Error("failed to recreate address", "IP", ipString, "interface", iface)
			// }
		}
	}

	// Ensure the address exists on the interface before attempting to ARP
	// if instance.network.HasEndpoints() {
	if added, err := instance.network.AddIP(true); err != nil {
		log.Warn(err.Error())
	} else if added {
		log.Warn("Re-applied the VIP configuration", "ip", ipString, "interface", iface)
	}
	// }

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
