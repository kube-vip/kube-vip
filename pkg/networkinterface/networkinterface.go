package networkinterface

import (
	"sync"

	"github.com/vishvananda/netlink"
)

type Manager struct {
	mutex      sync.Mutex
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
	attrs := intf.Attrs()

	m.mutex.Lock()
	defer m.mutex.Unlock()

	if l, ok := m.interfaces[attrs.Name]; ok && l.Intf.Attrs().Index == attrs.Index {
		return l
	}

	result := &Link{
		Intf: intf,
	}

	m.interfaces[attrs.Name] = result
	return result
}
