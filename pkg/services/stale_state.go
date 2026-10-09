package services

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/vip"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// staleStateReconcileInterval is how often a node re-checks service state it
// owns locally (VIP bindings, node labels) against the service leases and drops
// what belongs to a lease this node does not hold. Tests shorten it.
var staleStateReconcileInterval = time.Minute * 2

// startStaleStateReconcile releases local state that a teardown interrupted by
// an unreachable Kubernetes API left behind (#1775): a VIP still bound on this
// node's interface and `service-provided.kube-vip.io/...` labels on this node
// for leases some other node holds.
//
// The full pass runs synchronously before the watcher starts - before this
// process can win any election, so it cannot race a fresh bind - and covers
// deployments without node labeling, whose stranded bindings have no label to
// point at them. The label-driven pass then repeats periodically: a best-effort
// label removal that failed during the API outage heals itself once the API
// returns, without waiting for a pod restart.
//
// The forced-election watcher and the regular one share a Processor, so this
// runs once per processor. Candidate services are the union of both watchers'
// partitions, which is what a single pass over all services gives.
//
// It only ever touches state on this node and only when the lease object
// proves another node holds (or owns) the service: if the API is unreachable,
// nothing is released.
func (p *Processor) startStaleStateReconcile(ctx context.Context) {
	if !p.config.EnableServicesElection || p.config.NodeName == "" {
		return
	}
	// etcd-backed service elections have no coordination Lease object to read
	// the holder from; skip conservatively.
	if p.config.LeaderElectionType == "etcd" {
		return
	}

	p.staleReconcileOnce.Do(func() {
		if err := p.reconcileStaleServiceState(ctx, true); err != nil {
			log.Warn("stale service state reconcile", "err", err)
		}

		go func() {
			ticker := time.NewTicker(staleStateReconcileInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					// Cheap pass driven by the labels attributed to this node.
					if err := p.reconcileStaleServiceState(ctx, false); err != nil {
						log.Debug("stale service state reconcile", "err", err)
					}
				}
			}
		}()
	})
}

// reconcileStaleServiceState runs one pass. full=true enumerates candidate
// services (also finds stale bindings in deployments with labeling disabled);
// full=false only re-checks the service-provided labels this node carries.
func (p *Processor) reconcileStaleServiceState(ctx context.Context, full bool) error {
	if p.clientSet == nil || p.rwClientSet == nil || p.config.NodeName == "" {
		return nil
	}

	node, err := p.clientSet.CoreV1().Nodes().Get(ctx, p.config.NodeName, metav1.GetOptions{})
	if err != nil {
		// Conservative: without the API there is no way to tell whether the
		// local state legitimately belongs to this node.
		return fmt.Errorf("unable to read node %q: %w", p.config.NodeName, err)
	}
	owned := serviceProvidedLabels(node.Labels)

	if full {
		list, err := p.rwClientSet.CoreV1().Services(p.config.ServiceNamespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return fmt.Errorf("unable to list services: %w", err)
		}
		for i := range list.Items {
			svc := &list.Items[i]
			if !p.isStaleStateCandidate(svc) {
				continue
			}
			if p.leaseHeldByOtherNode(ctx, svc) {
				p.dropStaleServiceState(svc, labelsForService(owned, svc))
			}
		}
		return nil
	}

	for key, value := range owned {
		name, namespace, ok := splitServiceProvidedLabelKey(key)
		if !ok {
			log.Warn("stale service state reconcile: unexpected label key", "label", key)
			continue
		}
		svc, err := p.rwClientSet.CoreV1().Services(namespace).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			// The Service is gone: the label is leftover bookkeeping and any
			// VIP recorded in its value is unowned.
			if p.releaseStaleLocalState(name, namespace, "", namespace, defaultServiceLeaseName(name),
				staleAddressesFromLabelValue(value), map[string]string{key: value}) {
				log.Info("stale service state reconcile: released state of a vanished service",
					"service", namespace+"/"+name, "label", key)
			}
			continue
		}
		if err != nil {
			log.Debug("stale service state reconcile: service lookup failed", "label", key, "err", err)
			continue
		}
		if p.leaseHeldByOtherNode(ctx, svc) {
			p.dropStaleServiceState(svc, map[string]string{key: value})
		}
	}
	return nil
}

