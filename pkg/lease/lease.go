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

// Add creates or retrieves the lease identified by id.
func (m *Manager) Add(ctx context.Context, id ID) *Lease {
	m.lock.Lock()
	defer m.lock.Unlock()
	return m.addLocked(ctx, id)
}

// Acquire creates or retrieves a lease and atomically registers objectName as a
// member. The returned bool reports whether this object was newly registered.
func (m *Manager) Acquire(ctx context.Context, id ID, objectName string) (*Lease, bool) {
	m.lock.Lock()
	defer m.lock.Unlock()

	lease := m.addLocked(ctx, id)
	return lease, lease.Add(objectName)
}

// Claim atomically registers objectName against an existing lease. It returns
// nil when the lease was retired before the caller could join it.
func (m *Manager) Claim(id ID, objectName string) (*Lease, bool) {
	m.lock.Lock()
	defer m.lock.Unlock()

	lease, exists := m.leases[id.NamespacedName()]
	if !exists {
		return nil, false
	}
	return lease, lease.Add(objectName)
}

func (m *Manager) addLocked(ctx context.Context, id ID) *Lease {

	// A lease whose context is already cancelled cannot be handed out again:
	// anything derived from it would be cancelled straight away. Replace it.
	if l, exists := m.leases[id.NamespacedName()]; !exists || l.Ctx.Err() != nil {
		leaseCtx, leaseCancel := context.WithCancel(ctx)
		m.leases[id.NamespacedName()] = newLease(leaseCtx, leaseCancel)
	}

	return m.leases[id.NamespacedName()]
}

// Delete removes the object from the lease it was added to and cancels that lease
// once its last object is gone. With a common lease, the siblings that still use
// it keep it alive.
//
// The lease the caller was given has to be passed in, because cleanup is usually
// deferred to a goroutine that runs long after the object went away. By then the
// lease of that name may already have been replaced, for instance because the
// service was torn down and rebuilt, and cancelling the replacement would leave
// the service unhandled. A stale caller is therefore ignored.
//
// Teardown paths have to call this synchronously rather than leaving it to the
// deferred cleanup: until the lease is out of the map, Add hands the same
// instance back, so a service that is rebuilt straight away gets parented to a
// lease that the pending cleanup is about to cancel.
func (m *Manager) Delete(id ID, objectName string, l *Lease) {
	m.lock.Lock()
	defer m.lock.Unlock()

	current := m.currentFor(id, l)
	if current == nil {
		return
	}

	current.delete(objectName)
	if current.cnt.Load() < 1 {
		m.retire(id, current)
	}
}

// currentFor returns the registered lease for id, or nil when the caller is
// stale, meaning the lease it holds is no longer the registered one. Callers have
// to hold m.lock.
func (m *Manager) currentFor(id ID, l *Lease) *Lease {
	current, exist := m.leases[id.NamespacedName()]
	if !exist || (l != nil && current != l) {
		return nil
	}
	return current
}

// retire cancels the lease and drops it from the manager. Callers have to hold
// m.lock.
func (m *Manager) retire(id ID, l *Lease) {
	l.Cancel()
	delete(m.leases, id.NamespacedName())
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
	Ctx      context.Context
	Cancel   context.CancelFunc
	services sync.Map
	cnt      atomic.Int64
	Elected  atomic.Bool
	stateMu  sync.Mutex
	running  bool
	changed  chan struct{}
}

func newLease(ctx context.Context, cancel context.CancelFunc) *Lease {
	return &Lease{
		Ctx:     ctx,
		Cancel:  cancel,
		changed: make(chan struct{}),
	}
}

// Add adds the object to the lease and increments counter
// it will return true if object was added
func (l *Lease) Add(name string) bool {
	if _, exists := l.services.LoadOrStore(name, true); !exists {
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

func (l *Lease) BeginElection() bool {
	l.stateMu.Lock()
	defer l.stateMu.Unlock()
	if l.Elected.Load() || l.running {
		return false
	}
	l.running = true
	l.signalStateLocked()
	return true
}

func (l *Lease) ElectionStarted() {
	l.stateMu.Lock()
	defer l.stateMu.Unlock()
	if l.Elected.Load() {
		return
	}
	l.Elected.Store(true)
	l.running = false
	l.signalStateLocked()
}

func (l *Lease) ElectionStopped() {
	l.stateMu.Lock()
	defer l.stateMu.Unlock()
	if !l.Elected.Load() && !l.running {
		return
	}
	l.Elected.Store(false)
	l.running = false
	l.signalStateLocked()
}

// WaitForLeader waits for an in-flight lease election to either elect a leader
// or finish without one. It never holds the lease state mutex while waiting.
func (l *Lease) WaitForLeader(ctx context.Context) bool {
	for {
		l.stateMu.Lock()
		if l.Elected.Load() {
			l.stateMu.Unlock()
			return true
		}
		if !l.running {
			l.stateMu.Unlock()
			return false
		}
		changed := l.changed
		l.stateMu.Unlock()

		select {
		case <-ctx.Done():
			return false
		case <-l.Ctx.Done():
			return false
		case <-changed:
		}
	}
}

// WaitForElectionEnd waits until an elected lease loses its leader. It never
// holds the lease state mutex while waiting.
func (l *Lease) WaitForElectionEnd(ctx context.Context) {
	for {
		l.stateMu.Lock()
		if !l.Elected.Load() {
			l.stateMu.Unlock()
			return
		}
		changed := l.changed
		l.stateMu.Unlock()

		select {
		case <-ctx.Done():
			return
		case <-l.Ctx.Done():
			return
		case <-changed:
		}
	}
}

func (l *Lease) signalStateLocked() {
	close(l.changed)
	l.changed = make(chan struct{})
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
