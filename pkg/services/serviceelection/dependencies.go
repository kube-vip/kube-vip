package serviceelection

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/kube-vip/kube-vip/pkg/election"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	v1 "k8s.io/api/core/v1"
)

// ServiceState validates the current Service context and readiness generation.
type ServiceState interface {
	IsCurrent(*v1.Service, *servicecontext.Context, servicecontext.ReadinessGeneration) bool
}

// Datapath activates and cleans up the network state of a Service.
type Datapath interface {
	Activate(context.Context, *v1.Service, *servicecontext.Context, *sync.WaitGroup) error
	Cleanup(context.Context, *v1.Service, *servicecontext.Context, func() bool) error
}

// CampaignRunner executes one Kubernetes leader-election campaign.
type CampaignRunner interface {
	RunCampaign(context.Context, *election.RunConfig) error
}

// RestartScheduler schedules a delayed campaign restart.
type RestartScheduler interface {
	ScheduleRestart(context.Context, time.Duration, *sync.WaitGroup, func())
}

// LeaseStore is the narrow lease ownership contract used by coordinators.
type LeaseStore interface {
	AcquireRegistrations(context.Context, lease.ID, []lease.RegistrationSpec) (*lease.Lease, map[string]*lease.Registration, error)
	// ClaimRegistration returns nil when no entry exists for the requested Lease.
	// Implementations must cancel a retired Lease before making its entry
	// unavailable, so a coordinator can let work using that generation drain.
	ClaimRegistration(lease.ID, lease.RegistrationSpec) (*lease.Registration, bool)
}

// Dependencies defines the collaborators required by Manager.
type Dependencies struct {
	Config          *kubevip.Config
	Leases          LeaseStore
	ElectionManager *election.Manager
	State           ServiceState
	Datapath        Datapath
	Runner          CampaignRunner
	Scheduler       RestartScheduler
}

func (d *Dependencies) validate() error {
	if d.Config == nil {
		return errors.New("config is required")
	}
	if d.Leases == nil {
		return errors.New("lease store is required")
	}
	if d.State == nil {
		return errors.New("service state is required")
	}
	if d.Datapath == nil {
		return errors.New("datapath is required")
	}
	if d.Runner == nil {
		return errors.New("campaign runner is required")
	}
	if d.Scheduler == nil {
		return errors.New("restart scheduler is required")
	}
	return nil
}
