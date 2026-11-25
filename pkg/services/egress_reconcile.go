package services

import (
	"context"
	"errors"
	"fmt"
	log "log/slog"
	"net"
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
	desired          *v1.Service
	readiness        servicecontext.ReadinessGeneration
	cancel           context.CancelFunc
	families         egressFamilies
	publishTable     bool
	annotateEndpoint string
}

func (p *Processor) usesInternalEgress(svc *v1.Service) bool {
	return svc.Annotations[kubevip.EgressInternal] != "" || p.config.EgressWithNftables
}

func (p *Processor) egressEndpointAnnotation(svc *v1.Service) string {
	if !p.config.EnableEndpoints && svc.Annotations[kubevip.EgressIPv6] == "true" {
		return kubevip.ActiveEndpointIPv6
	}
	return kubevip.ActiveEndpoint
}

// missingLocalEgressAnnotation identifies an elected Local Service whose
// datapath can use the ready endpoint selected by its own endpoint watcher.
func (p *Processor) missingLocalEgressAnnotation(svc *v1.Service) string {
	if !p.usesInternalEgress(svc) || !kubevip.UsesServiceElection(p.config, svc) ||
		svc.Spec.ExternalTrafficPolicy != v1.ServiceExternalTrafficPolicyLocal ||
		(p.config.EnableEndpoints && svc.Annotations[kubevip.EgressIPv6] == "true") {
		return ""
	}
	key := p.egressEndpointAnnotation(svc)
	if svc.Annotations[key] != "" {
		return ""
	}
	addresses, _ := instance.FetchServiceAddresses(svc)
	for _, address := range addresses {
		if utils.IsIPv6(address) == (key == kubevip.ActiveEndpointIPv6) {
			return key
		}
	}
	return ""
}

func serviceWithEgressEndpoint(svc *v1.Service, key, endpoint string) *v1.Service {
	copy := svc.DeepCopy()
	if copy.Annotations == nil {
		copy.Annotations = make(map[string]string)
	}
	copy.Annotations[key] = endpoint
	return copy
}

func localEgressEndpoint(svcCtx *servicecontext.Context, key string) (string, error) {
	endpoint, _ := svcCtx.EgressEndpoint()
	ip := net.ParseIP(endpoint)
	if ip == nil || (ip.To4() == nil) != (key == kubevip.ActiveEndpointIPv6) {
		return "", fmt.Errorf("no ready local endpoint for %s", key)
	}
	return endpoint, nil
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
	state := p.egressState(inst)
	var failed egressFamilies
	var configured bool
	var err error
	var annotateEndpoint string
	if state != nil && svcCtx.ReadinessGenerationCurrent(readiness) {
		endpoint, known := svcCtx.EgressEndpoint()
		if !known || endpoint != "" {
			annotateEndpoint = p.missingLocalEgressAnnotation(svc)
		}
	}
	if annotateEndpoint != "" {
		var endpoint string
		endpoint, err = localEgressEndpoint(svcCtx, annotateEndpoint)
		if err == nil {
			svc = serviceWithEgressEndpoint(svc, annotateEndpoint, endpoint)
		}
	}
	if err == nil {
		failed, configured, err = p.programServiceEgress(ctx, svc, egressAllFamilies)
	} else {
		failed = egressAllFamilies
	}
	publishTable := configured && p.usesInternalEgress(svc)
	if configured && annotateEndpoint != "" {
		inst.ServiceSnapshot = svc
	}
	if err == nil && publishTable {
		err = p.publishEgressAnnotations(ctx, svc, annotateEndpoint)
	}
	if err == nil {
		if update {
			inst.ServiceSnapshot = svc
		}
		return nil
	}

	if state == nil || !p.usesInternalEgress(svc) || ctx.Err() != nil || !svcCtx.ReadinessGenerationCurrent(readiness) {
		return err
	}
	if update {
		// The watcher compares its next endpoint to this desired snapshot.
		// The retry tracks what is still missing in the kernel.
		inst.ServiceSnapshot = svc
	}
	p.scheduleEgressRetry(svcCtx, state, &egressRetry{
		desired: svc, readiness: readiness, families: failed,
		publishTable: publishTable, annotateEndpoint: annotateEndpoint,
	}, err)
	return nil
}

// scheduleEgressRetry requires the Service lock. Its caller has established
// that this activation still owns the pending work.
func (p *Processor) scheduleEgressRetry(svcCtx *servicecontext.Context, state *serviceEgressState, retry *egressRetry, cause error) {
	svc := retry.desired
	retryCtx, cancel := context.WithCancel(state.ctx)
	retry.cancel = cancel
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
		err := cause
		for {
			next := min(delay.Step(), 30*time.Second)
			log.Warn("egress reconciliation incomplete; retrying", "service", svc.Name, "namespace", svc.Namespace,
				"error", err, "retry_in", next)
			timer := time.NewTimer(next)
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
			err = p.retryServiceEgress(retryCtx, svcCtx, state, retry)
			if err == nil {
				return
			}
		}
	})
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
	endpointKey := p.egressEndpointAnnotation(desired)
	if endpoint, known := svcCtx.EgressEndpoint(); known {
		original := desired.Annotations[endpointKey]
		if endpoint == "" || (retry.annotateEndpoint == "" || original != "") && endpoint != original {
			return nil
		}
	}

	svc, err := p.clientSet.CoreV1().Services(desired.Namespace).Get(ctx, desired.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read current Service: %w", err)
	}
	if retry.annotateEndpoint != "" {
		endpoint, err := localEgressEndpoint(svcCtx, endpointKey)
		if err != nil {
			return err
		}
		if fromAPI := svc.Annotations[endpointKey]; fromAPI != "" && fromAPI != endpoint {
			return nil
		}
		desired = serviceWithEgressEndpoint(desired, endpointKey, endpoint)
		svc = serviceWithEgressEndpoint(svc, endpointKey, endpoint)
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
	if configured && retry.annotateEndpoint != "" {
		state.instance.ServiceSnapshot = svc
		retry.desired = desired
	}
	if err != nil {
		return err
	}
	if retry.publishTable {
		if err := p.publishEgressAnnotations(ctx, svc, retry.annotateEndpoint); err != nil {
			return err
		}
	}
	log.Info("egress configuration recovered", "service", svc.Name, "namespace", svc.Namespace)
	return nil
}

func sameEgressWork(desired, current *v1.Service) bool {
	if desired.UID != current.UID || current.DeletionTimestamp != nil || desired.Spec.Type != current.Spec.Type ||
		desired.Spec.ExternalTrafficPolicy != current.Spec.ExternalTrafficPolicy ||
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
