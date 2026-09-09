package networkinterface

import (
	"sync"

	"github.com/vishvananda/netlink"
)

type Manager struct {
	lock       sync.Mutex
	interfaces map[string]*Link
}

type Link struct {
	mu   sync.Mutex
	intf netlink.Link
}

func NewManager() *Manager {
	return &Manager{
		interfaces: make(map[string]*Link),
	}
}

func (m *Manager) Get(intf netlink.Link) *Link {
	if intf == nil || intf.Attrs() == nil {
		return nil
	}
	attrs := intf.Attrs()

	m.lock.Lock()
	defer m.lock.Unlock()
	if link, ok := m.interfaces[attrs.Name]; ok {
		link.replace(intf)
		return link
	}

	link := &Link{intf: intf}
	m.interfaces[attrs.Name] = link
	return link
}

func (l *Link) WithInterface(run func(netlink.Link) error) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return run(l.intf)
}

func (l *Link) replace(intf netlink.Link) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.intf = intf
}

func (m *Manager) Len() int {
	m.lock.Lock()
	defer m.lock.Unlock()
	return len(m.interfaces)
}
