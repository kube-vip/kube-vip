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
// The first pass enumerates candidate services - this also finds stale
// bindings in deployments without node labeling, whose stranded bindings have
// no label to point at them - and runs asynchronously so hundreds of lease
// lookups cannot delay the watcher. Binding/release of services is serialized
// against this pass through Processor.mutex (see releaseStaleLocalState).
// The label-driven passes then repeat on a ticker: a best-effort label removal
// that failed during the API outage heals itself once the API returns, without
// waiting for a pod restart.
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
		go func() {
			if err := p.reconcileStaleServiceState(ctx, true); err != nil {
				log.Warn("stale service state reconcile", "err", err)
			}

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

// leaseHeldByOtherNode reports whether the service's lease is held by a
// different node (or absent while this node provides nothing for it). A service
// this process actively leads is never stale locally: that's checked via
// providedLocally, not via mere instance registration - the watcher registers
// an instance on every node, leader or not.
func (p *Processor) leaseHeldByOtherNode(ctx context.Context, svc *v1.Service) bool {
	if p.providedLocally(svc) {
		return false
	}

	ns, name := lease.ServiceName(svc)
	l, err := p.clientSet.CoordinationV1().Leases(ns).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		// No lease object: nobody owns the service. A follower's leftover
		// binding can be released; a leader mid-re-creation is protected by
		// providedLocally (its lease reports Elected while the election holds).
		return true
	}
	if err != nil {
		// API problems again: leave the state alone for the next pass.
		log.Debug("stale service state reconcile: lease lookup failed", "service", svc.Name, "err", err)
		return false
	}
	return l.Spec.HolderIdentity == nil || *l.Spec.HolderIdentity != p.config.NodeName
}

// providedLocally reports whether this process is currently serving the
// service as leader: it has registered an instance that was actually added
// (AddCalled) or labelled (LabelAdded), or its in-memory lease reports elected
// but the instance list has not caught up yet. A bare instance for a follower
// (registered by the watcher, never added) is deliberately NOT treated as
// provided, otherwise the reconcile would skip every service the watcher has
// ever seen and never heal anything.
func (p *Processor) providedLocally(svc *v1.Service) bool {
	ns, name := lease.ServiceName(svc)
	if l := p.leaseMgr.Get(lease.NewID(p.config.LeaderElectionType, ns, name)); l != nil && l.Elected.Load() {
		return true
	}

	p.mutex.Lock()
	defer p.mutex.Unlock()
	for _, inst := range p.ServiceInstances {
		if inst == nil || inst.ServiceSnapshot == nil {
			continue
		}
		snapshot := inst.ServiceSnapshot
		if (snapshot.UID != "" && snapshot.UID == svc.UID) ||
			(snapshot.Name == svc.Name && snapshot.Namespace == svc.Namespace) {
			if inst.AddCalled || inst.LabelAdded {
				return true
			}
		}
	}
	return false
}

// dropStaleServiceState releases everything this node holds locally for a
// service whose lease belongs elsewhere.
func (p *Processor) dropStaleServiceState(svc *v1.Service, labels map[string]string) {
	leaseNS, leaseName := lease.ServiceName(svc)
	addresses, _ := instance.FetchServiceAddresses(svc)
	if p.releaseStaleLocalState(svc.Name, svc.Namespace, svc.UID, leaseNS, leaseName, addresses, labels) {
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
// An address that another locally-provided service still uses is never removed
// (shared-VIP guard): doing so would black-hole a live service whose VIP merely
// happens to collide with the stale one's.
func (p *Processor) releaseStaleLocalState(name, namespace string, uid types.UID, leaseNS, leaseName string, addresses []string, labels map[string]string) (released bool) {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	// Re-check actual local ownership (added/labelled/elected) before releasing.
	if p.providedLocallyLocked(name, namespace, uid) {
		return false
	}
	if l := p.leaseMgr.Get(lease.NewID(p.config.LeaderElectionType, leaseNS, leaseName)); l != nil && l.Elected.Load() {
		return false
	}

	for _, addr := range addresses {
		if p.addressProvidedByOtherLocked(addr, uid) {
			log.Debug("stale service state reconcile: keeping shared VIP used by another local service",
				"address", addr, "service", namespace+"/"+name)
			continue
		}
		if p.dropStaleAddressLocked(addr) {
			released = true
		}
	}
	if p.removeStaleLabels(labels) {
		released = true
	}
	return released
}

// providedLocallyLocked reports an instance that was actually added or
// labelled for the service. Callers must hold p.mutex.
func (p *Processor) providedLocallyLocked(name, namespace string, uid types.UID) bool {
	for _, inst := range p.ServiceInstances {
		if inst == nil || inst.ServiceSnapshot == nil {
			continue
		}
		snapshot := inst.ServiceSnapshot
		if (uid != "" && snapshot.UID == uid) || (snapshot.Name == name && snapshot.Namespace == namespace) {
			if inst.AddCalled || inst.LabelAdded {
				return true
			}
		}
	}
	return false
}

// addressProvidedByOtherLocked reports whether a locally-provided service
// (added or labelled) other than uid still advertises the address. Callers
// must hold p.mutex.
func (p *Processor) addressProvidedByOtherLocked(address string, uid types.UID) bool {
	for _, inst := range p.ServiceInstances {
		if inst == nil || inst.ServiceSnapshot == nil {
			continue
		}
		if !inst.AddCalled && !inst.LabelAdded {
			continue
		}
		if uid != "" && inst.ServiceSnapshot.UID == uid {
			continue
		}
		others, _ := instance.FetchServiceAddresses(inst.ServiceSnapshot)
		for _, a := range others {
			if a == address {
				return true
			}
		}
	}
	return false
}

// dropStaleAddressLocked removes the address from the service interface if it
// is bound; reports whether anything was actually deleted. Callers hold p.mutex.
func (p *Processor) dropStaleAddressLocked(address string) (deleted bool) {
	if !p.config.EnableARP {
		return false
	}
	gc := p.gcStaleAddress
	if gc == nil {
		gc = vip.GarbageCollect
	}
	found, err := gc(p.serviceInterface(), address, p.intfMgr)
	switch {
	case err != nil:
		log.Warn("stale service state reconcile: VIP check failed", "address", address, "err", err)
	case found:
		log.Warn("stale service state reconcile: removed VIP bound without the lease",
			"address", address, "interface", p.serviceInterface())
		return true
	}
	return false
}

// removeStaleLabels removes the given node labels, best-effort. Reports
// whether the patch succeeded; a failure is retried by the next pass.
func (p *Processor) removeStaleLabels(labels map[string]string) (removed bool) {
	if len(labels) == 0 {
		return false
	}
	if !p.config.EnableNodeLabeling {
		// Without labeling there is no writable labeler (noop), and kube-vip
		// never adds labels, so there is nothing to attribute to leases.
		log.Debug("stale service state reconcile: node labeling disabled, skipping label cleanup")
		return
	}
	if err := p.nodeLabelManager.RemoveLabel(labels); err != nil {
		log.Warn("stale service state reconcile: label removal failed, will retry next pass", "err", err)
		return false
	}
	for key := range labels {
		log.Info("stale service state reconcile: removed stale node label", "label", key)
	}
	return true
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