// leaseHeldByOtherNode reports whether the service's lease exists elsewhere or
// is demonstrably not this node's. A lease this process is electing for, or
// that this process still runs an instance for, is never stale locally.
func (p *Processor) leaseHeldByOtherNode(ctx context.Context, svc *v1.Service) bool {
	if p.leadsServiceLocally(svc) {
		return false
	}

	ns, name := lease.ServiceName(svc)
	l, err := p.clientSet.CoordinationV1().Leases(ns).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		// No lease object: nobody owns the service, so a local binding can
		// only be a leftover. A leader that is re-creating its lease after a
		// deletion keeps its binding protected through leadsServiceLocally.
		return true
	}
	if err != nil {
		// API problems again: leave the state alone for the next pass.
		log.Debug("stale service state reconcile: lease lookup failed", "service", svc.Name, "err", err)
		return false
	}
	if l.Spec.HolderIdentity != nil && *l.Spec.HolderIdentity == p.config.NodeName {
		return false
	}
	return true
}

// leadsServiceLocally reports whether this process currently owns the service:
// either an instance was registered for it (we lead, or the release is still
// running) or the in-memory lease says we are elected.
func (p *Processor) leadsServiceLocally(svc *v1.Service) bool {
	p.mutex.Lock()
	found := instance.FindServiceInstance(svc, p.ServiceInstances) != nil
	p.mutex.Unlock()
	if found {
		return true
	}

	ns, name := lease.ServiceName(svc)
	l := p.leaseMgr.Get(lease.NewID(p.config.LeaderElectionType, ns, name))
	return l != nil && l.Elected.Load()
}

// dropStaleServiceState releases everything this node holds locally for a
// service whose lease belongs elsewhere.
func (p *Processor) dropStaleServiceState(svc *v1.Service, labels map[string]string) {
	leaseNS, leaseName := lease.ServiceName(svc)
	addresses, _ := instance.FetchServiceAddresses(svc)
	if p.releaseStaleLocalState(svc.Name, svc.Namespace, svc.UID, leaseNS, leaseName, addresses, labels) &&
		(len(addresses) > 0 || len(labels) > 0) {
		log.Info("stale service state reconcile: released local state",
			"service", svc.Namespace+"/"+svc.Name, "addresses", addresses)
	}
}

// releaseStaleLocalState deletes the given VIPs from this node's interface and
// removes the given node labels, unless the ownership re-check under p.mutex
// says otherwise. It reports whether anything was released.
//
// The re-check runs under p.mutex for the whole release: winning an election
// publishes Elected before addService can take p.mutex to bind the VIP and add
// the label, so holding the mutex across the check and the delete means a
// just-won election either binds after this cleanup (its bind survives) or is
// seen and skipped. The lease mutex cannot be used for this: a follower's
// parked election holds it for its entire lifetime.
//
// The label patch happens under the mutex too, which serializes it against
// addService's AddLabel; the API was just reachable for the lease lookup, so
// the 30-second labeler timeout can only bite in a narrow double-fault window,
// and blocking reconciliation for it is preferred over removing a label that a
// concurrent winner just added.
func (p *Processor) releaseStaleLocalState(name, namespace string, uid types.UID, leaseNS, leaseName string, addresses []string, labels map[string]string) (released bool) {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	for _, inst := range p.ServiceInstances {
		if inst == nil {
			continue
		}
		snapshot := inst.ServiceSnapshot
		if snapshot == nil {
			continue
		}
		if (uid != "" && snapshot.UID == uid) || (snapshot.Name == name && snapshot.Namespace == namespace) {
			// An instance is registered: this process leads the service or is
			// still releasing it; the release path owns the cleanup.
			return false
		}
	}
	if l := p.leaseMgr.Get(lease.NewID(p.config.LeaderElectionType, leaseNS, leaseName)); l != nil && l.Elected.Load() {
		return false
	}

	p.dropStaleAddresses(addresses)
	p.removeStaleLabels(labels)
	return true
}

