package services

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/node"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const nodeLabelReconcileInterval = 2 * time.Minute

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
	if len(stale) > 0 {
		for _, inst := range p.serviceInstances() {
			if err := p.deleteActiveInstanceLabels(inst, stale); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if len(stale) > 0 {
		if err := nodeLabels.RemoveLabelWithContext(ctx, stale); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (p *Processor) deleteActiveInstanceLabels(inst *instance.Instance, stale map[string]string) (retErr error) {
	if inst == nil {
		return nil
	}
	uid := inst.UID()
	p.serviceLock.Lock(uid)
	defer func() {
		if err := p.serviceLock.Unlock(uid); err != nil {
			log.Error("failed to release service lock", "uid", uid, "err", err)
			retErr = errors.Join(retErr, err)
		}
	}()
	if inst.AddCalled && inst.ServiceSnapshot != nil {
		for key := range generateLabelsFromService(inst.ServiceSnapshot) {
			delete(stale, key)
		}
	}
	return nil
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
	if svc.Spec.Type == v1.ServiceTypeLoadBalancer && p.lbClassFilter != nil && p.lbClassFilter(svc, p.config) {
		return false, nil
	}
	provided, err := p.providesService(svc)
	if err != nil {
		return false, err
	}
	return !provided, nil
}

func (p *Processor) providesService(svc *v1.Service) (provided bool, retErr error) {
	p.serviceLock.Lock(svc.UID)
	defer func() {
		if err := p.serviceLock.Unlock(svc.UID); err != nil {
			log.Error("failed to release service lock", "uid", svc.UID, "err", err)
			retErr = errors.Join(retErr, err)
		}
	}()
	inst := p.findServiceInstance(svc)
	return inst != nil && inst.AddCalled, nil
}
