package lease

import (
	"context"
	"fmt"
	"sync"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	v1 "k8s.io/api/core/v1"
)

// Manager is used to manage leases.
type Manager struct {
	leases map[string]*Lease
	lock   sync.Mutex
}

// NewManager creates new lease manager.
func NewManager() *Manager {
	return &Manager{
		leases: make(map[string]*Lease),
	}
}

// Add adds lease or incerements counter if lease is alreay used.
func (m *Manager) Add(service *v1.Service) (*Lease, bool) {
	m.lock.Lock()
	defer m.lock.Unlock()
	_, id := GetName(service)
	if _, exist := m.leases[id]; !exist {
		ctx, cancel := context.WithCancel(context.Background())
		m.leases[id] = newLease(ctx, cancel)
		return m.leases[id], true
	}

	m.leases[id].increment()
	return m.leases[id], false
}

// Delete decrements lease counter and removes the lease if counter equals 0.
func (m *Manager) Delete(service *v1.Service) {
	m.lock.Lock()
	defer m.lock.Unlock()
	_, id := GetName(service)
	if _, exist := m.leases[id]; exist {
		m.leases[id].decrement()
		if m.leases[id].cnt < 1 {
			delete(m.leases, id)
		}
	}
}

// Get returns lease for the service.
func (m *Manager) Get(service *v1.Service) *Lease {
	m.lock.Lock()
	defer m.lock.Unlock()
	_, id := GetName(service)

	if lease, exist := m.leases[id]; exist {
		return lease
	}
	return nil
}

// GetLeaderContext returns leder context for the service.
func (m *Manager) GetLeaderContext(service *v1.Service) context.Context {
	m.lock.Lock()
	defer m.lock.Unlock()
	_, id := GetName(service)
	if _, ok := m.leases[id]; !ok {
		return nil
	}
	return m.leases[id].Ctx
}

// Lease holds lease data.
type Lease struct {
	cnt     uint
	Lock    *sync.Mutex
	Ctx     context.Context
	Cancel  context.CancelFunc
	Started chan any
}

func newLease(ctx context.Context, cancel context.CancelFunc) *Lease {
	return &Lease{
		Ctx:     ctx,
		Cancel:  cancel,
		cnt:     1,
		Lock:    new(sync.Mutex),
		Started: make(chan any),
	}
}

func (l *Lease) increment() {
	l.Lock.Lock()
	defer l.Lock.Unlock()
	l.cnt++
}

func (l *Lease) decrement() {
	l.Lock.Lock()
	defer l.Lock.Unlock()
	if l.cnt == 0 {
		return
	}
	l.cnt--
	if l.cnt < 1 {
		l.Cancel()
	}
}

// GetName gets lease name and id for the service.
func GetName(service *v1.Service) (string, string) {
	serviceLease, exists := service.Annotations[kubevip.ServiceLease]
	if !exists || serviceLease == "" {
		serviceLease = fmt.Sprintf("kubevip-%s", service.Name)
	}
	serviceLeaseID := fmt.Sprintf("%s/%s", serviceLease, service.Namespace)
	return serviceLease, serviceLeaseID
}

// UsesCommon checks if service uses common lease feature.
func UsesCommon(service *v1.Service) bool {
	_, common := service.Annotations[kubevip.ServiceLease]
	return common
}
