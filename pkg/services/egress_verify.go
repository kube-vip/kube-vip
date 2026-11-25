package services

import (
	"context"
	"errors"
	"fmt"
	log "log/slog"
	"time"

	"github.com/kube-vip/kube-vip/pkg/nftables"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	"github.com/kube-vip/kube-vip/pkg/utils"
	"k8s.io/apimachinery/pkg/util/wait"
)

const defaultEgressVerificationInterval = time.Minute

func egressFamily(address string) egressFamilies {
	if utils.IsIPv6(address) {
		return egressIPv6
	}
	return egressIPv4
}

// startEgressVerification is called under the Service lock. Each verification
// loop belongs to one activation and stops on withdrawal or readiness loss.
func (p *Processor) startEgressVerification(state *serviceEgressState, svcCtx *servicecontext.Context) {
	ctx, cancel := context.WithCancel(state.ctx)
	state.cancelVerification = cancel
	readiness := svcCtx.CurrentReadiness()
	interval := p.egressVerificationInterval
	state.wg.Go(func() {
		defer cancel()
		timer := time.NewTimer(wait.Jitter(interval, .1))
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-svcCtx.Ctx.Done():
				return
			case <-readiness.Lost():
				return
			case <-timer.C:
				p.verifyServiceEgress(ctx, state, svcCtx, readiness)
				timer.Reset(wait.Jitter(interval, .1))
			}
		}
	})
}

func (p *Processor) verifyServiceEgress(ctx context.Context, state *serviceEgressState,
	svcCtx *servicecontext.Context, readiness servicecontext.ReadinessGeneration) {
	uid := state.instance.UID()
	p.serviceLock.Lock(uid)
	defer func() {
		if err := p.serviceLock.Unlock(uid); err != nil {
			log.Error("failed to release service lock", "uid", uid, "err", err)
		}
	}()
	current, err := p.serviceContextCurrentLocked(uid, svcCtx)
	if err != nil || !current || ctx.Err() != nil || p.egressState(state.instance) != state ||
		state.retry != nil || len(state.rules) == 0 {
		return
	}
	ready, ok := svcCtx.AcquireReadinessGeneration(readiness)
	if !ok {
		return
	}
	defer ready.Release()

	svc := state.desired
	if endpoint, known := svcCtx.EgressEndpoint(); known && endpoint != svc.Annotations[p.egressEndpointAnnotation(svc)] {
		return
	}
	table := nftables.EgressTableBaseNameForInstance(p.config.InstanceName)
	var missing egressFamilies
	var failures []error
	for _, family := range []egressFamilies{egressIPv4, egressIPv6} {
		expected := make(map[string]int)
		for vip, count := range state.rules {
			if egressFamily(vip) == family {
				expected[vip] = count
			}
		}
		if len(expected) == 0 {
			continue
		}
		ipv6 := family == egressIPv6
		intact, err := nftables.HasSNATRules(ipv6, string(uid), table, expected)
		if err != nil {
			log.Warn("unable to verify egress SNAT rules", "service", svc.Name, "namespace", svc.Namespace,
				"ipv6", ipv6, "error", err)
			continue
		}
		if !intact {
			missing |= family
			failures = append(failures, fmt.Errorf("nftables SNAT rules missing (IPv6: %t)", ipv6))
		}
	}
	if missing != 0 && ctx.Err() == nil {
		p.scheduleEgressRetry(svcCtx, state, &egressRetry{desired: svc, readiness: readiness, families: missing},
			errors.Join(failures...))
	}
}
