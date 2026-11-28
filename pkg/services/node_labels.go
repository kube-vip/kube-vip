package services

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/node"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const nodeLabelReconcileInterval = 2 * time.Minute

// nodeLabelPatchTimeout bounds a stale label removal patch. Services claiming
// a label wait for that patch, so it must stay short.
const nodeLabelPatchTimeout = 5 * time.Second

// RunNodeLabelReconcile periodically removes stale service-provided labels from
// this node until ctx ends.
func (p *Processor) RunNodeLabelReconcile(ctx context.Context, nodeLabels node.LabelReader) {
	if p == nil || p.config == nil || !p.config.EnableNodeLabeling || nodeLabels == nil || p.clientSet == nil {
		return
	}
	ticker := time.NewTicker(nodeLabelReconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := p.RemoveStaleNodeLabels(ctx, nodeLabels); err != nil {
				log.Warn("[service] removing stale node labels", "err", err)
			}
		}
	}
}

// RemoveStaleNodeLabels removes the service-provided labels from this node whose
// Service this process does not provide, preserving labels handled by another
// kube-vip deployment on the same node.
func (p *Processor) RemoveStaleNodeLabels(ctx context.Context, nodeLabels node.LabelReader) error {
	if p == nil || p.config == nil || !p.config.EnableNodeLabeling || nodeLabels == nil || p.clientSet == nil {
		return nil
	}
	prefix := kubevip.ServiceProvided + "/"
	labels, err := nodeLabels.Labels(ctx, prefix)
	if err != nil {
		return err
	}
	p.dropClaimedNodeLabels(labels)
	stale := map[string]string{}
	var errs []error
	for key, value := range labels {
		name, namespace, ok := strings.Cut(strings.TrimPrefix(key, prefix), ".")
		if !ok || name == "" || namespace == "" {
			continue
		}
		remove, err := p.isStaleServiceLabel(ctx, namespace, name)
		if err != nil {
			errs = append(errs, fmt.Errorf("label %q: %w", key, err))
			continue
		}
		if remove {
			stale[key] = value
		}
	}
	if err := p.removeUnclaimedNodeLabels(ctx, nodeLabels, stale); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// claimNodeLabels records labels as provided by the Service instance uid.
// The caller must hold the Service lock for uid.
func (p *Processor) claimNodeLabels(uid types.UID, labels map[string]string) {
	p.nodeLabelMu.Lock()
	defer p.nodeLabelMu.Unlock()
	if p.nodeLabelClaims == nil {
		p.nodeLabelClaims = make(map[types.UID]map[string]string)
	}
	p.nodeLabelClaims[uid] = labels
}

// releaseNodeLabels drops the labels claimed by the Service instance uid. The
// caller must hold the Service lock for uid.
func (p *Processor) releaseNodeLabels(uid types.UID) {
	p.nodeLabelMu.Lock()
	defer p.nodeLabelMu.Unlock()
	delete(p.nodeLabelClaims, uid)
}

func (p *Processor) dropClaimedNodeLabels(labels map[string]string) {
	p.nodeLabelMu.Lock()
	defer p.nodeLabelMu.Unlock()
	p.dropClaimedNodeLabelsLocked(labels)
}

func (p *Processor) dropClaimedNodeLabelsLocked(labels map[string]string) {
	for _, claimed := range p.nodeLabelClaims {
		for key := range claimed {
			delete(labels, key)
		}
	}
}

// removeUnclaimedNodeLabels removes the labels no Service of this process
// claims. Claims are checked again while holding nodeLabelMu across the
// patch, so a Service claiming its label meanwhile either keeps it or adds it
// again once the patch is done.
func (p *Processor) removeUnclaimedNodeLabels(ctx context.Context, nodeLabels node.LabelReader, labels map[string]string) error {
	if len(labels) == 0 {
		return nil
	}
	p.nodeLabelMu.Lock()
	defer p.nodeLabelMu.Unlock()
	p.dropClaimedNodeLabelsLocked(labels)
	if len(labels) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, nodeLabelPatchTimeout)
	defer cancel()
	return nodeLabels.RemoveLabelWithContext(ctx, labels)
}

