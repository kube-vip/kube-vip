package services

import (
	"context"
	"errors"
	"fmt"
	log "log/slog"
	"slices"
	"sync"
	"time"

	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	"github.com/kube-vip/kube-vip/pkg/utils"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/ptr"
)

type egressFamilies uint8

const (
	egressIPv4 egressFamilies = 1 << iota
	egressIPv6
	egressAllFamilies = egressIPv4 | egressIPv6
)

// serviceEgressState is owned by one activation. Its retry is accessed only
// while holding the Service lock.
type serviceEgressState struct {
	instance *instance.Instance
	ctx      context.Context
	wg       *sync.WaitGroup
	retry    *egressRetry
}

type egressRetry struct {
	desired      *v1.Service
	readiness    servicecontext.ReadinessGeneration
	cancel       context.CancelFunc
	families     egressFamilies
	publishTable bool
}

func (p *Processor) usesInternalEgress(svc *v1.Service) bool {
	return svc.Annotations[kubevip.EgressInternal] != "" || p.config.EgressWithNftables
}

// programServiceEgress rebuilds only the requested families. A failed nftables
// family must be rebuilt as a group because its VIPs share a chain.
func (p *Processor) programServiceEgress(ctx context.Context, svc *v1.Service, families egressFamilies) (egressFamilies, bool, error) {
	if families == 0 {
		return 0, false, nil
	}
	serviceIPs, _ := instance.FetchServiceAddresses(svc)
	ipv4Endpoint := svc.Annotations[kubevip.ActiveEndpoint]
	ipv6Endpoint := svc.Annotations[kubevip.ActiveEndpointIPv6]
	ipv6Only := svc.Annotations[kubevip.EgressIPv6] == "true"
	if p.config.EnableEndpoints {
		ipv6Endpoint = ipv4Endpoint
	}
	if ipv6Only && p.config.EnableEndpoints || !ipv6Only && ipv4Endpoint == "" {
		return 0, false, nil
	}

	var failed egressFamilies
	var configured, preparedIPv4, preparedIPv6 bool
	var errs []error
	useNftables := p.usesInternalEgress(svc)
	for _, serviceIP := range serviceIPs {
		ipv6 := utils.IsIPv6(serviceIP)
		family, endpoint, prepared := egressIPv4, ipv4Endpoint, &preparedIPv4
		if ipv6 {
			family, endpoint, prepared = egressIPv6, ipv6Endpoint, &preparedIPv6
		}
		if families&family == 0 || ipv6Only && !ipv6 || endpoint == "" {
			continue
		}
		if err := ctx.Err(); err != nil {
			return failed, configured, err
		}
		if useNftables && !*prepared {
			*prepared = true
			if err := p.prepareEgressNftablesTable(string(svc.UID), ipv6); err != nil {
				failed |= family
				families &^= family
				errs = append(errs, err)
				continue
			}
		}
		var applied bool
		if err := p.configureEgress(ctx, serviceIP, endpoint, svc.Namespace, string(svc.UID), svc.Annotations, &applied); err != nil {
			failed |= family
			errs = append(errs, fmt.Errorf("program SNAT for %s: %w", serviceIP, err))
		}
		configured = configured || applied
	}
	return failed, configured, errors.Join(errs...)
}

func (p *Processor) egressState(inst *instance.Instance) *serviceEgressState {
	value, ok := p.serviceEgress.Load(inst.UID())
	if !ok {
		return nil
	}
	state := value.(*serviceEgressState)
	if state.instance != inst {
		return nil
	}
	return state
}

func (s *serviceEgressState) cancelRetry() {
	if s.retry != nil {
		s.retry.cancel()
		s.retry = nil
	}
}

