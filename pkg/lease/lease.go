package lease

import (
	"context"
	"fmt"
	log "log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"

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

// Add adds lease to the manager.
// It returns three values:
// - lease for the service
// - newService, which reports if it is a new service that is being handled
// - sharedLease, which is true if service shares the lease with another service
// If service is new but not shared, we should start leaderelection and sync it
// If service is new and shared, we should only sync it as the leaderelection should be already handled
// If service is not new we should do nothing
func (m *Manager) Add(leaseID, objectName string) (lease *Lease, newService bool, sharedLease bool) {
	m.lock.Lock()
	defer m.lock.Unlock()
	if _, sharedLease = m.leases[leaseID]; !sharedLease {
		ctx, cancel := context.WithCancel(context.Background())
		m.leases[leaseID] = newLease(ctx, cancel)
	}
	lease = m.leases[leaseID]
	newService = m.leases[leaseID].add(objectName)
	return
}

// Delete removes the lease and cancels it if the lease counter equals 0.
func (m *Manager) Delete(leaseID, objectName string) {
	m.lock.Lock()
	defer m.lock.Unlock()
	if _, exist := m.leases[leaseID]; exist {
		m.leases[leaseID].delete(objectName)
		if m.leases[leaseID].cnt.Load() < 1 {
			m.leases[leaseID].Cancel()
			delete(m.leases, leaseID)
		}
	}
}

// Get returns lease for the service.
func (m *Manager) Get(leaseID string) *Lease {
	m.lock.Lock()
	defer m.lock.Unlock()

	if lease, exist := m.leases[leaseID]; exist {
		return lease
	}
	return nil
}

// Lease holds lease data.
type Lease struct {
	Lock     sync.Mutex
	Ctx      context.Context
	Cancel   context.CancelFunc
	Started  chan any
	services sync.Map
	cnt      atomic.Int64
}

func newLease(ctx context.Context, cancel context.CancelFunc) *Lease {
	return &Lease{
		Ctx:      ctx,
		Cancel:   cancel,
		Started:  make(chan any),
		services: sync.Map{},
		cnt:      atomic.Int64{},
	}
}

// add adds the service to the lease and increments counter
// it will return true if service was added
func (l *Lease) add(service string) bool {
	if _, exists := l.services.Load(service); !exists {
		l.services.Store(service, true)
		l.cnt.Add(1)
		return true
	}
	return false
}

// delete removes the service from the lease and decrements the counter
func (l *Lease) delete(service string) {
	if _, exists := l.services.Load(service); exists {
		l.services.Delete(service)
		l.cnt.Add(-1)
	}
}

// ServiceName gets lease name and id for the service.
func ServiceName(service *v1.Service) (string, string, string) {
	serviceLease, exists := service.Annotations[kubevip.ServiceLease]
	if !exists || serviceLease == "" {
		serviceLease = fmt.Sprintf("kubevip-%s", service.Name)
	}

	serviceLeaseParts := strings.Split(serviceLease, "/")
	namespace := service.Namespace

	if len(serviceLeaseParts) > 1 {
		namespace = serviceLeaseParts[0]
		serviceLease = serviceLeaseParts[1]
	}

	serviceLeaseID := fmt.Sprintf("%s/%s", namespace, serviceLease)

	return serviceLease, serviceLeaseID, namespace
}

func ServiceNamespacedName(service *v1.Service) string {
	return fmt.Sprintf("%s/%s", service.Namespace, service.Name)
}

func NamespaceName(lease string, c *kubevip.Config) (string, string) {
	leaseName := lease
	leasnameParts := strings.Split(lease, "/")
	var ns string
	var err error
	if len(leasnameParts) > 1 {
		ns = leasnameParts[0]
		leaseName = leasnameParts[1]
	} else {
		ns, err = returnNamespace()
		if err != nil {
			log.Warn("unable to auto-detect namespace, dropping to config", "namespace", c.Namespace)
			ns = c.Namespace
		}
	}
	return ns, leaseName
}

func returnNamespace() (string, error) {
	if data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		if ns := strings.TrimSpace(string(data)); len(ns) > 0 {
			return ns, nil
		}
		return "", err
	}
	return "", fmt.Errorf("unable to find Namespace")
}
