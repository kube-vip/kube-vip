package networkinterface

import (
	log "log/slog"
	"sync"

	"github.com/vishvananda/netlink"
)

type Manager struct {
	interfaces map[string]*Link
}

type Link struct {
	Lock sync.Mutex
	Intf netlink.Link
}

func NewManager() *Manager {
	return &Manager{
		interfaces: make(map[string]*Link),
	}
}

func (m *Manager) Get(intf netlink.Link) *Link {
	if l, ok := m.interfaces[intf.Attrs().Name]; ok {
		updated, err := netlink.LinkByName(l.Intf.Attrs().Name)
		if err != nil {
			log.Error("failed to get interface %q: %w", l.Intf.Attrs().Name, err)
			return nil
		}
		l.Intf = updated
		return l
	}
	result := &Link{
		Intf: intf,
	}

	m.interfaces[intf.Attrs().Name] = result
	return result
}
