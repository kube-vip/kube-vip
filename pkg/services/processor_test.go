package services

import (
	"context"
	"sync"
	"testing"

	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	"github.com/kube-vip/kube-vip/pkg/services/serviceelection"
	v1 "k8s.io/api/core/v1"
)

// newTestServiceLocks supplies the invariant normally established by
// NewServicesProcessor to tests that need a partially configured Processor.
func newTestServiceLocks() *ServiceLock {
	return NewServiceLock()
}

func publishTestServiceContext(processor *Processor, service *v1.Service) *servicecontext.Context {
	svcCtx := servicecontext.New(context.Background())
	processor.svcMap.Store(service.UID, svcCtx)
	return svcCtx
}

type serviceInstanceFactoryFunc func(context.Context, *v1.Service, *sync.WaitGroup) (*instance.Instance, error)

func (f serviceInstanceFactoryFunc) Create(ctx context.Context, service *v1.Service,
	wg *sync.WaitGroup) (*instance.Instance, error) {
	return f(ctx, service, wg)
}

type testElectionDatapath struct {
	production *electionAdapter
	activate   func(context.Context, *v1.Service, *servicecontext.Context, *sync.WaitGroup) error
}

type testElectionCoordinatorConfig struct {
	activate  func(context.Context, *v1.Service, *servicecontext.Context, *sync.WaitGroup) error
	runner    serviceelection.CampaignRunner
	scheduler serviceelection.RestartScheduler
}

type testElectionCoordinatorOption func(*testElectionCoordinatorConfig)

func withTestElectionActivation(
	activate func(context.Context, *v1.Service, *servicecontext.Context, *sync.WaitGroup) error,
) testElectionCoordinatorOption {
	return func(config *testElectionCoordinatorConfig) {
		config.activate = activate
	}
}

func withTestCampaignRunner(runner serviceelection.CampaignRunner) testElectionCoordinatorOption {
	return func(config *testElectionCoordinatorConfig) {
		config.runner = runner
	}
}

func withTestRestartScheduler(scheduler serviceelection.RestartScheduler) testElectionCoordinatorOption {
	return func(config *testElectionCoordinatorConfig) {
		config.scheduler = scheduler
	}
}

func (d *testElectionDatapath) Activate(ctx context.Context, service *v1.Service,
	svcCtx *servicecontext.Context, wg *sync.WaitGroup) error {
	if d.activate == nil {
		return nil
	}
	return d.activate(ctx, service, svcCtx, wg)
}

func (d *testElectionDatapath) Cleanup(ctx context.Context, service *v1.Service,
	svcCtx *servicecontext.Context, current func() bool) error {
	return d.production.Cleanup(ctx, service, svcCtx, current)
}

// initializeTestElectionCoordinators supplies the invariant normally
// established by NewServicesProcessor after a partial test Processor is built.
// Tests replace only datapath activation; cleanup retains production behavior.
func initializeTestElectionCoordinators(processor *Processor,
	options ...testElectionCoordinatorOption) {
	if processor.config == nil {
		processor.config = &kubevip.Config{}
	}
	if processor.leaseMgr == nil {
		processor.leaseMgr = lease.NewManager()
	}
	if processor.instanceFactory == nil {
		processor.instanceFactory = instance.NewFactory(processor.config, processor.intfMgr,
			processor.arpMgr, processor.routeMgr, processor.nodeLabelManager)
	}
	adapter := &electionAdapter{processor: processor}
	config := testElectionCoordinatorConfig{runner: adapter, scheduler: adapter}
	for _, option := range options {
		option(&config)
	}
	datapath := &testElectionDatapath{production: adapter, activate: config.activate}
	manager, err := serviceelection.NewManager(&serviceelection.Dependencies{
		Config: processor.config, Leases: processor.leaseMgr, ElectionManager: processor.electionMgr,
		State: adapter, Datapath: datapath, Runner: config.runner, Scheduler: config.scheduler,
	})
	if err != nil {
		panic(err)
	}
	processor.electionCoordinators = manager
}

func TestNewServicesProcessorInitializesElectionCoordinators(t *testing.T) {
	processor, err := NewServicesProcessor(&kubevip.Config{}, nil, nil, nil, nil, nil, nil, nil, lease.NewManager(), nil)
	if err != nil {
		t.Fatalf("NewServicesProcessor() error = %v", err)
	}
	if processor.electionCoordinators == nil {
		t.Fatal("NewServicesProcessor() did not initialize election coordinators")
	}
}

func TestNewServicesProcessorRejectsNilLeaseManager(t *testing.T) {
	processor, err := NewServicesProcessor(&kubevip.Config{}, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	if err == nil {
		t.Fatal("NewServicesProcessor() accepted a nil lease manager")
	}
	if processor != nil {
		t.Fatal("NewServicesProcessor() returned a processor after rejecting its lease manager")
	}
}
