package networkinterface

import (
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
		return l
	}
	result := &Link{
		Intf: intf,
	}

	m.interfaces[intf.Attrs().Name] = result
	return result
}

func (m *Manager) Delete(intf netlink.Link) {
	delete(m.interfaces, intf.Attrs().Name)
}