func (p *Processor) dropStaleAddresses(addresses []string) {
	if !p.config.EnableARP || len(addresses) == 0 {
		return
	}
	gc := p.gcStaleAddress
	if gc == nil {
		gc = vip.GarbageCollect
	}
	for _, addr := range addresses {
		found, err := gc(p.serviceInterface(), addr, p.intfMgr)
		switch {
		case err != nil:
			log.Warn("stale service state reconcile: VIP check failed", "address", addr, "err", err)
		case found:
			log.Warn("stale service state reconcile: removed VIP bound without the lease",
				"address", addr, "interface", p.serviceInterface())
		}
	}
}

func (p *Processor) removeStaleLabels(labels map[string]string) {
	if len(labels) == 0 {
		return
	}
	if !p.config.EnableNodeLabeling {
		// Without labeling there is no writable labeler (noop), and kube-vip
		// never adds labels, so there is nothing to attribute to leases.
		log.Debug("stale service state reconcile: node labeling disabled, skipping label cleanup")
		return
	}
	if err := p.nodeLabelManager.RemoveLabel(labels); err != nil {
		log.Warn("stale service state reconcile: label removal failed, will retry next pass", "err", err)
		return
	}
	for key := range labels {
		log.Info("stale service state reconcile: removed stale node label", "label", key)
	}
}

// isStaleStateCandidate mirrors the watcher's filter chain for services this
// kube-vip deployment manages. Both watcher partitions (regular and
// forced-election) draw from this union, so the forcedOnly split is not
// needed for local cleanup decisions.
func (p *Processor) isStaleStateCandidate(svc *v1.Service) bool {
	if svc.Spec.Type != v1.ServiceTypeLoadBalancer {
		return false
	}
	if svc.Annotations[kubevip.LoadbalancerIgnore] == "true" {
		return false
	}
	if p.lbClassFilter == nil {
		return false
	}
	return !p.lbClassFilter(svc, p.config)
}

func serviceProvidedLabels(nodeLabels map[string]string) map[string]string {
	out := map[string]string{}
	prefix := kubevip.ServiceProvided + "/"
	for key, value := range nodeLabels {
		if strings.HasPrefix(key, prefix) {
			out[key] = value
		}
	}
	return out
}

func labelsForService(owned map[string]string, svc *v1.Service) map[string]string {
	key := fmt.Sprintf("%s/%s.%s", kubevip.ServiceProvided, svc.Name, svc.Namespace)
	if value, ok := owned[key]; ok {
		return map[string]string{key: value}
	}
	return nil
}

// splitServiceProvidedLabelKey parses "<prefix>/<name>.<namespace>"; neither
// Kubernetes object name may contain a dot, so the last dot is the separator.
func splitServiceProvidedLabelKey(key string) (name, namespace string, ok bool) {
	trimmed, found := strings.CutPrefix(key, kubevip.ServiceProvided+"/")
	if !found {
		return "", "", false
	}
	idx := strings.LastIndex(trimmed, ".")
	if idx <= 0 || idx == len(trimmed)-1 {
		return "", "", false
	}
	return trimmed[:idx], trimmed[idx+1:], true
}

// staleAddressesFromLabelValue recovers the VIPs recorded in a
// service-provided label value. IPv4 round-trips exactly; sanitized IPv6
// values do not parse back and are left to the lease-driven passes.
func staleAddressesFromLabelValue(value string) []string {
	var out []string
	for _, token := range strings.Split(value, ",") {
		token = strings.TrimSpace(token)
		if net.ParseIP(token) != nil {
			out = append(out, token)
		}
	}
	return out
}

// defaultServiceLeaseName mirrors lease.ServiceName for a Service object that
// no longer exists, where the kube-vip.io/leaseName annotation cannot be read.
func defaultServiceLeaseName(serviceName string) string {
	return fmt.Sprintf("kubevip-%s", serviceName)
}
