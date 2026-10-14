package lease

import (
	"context"
	"fmt"
	log "log/slog"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	v1 "k8s.io/api/core/v1"
)

// Manager is used to manage leases.
type Manager struct {
	leases map[string]*Lease
	lock   sync.Mutex
}

// RegistrationSpec describes one local participant in a shared Lease.
type RegistrationSpec struct {
	Name        string
	VIPProvider VIPProvider
}

// Registration is a generation-safe handle to one local Lease participant.
// Release only affects the exact Lease instance against which the participant
// was registered, so delayed cleanup cannot retire a replacement Lease.
type Registration struct {
	manager *Manager
	id      ID
	spec    RegistrationSpec
	lease   *Lease
	owned   bool
	once    sync.Once
	retired bool
}

// Release removes this registration once and reports whether it retired the
// shared Lease.
func (r *Registration) Release() bool {
	if r == nil || r.manager == nil || !r.owned {
		return false
	}
	r.once.Do(func() {
		r.retired = r.manager.Delete(r.id, r.spec.Name, r.lease)
	})
	return r.retired
}

// NewManager creates new lease manager.
func NewManager() *Manager {
	return &Manager{
		leases: make(map[string]*Lease),
	}
}

// Acquire creates or retrieves a lease and atomically registers objectName
// together with its current VIP ownership provider. The returned bool reports
// whether this object was newly registered.
func (m *Manager) Acquire(ctx context.Context, id ID, objectName string,
	vipProvider VIPProvider) (*Lease, bool) {
	m.lock.Lock()
	defer m.lock.Unlock()

	lease := m.addLocked(ctx, id)
	return lease, lease.addWithVIPProvider(objectName, vipProvider)
}

// AcquireRegistrations atomically registers all supplied participants against
// one Lease. Either every registration is visible to OwnedVIPs or none of the
// registrations from this call is retained.
func (m *Manager) AcquireRegistrations(ctx context.Context, id ID,
	specs []RegistrationSpec) (*Lease, map[string]*Registration, error) {
	m.lock.Lock()
	defer m.lock.Unlock()

	registeredLease := m.addLocked(ctx, id)
	if err := registeredLease.addAll(specs); err != nil {
		if registeredLease.count() == 0 {
			m.retire(id, registeredLease)
		}
		return nil, nil, err
	}
	registrations := make(map[string]*Registration, len(specs))
	for _, spec := range specs {
		registrations[spec.Name] = &Registration{
			manager: m, id: id, spec: spec, lease: registeredLease, owned: true,
		}
	}
	return registeredLease, registrations, nil
}

// ClaimWithVIPProvider atomically registers objectName and its VIP ownership
// provider against an existing lease.
func (m *Manager) ClaimWithVIPProvider(id ID, objectName string, vipProvider VIPProvider) (*Lease, bool) {
	m.lock.Lock()
	defer m.lock.Unlock()

	lease, exists := m.leases[id.NamespacedName()]
	if !exists {
		return nil, false
	}
	return lease, lease.addWithVIPProvider(objectName, vipProvider)
}