// reconcileServiceEgress is called with the Service lock held. Native nftables
// failures leave the Service active and queue only the unfinished work.
func (p *Processor) reconcileServiceEgress(ctx context.Context, svcCtx *servicecontext.Context,
	inst *instance.Instance, svc *v1.Service, update bool) error {
	readiness := svcCtx.CurrentReadiness()
	failed, configured, err := p.programServiceEgress(ctx, svc, egressAllFamilies)
	publishTable := configured && p.usesInternalEgress(svc)
	if err == nil && publishTable {
		err = p.updateEgressNftablesTableAnnotation(ctx, svc)
		publishTable = err != nil
	}
	if err == nil {
		if update {
			inst.ServiceSnapshot = svc
		}
		return nil
	}

	state := p.egressState(inst)
	if state == nil || !p.usesInternalEgress(svc) || ctx.Err() != nil || !svcCtx.ReadinessGenerationCurrent(readiness) {
		return err
	}
	if update {
		// The watcher compares its next endpoint to this desired snapshot.
		// The retry tracks what is still missing in the kernel.
		inst.ServiceSnapshot = svc
	}
	log.Warn("scheduling egress configuration retry", "service", svc.Name, "namespace", svc.Namespace, "error", err)
	retryCtx, cancel := context.WithCancel(state.ctx)
	retry := &egressRetry{
		desired: svc, readiness: readiness, cancel: cancel,
		families: failed, publishTable: publishTable,
	}
	state.retry = retry
	state.wg.Go(func() {
		defer cancel()
		defer func() {
			p.serviceLock.Lock(svc.UID)
			if state.retry == retry {
				state.retry = nil
			}
			if err := p.serviceLock.Unlock(svc.UID); err != nil {
				log.Error("failed to release service lock", "uid", svc.UID, "err", err)
			}
		}()
		delay := wait.Backoff{Duration: time.Second, Factor: 2, Jitter: 0.1, Cap: 30 * time.Second, Steps: 6}
		for {
			timer := time.NewTimer(min(delay.Step(), 30*time.Second))
			select {
			case <-retryCtx.Done():
				timer.Stop()
				return
			case <-svcCtx.Ctx.Done():
				timer.Stop()
				return
			case <-retry.readiness.Lost():
				timer.Stop()
				return
			case <-timer.C:
			}
			err := p.retryServiceEgress(retryCtx, svcCtx, state, retry)
			if err == nil {
				return
			}
			log.Warn("egress configuration retry failed", "service", svc.Name, "namespace", svc.Namespace, "error", err)
		}
	})
	return nil
}

func (p *Processor) retryServiceEgress(ctx context.Context, svcCtx *servicecontext.Context,
	state *serviceEgressState, retry *egressRetry) error {
	desired := retry.desired
	p.serviceLock.Lock(desired.UID)
	defer func() {
		if err := p.serviceLock.Unlock(desired.UID); err != nil {
			log.Error("failed to release service lock", "uid", desired.UID, "err", err)
		}
	}()

	current, err := p.serviceContextCurrentLocked(desired.UID, svcCtx)
	if err != nil || !current || ctx.Err() != nil || state.retry != retry ||
		p.findServiceInstance(desired) != state.instance || !svcCtx.ReadinessGenerationCurrent(retry.readiness) {
		return nil
	}
	endpointKey := kubevip.ActiveEndpoint
	if !p.config.EnableEndpoints && desired.Annotations[kubevip.EgressIPv6] == "true" {
		endpointKey = kubevip.ActiveEndpointIPv6
	}
	if endpoint, known := svcCtx.EgressEndpoint(); known && endpoint != desired.Annotations[endpointKey] {
		return nil
	}

	svc, err := p.clientSet.CoreV1().Services(desired.Namespace).Get(ctx, desired.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read current Service: %w", err)
	}
	if !sameEgressWork(desired, svc) {
		return nil
	}
	reservation, ready := svcCtx.AcquireReadinessGeneration(retry.readiness)
	if !ready {
		return nil
	}
	defer reservation.Release()

	failed, configured, err := p.programServiceEgress(ctx, svc, retry.families)
	retry.families = failed
	retry.publishTable = retry.publishTable || configured
	if err != nil {
		return err
	}
	if retry.publishTable {
		if err := p.updateEgressNftablesTableAnnotation(ctx, svc); err != nil {
			return err
		}
	}
	log.Info("egress configuration recovered", "service", svc.Name, "namespace", svc.Namespace)
	return nil
}

func sameEgressWork(desired, current *v1.Service) bool {
	if desired.UID != current.UID || current.DeletionTimestamp != nil || desired.Spec.Type != current.Spec.Type ||
		!ptr.Equal(desired.Spec.LoadBalancerClass, current.Spec.LoadBalancerClass) {
		return false
	}
	for _, key := range []string{
		kubevip.Egress, kubevip.EgressIPv6, kubevip.EgressInternal,
		kubevip.ActiveEndpoint, kubevip.ActiveEndpointIPv6,
		kubevip.EgressDestinationPorts, kubevip.EgressAllowedNetworks, kubevip.EgressDeniedNetworks,
		kubevip.EgressNoInternalTraffic, kubevip.EgressDetectAPIServer,
		kubevip.LoadbalancerIgnore, kubevip.ServiceLease, kubevip.ForcePerServiceElection,
	} {
		if desired.Annotations[key] != current.Annotations[key] {
			return false
		}
	}
	desiredIPs, _ := instance.FetchServiceAddresses(desired)
	currentIPs, _ := instance.FetchServiceAddresses(current)
	return slices.Equal(desiredIPs, currentIPs)
}
