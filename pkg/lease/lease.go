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
// - lease for the object
// - isNewObject, which reports if it is a new object that is being handled
// - isSharedLease, which is true if object shares the lease with another object
// If object is new but not shared, we should start leaderelection and sync it
// If object is new and shared, we should only sync it as the leaderelection should be already handled
// If object is not new we should do nothing
func (m *Manager) Add(id ID, objectName string) (lease *Lease, isNewObject bool, isSharedLease bool) {
	m.lock.Lock()
	defer m.lock.Unlock()
	if _, isSharedLease = m.leases[id.NamespacedName()]; !isSharedLease {
		ctx, cancel := context.WithCancel(context.Background())
		m.leases[id.NamespacedName()] = newLease(ctx, cancel)
	}
	lease = m.leases[id.NamespacedName()]
	isNewObject = m.leases[id.NamespacedName()].add(objectName)
	return
}

// Delete removes the lease and cancels it if the lease counter equals 0.
func (m *Manager) Delete(id ID, objectName string) {
	m.lock.Lock()
	defer m.lock.Unlock()
	if _, exist := m.leases[id.NamespacedName()]; exist {
		m.leases[id.NamespacedName()].delete(objectName)
		if m.leases[id.NamespacedName()].cnt.Load() < 1 {
			m.leases[id.NamespacedName()].Cancel()
			delete(m.leases, id.NamespacedName())
		}
	}
}

// Get returns lease for the service.
func (m *Manager) Get(id ID) *Lease {
	m.lock.Lock()
	defer m.lock.Unlock()

	if lease, exist := m.leases[id.NamespacedName()]; exist {
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
func ServiceName(service *v1.Service) (string, string) {
	name, exists := service.Annotations[kubevip.ServiceLease]
	if !exists || name == "" {
		name = fmt.Sprintf("kubevip-%s", service.Name)
	}

	serviceLeaseParts := strings.Split(name, "/")
	namespace := service.Namespace

	if len(serviceLeaseParts) > 1 {
		namespace = serviceLeaseParts[0]
		name = serviceLeaseParts[1]
	}

	return namespace, name
}

func ServiceNamespacedName(service *v1.Service) string {
	return fmt.Sprintf("%s/%s", service.Namespace, service.Name)
}

func ObjectName(id ID, suffix string) string {
	return fmt.Sprintf("%s-%s", id.NamespacedName(), suffix)
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

type ID interface {
	Name() string
	Namespace() string
	NamespacedName() string
}

type CommonID struct {
	namespace string
	name      string
}

func NewID(leaseType, namespace, name string) ID {
	if leaseType == "etcd" {
		return newEtcdID(namespace, name)
	}
	return newKubernetesID(namespace, name)
}

func newKubernetesID(namespace, name string) ID {
	return &KubernetesID{
		CommonID: CommonID{
			namespace: namespace,
			name:      name,
		},
	}
}
func newEtcdID(namespace, name string) ID {
	return &EtcdID{
		CommonID: CommonID{
			namespace: namespace,
			name:      name,
		},
	}
}

func (c *CommonID) Name() string {
	return c.name
}

func (c *CommonID) Namespace() string {
	return c.namespace
}

type KubernetesID struct {
	CommonID
}

func (k *KubernetesID) NamespacedName() string {
	return fmt.Sprintf("%s/%s", k.namespace, k.name)
}

type EtcdID struct {
	CommonID
}

func (e *EtcdID) NamespacedName() string {
	return fmt.Sprintf("%s-%s", e.namespace, e.name)
}