func (p *Processor) isStaleServiceLabel(ctx context.Context, namespace, name string) (bool, error) {
	if p.config.ServiceNamespace != v1.NamespaceAll && namespace != p.config.ServiceNamespace {
		return false, nil
	}
	svc, err := p.clientSet.CoreV1().Services(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	// Claimed labels were dropped before, so the label is stale unless it
	// belongs to another kube-vip deployment.
	foreign := svc.Spec.Type == v1.ServiceTypeLoadBalancer && p.lbClassFilter != nil && p.lbClassFilter(svc, p.config)
	return !foreign, nil
}

// RecoverNodeLabels removes labels that an earlier kube-vip process left on
// this node, e.g. after it was killed. It must run before this process adds
// labels. As in RecoverAddresses, a label is kept only while this node owns it
// according to the Lease holders, so labels of Services led by other nodes or
// of deleted Services do not survive a restart.
func (p *Processor) RecoverNodeLabels(ctx context.Context, nodeLabels node.LabelReader) error {
	if p == nil || p.config == nil || !p.config.EnableNodeLabeling || nodeLabels == nil || p.clientSet == nil {
		return nil
	}
	labels, err := nodeLabels.Labels(ctx, "")
	if err != nil {
		return err
	}
	prefix := kubevip.ServiceProvided + "/"
	holders := make(map[string]string)
	stale := map[string]string{}
	var errs []error
	for key, value := range labels {
		var retain bool
		var err error
		switch {
		case p.config.EnableServices && strings.HasPrefix(key, prefix):
			retain, err = p.serviceLabelRetained(ctx, strings.TrimPrefix(key, prefix), holders)
		case p.config.EnableControlPlane && key == kubevip.HasIP:
			retain, err = p.controlPlaneLabelRetained(ctx, holders)
		default:
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("label %q: %w", key, err))
			continue
		}
		if !retain {
			stale[key] = value
		}
	}
	if len(stale) > 0 {
		log.Info("removing node labels left by a previous kube-vip process", "labels", stale)
		if err := p.removeUnclaimedNodeLabels(ctx, nodeLabels, stale); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// serviceLabelRetained reports whether the label of the Service named by
// "<name>.<namespace>" still belongs to this node. Labels of Services outside
// this instance's scope belong to another kube-vip deployment and are kept.
func (p *Processor) serviceLabelRetained(ctx context.Context, serviceKey string, holders map[string]string) (bool, error) {
	name, namespace, ok := strings.Cut(serviceKey, ".")
	if !ok || name == "" || namespace == "" {
		return true, nil
	}
	if p.config.ServiceNamespace != v1.NamespaceAll && namespace != p.config.ServiceNamespace {
		return true, nil
	}
	svc, err := p.clientSet.CoreV1().Services(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	classFilter := p.lbClassFilter
	if classFilter == nil {
		classFilter = lbClassFilter
	}
	if svc.Spec.Type == v1.ServiceTypeLoadBalancer && classFilter(svc, p.config) {
		return true, nil
	}
	if !p.serviceOwnsRecoverableVIP(svc) {
		return false, nil
	}
	return p.serviceAddressRetained(ctx, svc, holders)
}

// controlPlaneLabelRetained reports whether this node holds the control-plane
// Lease. Only a leader-elected control plane adds the label.
func (p *Processor) controlPlaneLabelRetained(ctx context.Context, holders map[string]string) (bool, error) {
	if p.config.LeaderElectionType == "etcd" {
		return true, nil
	}
	if !p.config.EnableLeaderElection {
		return false, nil
	}
	namespace, name := lease.NamespaceName(p.config.LeaseName, p.config)
	return p.isLocalLeaseHolder(ctx, lease.NewID(p.config.LeaderElectionType, namespace, name), holders)
}