// ClaimRegistration registers a participant against an existing Lease and
// returns a generation-safe release handle. It returns nil after retirement.
func (m *Manager) ClaimRegistration(id ID, spec RegistrationSpec) (*Registration, bool) {
	registeredLease, added := m.ClaimWithVIPProvider(id, spec.Name, spec.VIPProvider)
	if registeredLease == nil {
		return nil, false
	}
	return &Registration{manager: m, id: id, spec: spec, lease: registeredLease, owned: added}, added
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
// once its last object is gone. It reports whether the lease was retired. With a
// common lease, the siblings that still use it keep it alive.
//
// The lease the caller was given has to be passed in, because cleanup is usually
// deferred to a goroutine that runs long after the object went away. By then the
// lease of that name may already have been replaced, for instance because the
// service was torn down and rebuilt, and cancelling the replacement would leave
// the service unhandled. A stale caller is therefore ignored.
//
// Teardown paths have to call this synchronously rather than leaving it to the
// deferred cleanup: until the lease is out of the map, Acquire hands the same
// instance back, so a service that is rebuilt straight away gets parented to a
// lease that the pending cleanup is about to cancel.
func (m *Manager) Delete(id ID, objectName string, l *Lease) bool {
	m.lock.Lock()
	defer m.lock.Unlock()

	current := m.currentFor(id, l)
	if current == nil {
		return false
	}

	current.delete(objectName)
	if current.count() < 1 {
		m.retire(id, current)
		return true
	}
	return false
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
	Ctx       context.Context
	Cancel    context.CancelFunc
	membersMu sync.RWMutex
	services  map[string]member
	stateMu   sync.Mutex
	election  *electionGeneration
}

type electionPhase uint8

const (
	electionIdle electionPhase = iota
	electionCampaigning
	electionLeading
)

// electionGeneration owns the one-shot notifications for one local election.
// decided closes when the campaign either becomes leader or stops before doing
// so. done closes whenever the generation stops.
type electionGeneration struct {
	phase   electionPhase
	decided chan struct{}
	done    chan struct{}
}

// ElectionSession identifies one generation of the local runner coordinating
// a shared Lease. Only the session that started a generation may change its
// state; delayed callbacks from older generations are ignored.
type ElectionSession struct {
	lease      *Lease
	generation *electionGeneration
	owner      bool
}

// ElectionRole describes whether a local participant runs the shared election
// backend or observes a runner already started by another local subsystem.
type ElectionRole uint8

const (
	ElectionRunner ElectionRole = iota
	ElectionObserver
)

// ElectionParticipation is the common entry point for users sharing an
// election. Runner ownership is local and does not imply cluster leadership.
type ElectionParticipation struct {
	Session *ElectionSession
	Role    ElectionRole
}

func (p ElectionParticipation) RunsCampaign() bool { return p.Role == ElectionRunner }

// VIPProvider returns the VIPs currently owned by one local Lease member.
// Implementations must be safe for concurrent use and must not return mutable
// state that can change while the caller is reading it.
type VIPProvider func() []string

// StaticVIPProvider returns a provider backed by an immutable copy of vips.
func StaticVIPProvider(vips []string) VIPProvider {
	owned := append([]string(nil), vips...)
	return func() []string {
		return append([]string(nil), owned...)
	}
}

type member struct {
	vipProvider VIPProvider
}

func newLease(ctx context.Context, cancel context.CancelFunc) *Lease {
	return &Lease{
		Ctx:      ctx,
		Cancel:   cancel,
		services: make(map[string]member),
	}
}

// NewElectionContext returns a context for one election runner. Cancelling it
// stops only that runner; the Lease context remains live until its final member
// is deleted from the Manager.
func (l *Lease) NewElectionContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(l.Ctx)
	stopParent := context.AfterFunc(parent, cancel)
	return ctx, func() {
		stopParent()
		cancel()
	}
}

// addWithVIPProvider adds an object and its VIP ownership provider to the
// lease. Re-adding the same object leaves the original registration intact.
func (l *Lease) addWithVIPProvider(name string, vipProvider VIPProvider) bool {
	l.membersMu.Lock()
	defer l.membersMu.Unlock()
	if _, exists := l.services[name]; exists {
		return false
	}
	l.services[name] = member{vipProvider: vipProvider}
	return true
}

func (l *Lease) addAll(specs []RegistrationSpec) error {
	l.membersMu.Lock()
	defer l.membersMu.Unlock()

	seen := make(map[string]struct{}, len(specs))
	for _, spec := range specs {
		if spec.Name == "" {
			return fmt.Errorf("register Lease participants: empty registration name")
		}
		if _, duplicate := seen[spec.Name]; duplicate {
			return fmt.Errorf("register Lease participants: duplicate registration %q", spec.Name)
		}
		if _, exists := l.services[spec.Name]; exists {
			return fmt.Errorf("register Lease participants: registration %q already exists", spec.Name)
		}
		seen[spec.Name] = struct{}{}
	}
	for _, spec := range specs {
		l.services[spec.Name] = member{vipProvider: spec.VIPProvider}
	}
	return nil
}

// OwnedVIPs returns a stable, deduplicated snapshot of VIPs contributed by all
// local members sharing this Lease.
func (l *Lease) OwnedVIPs() []string {
	l.membersMu.RLock()
	providers := make([]VIPProvider, 0, len(l.services))
	for _, registered := range l.services {
		if registered.vipProvider != nil {
			providers = append(providers, registered.vipProvider)
		}
	}
	l.membersMu.RUnlock()

	unique := make(map[string]struct{})
	for _, provider := range providers {
		for _, vip := range provider() {
			if vip != "" {
				unique[vip] = struct{}{}
			}
		}
	}
	vips := make([]string, 0, len(unique))
	for vip := range unique {
		vips = append(vips, vip)
	}
	slices.Sort(vips)
	return vips
}

// delete removes one participant from the Lease.
func (l *Lease) delete(service string) {
	l.membersMu.Lock()
	defer l.membersMu.Unlock()
	delete(l.services, service)
}

func (l *Lease) count() int {
	l.membersMu.RLock()
	defer l.membersMu.RUnlock()
	return len(l.services)
}

// JoinElection starts a local election generation or observes the current one.
func (l *Lease) JoinElection() ElectionParticipation {
	l.stateMu.Lock()
	defer l.stateMu.Unlock()

	runsCampaign := l.election == nil
	if runsCampaign {
		l.election = &electionGeneration{
			phase:   electionCampaigning,
			decided: make(chan struct{}),
			done:    make(chan struct{}),
		}
	}
	role := ElectionObserver
	if runsCampaign {
		role = ElectionRunner
	}
	return ElectionParticipation{
		Session: &ElectionSession{lease: l, generation: l.election, owner: runsCampaign},
		Role:    role,
	}
}

// Started marks this session as leading. It returns false for observers,
// already-stopped sessions, and sessions superseded by a newer generation.
func (s *ElectionSession) Started() bool {
	if s == nil || s.lease == nil || s.generation == nil || !s.owner {
		return false
	}
	l := s.lease
	l.stateMu.Lock()
	defer l.stateMu.Unlock()
	if l.election != s.generation || s.generation.phase != electionCampaigning {
		return false
	}
	s.generation.phase = electionLeading
	close(s.generation.decided)
	return true
}

// Stopped ends this session. It is safe to call repeatedly: once another
// generation starts, a delayed call from this session cannot stop it.
func (s *ElectionSession) Stopped() bool {
	if s == nil || s.lease == nil || s.generation == nil || !s.owner {
		return false
	}
	l := s.lease
	l.stateMu.Lock()
	defer l.stateMu.Unlock()
	if l.election != s.generation || s.generation.phase == electionIdle {
		return false
	}
	if s.generation.phase == electionCampaigning {
		close(s.generation.decided)
	}
	s.generation.phase = electionIdle
	l.election = nil
	close(s.generation.done)
	return true
}

// IsLeading reports whether this exact election generation is still leading.
func (s *ElectionSession) IsLeading() bool {
	if s == nil || s.lease == nil || s.generation == nil {
		return false
	}
	phase, current := s.lease.electionState(s.generation)
	return current && phase == electionLeading
}

// IsCurrent reports whether this session still represents the Lease's current
// election generation. It can be used by delayed callbacks without ending the
// session; finalization remains the runner's responsibility.
func (s *ElectionSession) IsCurrent() bool {
	if s == nil || s.lease == nil || s.generation == nil {
		return false
	}
	phase, current := s.lease.electionState(s.generation)
	return current && phase != electionIdle
}

// WaitForLeader waits for this election generation to either become leader or
// end. A replacement generation is not silently adopted.
func (s *ElectionSession) WaitForLeader(ctx context.Context) bool {
	if s == nil || s.lease == nil || s.generation == nil {
		return false
	}
	for {
		phase, current := s.lease.electionState(s.generation)
		if !current {
			return false
		}
		switch phase {
		case electionLeading:
			return true
		case electionIdle:
			return false
		}

		select {
		case <-ctx.Done():
			return false
		case <-s.lease.Ctx.Done():
			return false
		case <-s.generation.decided:
		}
	}
}

// WaitForEnd waits until this election generation is no longer leading.
func (s *ElectionSession) WaitForEnd(ctx context.Context) {
	if s == nil || s.lease == nil || s.generation == nil {
		return
	}
	phase, current := s.lease.electionState(s.generation)
	if !current || phase != electionLeading {
		return
	}

	select {
	case <-ctx.Done():
	case <-s.lease.Ctx.Done():
	case <-s.generation.done:
	}
}

func (l *Lease) electionState(generation *electionGeneration) (electionPhase, bool) {
	l.stateMu.Lock()
	defer l.stateMu.Unlock()
	if generation == nil || l.election != generation {
		return electionIdle, false
	}
	return generation.phase, true
}

// ServiceName gets lease name and id for the service.
func ServiceName(service *v1.Service) (string, string) {
	return ServiceNameFor(service.Namespace, service.Name, service.Annotations[kubevip.ServiceLease])
}

func ServiceNameFor(namespace, serviceName, leaseName string) (string, string) {
	name := leaseName
	if name == "" {
		name = fmt.Sprintf("kubevip-%s", serviceName)
	}

	serviceLeaseParts := strings.Split(name, "/")

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
