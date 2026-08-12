package loadbalancer

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"reflect"
	"testing"

	"github.com/cloudflare/ipvs"
	"github.com/kube-vip/kube-vip/pkg/backend"
	"github.com/kube-vip/kube-vip/pkg/utils"
	"github.com/vishvananda/netlink"
)

func Test_ipAndFamily(t *testing.T) {
	type args struct {
		address string
	}
	tests := []struct {
		name    string
		args    args
		want    netip.Addr
		want1   ipvs.AddressFamily
		wantErr bool
	}{
		{
			name: utils.IPv4Family,
			args: args{
				address: "192.168.0.20",
			},
			want:  netip.AddrFrom4([4]byte{192, 168, 0, 20}),
			want1: ipvs.INET,
		},
		{
			name: utils.IPv6Family,
			args: args{
				address: "ff02::3",
			},
			want:  netip.AddrFrom16([16]byte{0: 0xff, 1: 0x02, 15: 0x03}),
			want1: ipvs.INET6,
		},
		{
			name: "invalid",
			args: args{
				address: "not-an-ip",
			},
			wantErr: true,
		},
		{
			name: "empty",
			args: args{
				address: "",
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, got1, err := ipAndFamily(tt.args.address)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ipAndFamily() error = %v, wantErr %t", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ipAndFamily() got = %v, want %v", got, tt.want)
			}
			if !reflect.DeepEqual(got1, tt.want1) {
				t.Errorf("ipAndFamily() got1 = %v, want %v", got1, tt.want1)
			}
		})
	}
}

func Test_ipAndFamilyFromBackendAddress(t *testing.T) {
	tests := []struct {
		name    string
		address string
	}{
		{name: "bare IPv6", address: "fd00::10"},
		{name: "bracketed IPv6 with port", address: "[fd00::10]:6443"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := backend.New(&backend.Config{
				Type:    backend.Discovery,
				Address: tt.address,
				Port:    6443,
			})
			if err != nil {
				t.Fatalf("creating backend: %v", err)
			}
			got, family, err := ipAndFamily(b.Address())
			if err != nil {
				t.Errorf("ipAndFamily() error = %v", err)
				return
			}
			if family != ipvs.INET6 {
				t.Errorf("ipAndFamily() family = %v, want %v", family, ipvs.INET6)
			}
			if got.String() != "fd00::10" {
				t.Errorf("ipAndFamily() address = %v, want fd00::10", got)
			}
		})
	}
}

type fakeIPVSClient struct {
	ipvs.Client
	removed                 []ipvs.Destination
	createDestinationCalls  int
	removeDestinationCalls  int
	createDestinationErrors []error
	removeDestinationErrors []error
}

func (f *fakeIPVSClient) RemoveDestination(_ ipvs.Service, d ipvs.Destination) error {
	f.removeDestinationCalls++
	f.removed = append(f.removed, d)
	if len(f.removeDestinationErrors) > 0 {
		err := f.removeDestinationErrors[0]
		f.removeDestinationErrors = f.removeDestinationErrors[1:]
		return err
	}
	return nil
}

func (f *fakeIPVSClient) Destinations(ipvs.Service) ([]ipvs.DestinationExtended, error) {
	return nil, nil
}

func (f *fakeIPVSClient) CreateService(ipvs.Service) error { return nil }

func (f *fakeIPVSClient) CreateDestination(ipvs.Service, ipvs.Destination) error {
	f.createDestinationCalls++
	if len(f.createDestinationErrors) > 0 {
		err := f.createDestinationErrors[0]
		f.createDestinationErrors = f.createDestinationErrors[1:]
		return err
	}
	return nil
}

func TestHealthCheckRetriesFailedIPVSUpdates(t *testing.T) {
	createErr := errors.New("create destination failed")
	removeErr := errors.New("remove destination failed")

	t.Run("failed creation retries after healthy cycle", func(t *testing.T) {
		fake := &fakeIPVSClient{createDestinationErrors: []error{createErr}}
		lb := &IPVSLoadBalancer{
			client:              fake,
			loadBalancerService: ipvs.Service{Family: ipvs.INET},
			backendMap:          backend.Map{},
			forwardingMethod:    ipvs.Local,
		}
		b := &fakeBackend{addr: "10.0.0.1", local: true, healthy: false}
		lb.backendMap.Add(&backend.Entry{Backend: b, Healthy: b.healthy, IsLocal: b.local})

		b.healthy = true
		lb.performHealthCheck(context.Background())
		entry := lb.backendMap[b.Key()]
		if !entry.Healthy || entry.Applied {
			t.Fatalf("after failed create: Healthy=%t Applied=%t, want true/false", entry.Healthy, entry.Applied)
		}
		if fake.createDestinationCalls != 1 {
			t.Fatalf("CreateDestination calls after failure = %d, want 1", fake.createDestinationCalls)
		}

		lb.performHealthCheck(context.Background())
		if !entry.Healthy || !entry.Applied {
			t.Fatalf("after retry: Healthy=%t Applied=%t, want true/true", entry.Healthy, entry.Applied)
		}
		if fake.createDestinationCalls != 2 {
			t.Fatalf("CreateDestination calls after retry = %d, want 2", fake.createDestinationCalls)
		}

		lb.performHealthCheck(context.Background())
		if fake.createDestinationCalls != 2 {
			t.Fatalf("CreateDestination calls after unchanged healthy probe = %d, want 2", fake.createDestinationCalls)
		}
	})

	t.Run("failed removal retries after unhealthy cycle", func(t *testing.T) {
		fake := &fakeIPVSClient{removeDestinationErrors: []error{removeErr}}
		lb := &IPVSLoadBalancer{
			client:              fake,
			loadBalancerService: ipvs.Service{Family: ipvs.INET},
			backendMap:          backend.Map{},
			forwardingMethod:    ipvs.Local,
		}
		b := &fakeBackend{addr: "10.0.0.1", local: true, healthy: true}
		if err := lb.registerBackend(context.Background(), b, b.local, false); err != nil {
			t.Fatalf("registerBackend() error = %v", err)
		}
		entry := lb.backendMap[b.Key()]
		if !entry.Healthy || !entry.Applied {
			t.Fatalf("after registration: Healthy=%t Applied=%t, want true/true", entry.Healthy, entry.Applied)
		}

		b.healthy = false
		lb.performHealthCheck(context.Background())
		if entry.Healthy || !entry.Applied {
			t.Fatalf("after failed removal: Healthy=%t Applied=%t, want false/true", entry.Healthy, entry.Applied)
		}
		if fake.removeDestinationCalls != 1 {
			t.Fatalf("RemoveDestination calls after failure = %d, want 1", fake.removeDestinationCalls)
		}

		lb.performHealthCheck(context.Background())
		if entry.Healthy || entry.Applied {
			t.Fatalf("after retry: Healthy=%t Applied=%t, want false/false", entry.Healthy, entry.Applied)
		}
		if fake.removeDestinationCalls != 2 {
			t.Fatalf("RemoveDestination calls after retry = %d, want 2", fake.removeDestinationCalls)
		}

		lb.performHealthCheck(context.Background())
		if fake.removeDestinationCalls != 2 {
			t.Fatalf("RemoveDestination calls after unchanged unhealthy probe = %d, want 2", fake.removeDestinationCalls)
		}
	})
}

func TestRegisterBackendMarksSuccessfulRegistrationApplied(t *testing.T) {
	fake := &fakeIPVSClient{}
	kills := 0
	lb := &IPVSLoadBalancer{
		client:              fake,
		loadBalancerService: ipvs.Service{Family: ipvs.INET},
		backendMap:          backend.Map{},
		forwardingMethod:    ipvs.Local,
		killFunc:            func() { kills++ },
	}
	b := &fakeBackend{addr: "10.0.0.1", local: true, healthy: true}

	if err := lb.registerBackend(context.Background(), b, b.local, false); err != nil {
		t.Fatalf("registerBackend() error = %v", err)
	}
	entry := lb.backendMap[b.Key()]
	if !entry.Healthy || !entry.Applied || !entry.IsLocal {
		t.Fatalf("after registration: Healthy=%t Applied=%t IsLocal=%t, want true/true/true", entry.Healthy, entry.Applied, entry.IsLocal)
	}
	if fake.createDestinationCalls != 1 {
		t.Fatalf("CreateDestination calls after registration = %d, want 1", fake.createDestinationCalls)
	}

	lb.performHealthCheck(context.Background())
	if !entry.Healthy || !entry.Applied || !entry.IsLocal {
		t.Fatalf("after healthy tick: Healthy=%t Applied=%t IsLocal=%t, want true/true/true", entry.Healthy, entry.Applied, entry.IsLocal)
	}
	if fake.createDestinationCalls != 1 {
		t.Fatalf("CreateDestination calls after unchanged healthy probe = %d, want 1", fake.createDestinationCalls)
	}
	if kills != 0 {
		t.Fatalf("killFunc calls after healthy tick = %d, want 0", kills)
	}
}

func TestRemoveLastRegisteredLocalBackendRequestsOneRestart(t *testing.T) {
	kills := 0
	fake := &fakeIPVSClient{}
	lb := &IPVSLoadBalancer{
		client:              fake,
		loadBalancerService: ipvs.Service{Family: ipvs.INET},
		backendMap:          backend.Map{},
		forwardingMethod:    ipvs.Local,
		killFunc:            func() { kills++ },
	}
	b := &fakeBackend{addr: "10.0.0.1", local: true, healthy: true}
	if err := lb.registerBackend(context.Background(), b, true, false); err != nil {
		t.Fatalf("registerBackend() error = %v", err)
	}
	if err := lb.RemoveBackend(b.addr, b.Port()); err != nil {
		t.Fatalf("RemoveBackend() error = %v", err)
	}
	if kills != 1 {
		t.Fatalf("killFunc calls after removal = %d, want 1", kills)
	}

	lb.performHealthCheck(context.Background())
	if kills != 1 {
		t.Fatalf("killFunc calls after following tick = %d, want 1", kills)
	}
}

func TestHealthCheckRestartAfterUnknownLocality(t *testing.T) {
	t.Run("healthy backend does not restart", func(t *testing.T) {
		kills := 0
		lb := &IPVSLoadBalancer{
			client:              &fakeIPVSClient{},
			loadBalancerService: ipvs.Service{Family: ipvs.INET},
			backendMap:          backend.Map{},
			forwardingMethod:    ipvs.Local,
			killFunc:            func() { kills++ },
		}
		b := &fakeBackend{addr: "10.0.0.1", healthy: true}
		if err := lb.registerBackend(context.Background(), b, false, true); err != nil {
			t.Fatalf("registerBackend() error = %v", err)
		}
		lb.performHealthCheck(context.Background())
		entry := lb.backendMap[b.Key()]
		if !entry.Healthy || !entry.Applied || entry.IsLocal {
			t.Fatalf("after healthy tick: Healthy=%t Applied=%t IsLocal=%t, want true/true/false", entry.Healthy, entry.Applied, entry.IsLocal)
		}
		if kills != 0 {
			t.Fatalf("killFunc calls after healthy tick = %d, want 0", kills)
		}
	})

	t.Run("backend becoming unhealthy restarts once", func(t *testing.T) {
		kills := 0
		lb := &IPVSLoadBalancer{
			client:              &fakeIPVSClient{},
			loadBalancerService: ipvs.Service{Family: ipvs.INET},
			backendMap:          backend.Map{},
			forwardingMethod:    ipvs.Local,
			killFunc:            func() { kills++ },
		}
		b := &fakeBackend{addr: "10.0.0.1", healthy: true}
		if err := lb.registerBackend(context.Background(), b, false, true); err != nil {
			t.Fatalf("registerBackend() error = %v", err)
		}
		b.healthy = false
		lb.performHealthCheck(context.Background())
		entry := lb.backendMap[b.Key()]
		if entry.Healthy || entry.Applied {
			t.Fatalf("after unhealthy tick: Healthy=%t Applied=%t, want false/false", entry.Healthy, entry.Applied)
		}
		if kills != 1 {
			t.Fatalf("killFunc calls after unhealthy tick = %d, want 1", kills)
		}
		lb.performHealthCheck(context.Background())
		if kills != 1 {
			t.Fatalf("killFunc calls after subsequent tick = %d, want 1", kills)
		}
	})

	t.Run("initially unhealthy backend still restarts", func(t *testing.T) {
		kills := 0
		lb := &IPVSLoadBalancer{
			client:              &fakeIPVSClient{},
			loadBalancerService: ipvs.Service{Family: ipvs.INET},
			backendMap:          backend.Map{},
			forwardingMethod:    ipvs.Local,
			killFunc:            func() { kills++ },
		}
		b := &fakeBackend{addr: "10.0.0.1", healthy: false}
		if err := lb.registerBackend(context.Background(), b, false, true); err != nil {
			t.Fatalf("registerBackend() error = %v", err)
		}
		entry := lb.backendMap[b.Key()]
		if entry.Healthy || entry.Applied {
			t.Fatalf("after registration: Healthy=%t Applied=%t, want false/false", entry.Healthy, entry.Applied)
		}
		lb.performHealthCheck(context.Background())
		if kills != 1 {
			t.Fatalf("killFunc calls after unhealthy tick = %d, want 1", kills)
		}
		lb.performHealthCheck(context.Background())
		if kills != 1 {
			t.Fatalf("killFunc calls after subsequent tick = %d, want 1", kills)
		}
	})
}

func TestUnhealthyLocalBackendRestartsDespiteRemovalFailure(t *testing.T) {
	fake := &fakeIPVSClient{removeDestinationErrors: []error{errors.New("remove failed")}}
	kills := 0
	lb := &IPVSLoadBalancer{
		client:              fake,
		loadBalancerService: ipvs.Service{Family: ipvs.INET},
		backendMap:          backend.Map{},
		forwardingMethod:    ipvs.Local,
		killFunc:            func() { kills++ },
	}
	b := &fakeBackend{addr: "10.0.0.1", local: true, healthy: true}
	if err := lb.registerBackend(context.Background(), b, b.local, false); err != nil {
		t.Fatalf("registerBackend() error = %v", err)
	}
	b.healthy = false
	lb.performHealthCheck(context.Background())
	entry := lb.backendMap[b.Key()]
	if entry.Healthy || !entry.Applied {
		t.Fatalf("after failed removal: Healthy=%t Applied=%t, want false/true", entry.Healthy, entry.Applied)
	}
	if fake.removeDestinationCalls != 1 {
		t.Fatalf("RemoveDestination calls = %d, want 1", fake.removeDestinationCalls)
	}
	if kills != 1 {
		t.Fatalf("killFunc calls = %d, want 1", kills)
	}
}

func TestRemoveBackendRemovesLocalBackend(t *testing.T) {
	fake := &fakeIPVSClient{}
	lb := &IPVSLoadBalancer{client: fake, loadBalancerService: ipvs.Service{Family: ipvs.INET}, backendMap: backend.Map{}}

	b, err := backend.New(&backend.Config{Type: backend.Discovery, Address: "10.0.0.5", Port: 6443})
	if err != nil {
		t.Fatalf("creating backend: %v", err)
	}
	lb.backendMap.Add(&backend.Entry{Backend: b, Healthy: true, IsLocal: true})

	// Backend identity must not depend on IsLocal; previously local backends could never be removed.
	if err := lb.RemoveBackend("10.0.0.5", 6443); err != nil {
		t.Fatalf("RemoveBackend() error = %v", err)
	}
	if lb.backendMap.Find(backend.Discovery, "10.0.0.5", 6443) != nil {
		t.Error("local backend still present in backendMap after RemoveBackend")
	}
	if len(fake.removed) != 1 {
		t.Errorf("RemoveDestination calls = %d, want 1", len(fake.removed))
	}
}

type fakeBackend struct {
	addr    string
	local   bool
	healthy bool
}

func (f *fakeBackend) Check(context.Context) (bool, error) { return f.healthy, nil }
func (f *fakeBackend) Address() string                     { return f.addr }
func (f *fakeBackend) Port() uint16                        { return 6443 }
func (f *fakeBackend) Key() backend.Key                    { return backend.NewKey(backend.Discovery, f.addr, 6443) }

// ctxBackend reports healthy only while the probe context is alive, like the
// Discovery backend whose request fails once its context is cancelled.
type ctxBackend struct{ fakeBackend }

func (c *ctxBackend) Check(ctx context.Context) (bool, error) { return ctx.Err() == nil, ctx.Err() }

// cancellingBackend cancels the probe context from inside Check and then reports
// unhealthy, like a probe that is interrupted by shutdown or lost leadership. The
// cancel func is nil while the backend is first registered, so Check must tolerate
// being probed before cancel is set.
type cancellingBackend struct {
	fakeBackend
	cancel context.CancelFunc
}

func (c *cancellingBackend) Check(ctx context.Context) (bool, error) {
	if c.cancel != nil {
		c.cancel()
	}
	return ctx.Err() == nil, ctx.Err()
}

func TestLocalForwardingRestart(t *testing.T) {
	const localAddr = "10.0.0.1"
	const remoteAddr = "10.0.0.2"

	local := func(healthy bool) *fakeBackend {
		return &fakeBackend{addr: localAddr, local: true, healthy: healthy}
	}
	remote := func(healthy bool) *fakeBackend {
		return &fakeBackend{addr: remoteAddr, local: false, healthy: healthy}
	}

	tests := []struct {
		name             string
		backends         []*fakeBackend
		initiallyHealthy map[string]bool
		remove           string
		wantKillOnRemove bool
		wantKill         bool
		// wantKills is the exact number of killFunc calls after the tick.
		wantKills int
	}{
		{
			name:      "empty map at startup",
			backends:  nil,
			wantKill:  false,
			wantKills: 0,
		},
		{
			name:      "only healthy remote, local never registered",
			backends:  []*fakeBackend{remote(true)},
			wantKill:  false,
			wantKills: 0,
		},
		{
			name:      "local registered but unhealthy",
			backends:  []*fakeBackend{local(false), remote(true)},
			wantKill:  true,
			wantKills: 1,
		},
		{
			name:             "local becomes unhealthy",
			backends:         []*fakeBackend{local(false), remote(true)},
			initiallyHealthy: map[string]bool{localAddr: true},
			wantKill:         true,
			wantKills:        1,
		},
		{
			name:      "local healthy, remote healthy",
			backends:  []*fakeBackend{local(true), remote(true)},
			wantKill:  false,
			wantKills: 0,
		},
		{
			name:      "local healthy, remote failing",
			backends:  []*fakeBackend{local(true), remote(false)},
			wantKill:  false,
			wantKills: 0,
		},
		{
			name:             "last local removed, healthy remote remains",
			backends:         []*fakeBackend{local(true), remote(true)},
			remove:           localAddr,
			wantKillOnRemove: true,
			wantKill:         true,
			wantKills:        1,
		},
		{
			name:      "remote removed, local healthy",
			backends:  []*fakeBackend{local(true), remote(true)},
			remove:    remoteAddr,
			wantKill:  false,
			wantKills: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			kills := 0
			lb := &IPVSLoadBalancer{
				client:              &fakeIPVSClient{},
				loadBalancerService: ipvs.Service{Family: ipvs.INET},
				backendMap:          backend.Map{},
				forwardingMethod:    ipvs.Local,
				killFunc:            func() { kills++ },
			}

			for _, b := range tc.backends {
				healthy := b.healthy
				if v, ok := tc.initiallyHealthy[b.addr]; ok {
					healthy = v
				}
				lb.backendMap[b.Key()] = &backend.Entry{Backend: b, Healthy: healthy, IsLocal: b.local}
			}

			if tc.remove != "" {
				if err := lb.RemoveBackend(tc.remove, 6443); err != nil {
					t.Fatalf("RemoveBackend() error = %v", err)
				}
				// Removal itself must request the restart, without waiting for the next tick.
				if got := kills > 0; got != tc.wantKillOnRemove {
					t.Errorf("killFunc called by RemoveBackend = %v, want %v", got, tc.wantKillOnRemove)
				}
			}

			lb.performHealthCheck(context.Background())

			if got := kills > 0; got != tc.wantKill {
				t.Errorf("killFunc called = %v, want %v", got, tc.wantKill)
			}
			// Removal and the following tick must request only one restart: killFunc
			// is guarded by killInProgress, so the tick must not repeat RemoveBackend's request.
			if kills != tc.wantKills {
				t.Errorf("killFunc calls after tick = %d, want %d", kills, tc.wantKills)
			}
		})
	}
}

func TestLocalForwardingRestartIgnoredInOtherModes(t *testing.T) {
	killed := false
	lb := &IPVSLoadBalancer{
		client:              &fakeIPVSClient{},
		loadBalancerService: ipvs.Service{Family: ipvs.INET},
		backendMap:          backend.Map{},
		forwardingMethod:    ipvs.Masquerade,
		killFunc:            func() { killed = true },
	}
	lb.backendMap.Add(&backend.Entry{Backend: &fakeBackend{addr: "10.0.0.1", local: true}, Healthy: true, IsLocal: true})
	lb.backendMap.Add(&backend.Entry{Backend: &fakeBackend{addr: "10.0.0.2", local: false, healthy: true}, Healthy: true})

	lb.performHealthCheck(context.Background())

	if killed {
		t.Error("killFunc called in non-Local forwarding mode, want no call")
	}
}

// TestRegisterBackendTracksLocalBackend registers backends through registerBackend
// (the part of AddBackend after the backend is built), so it guards that IsLocal
// is recorded on the entry the restart decision relies on.
func TestRegisterBackendTracksLocalBackend(t *testing.T) {
	tests := []struct {
		name             string
		local            bool
		initiallyHealthy bool
		wantKill         bool
	}{
		{name: "local registered healthy, then fails", local: true, initiallyHealthy: true, wantKill: true},
		{name: "local registered unhealthy", local: true, initiallyHealthy: false, wantKill: true},
		{name: "remote only, then fails", local: false, initiallyHealthy: true, wantKill: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			kills := 0
			lb := &IPVSLoadBalancer{
				client:              &fakeIPVSClient{},
				loadBalancerService: ipvs.Service{Family: ipvs.INET},
				backendMap:          backend.Map{},
				forwardingMethod:    ipvs.Local,
				killFunc:            func() { kills++ },
			}
			b := &fakeBackend{addr: "10.0.0.1", local: tc.local, healthy: tc.initiallyHealthy}
			if err := lb.registerBackend(context.Background(), b, b.local, false); err != nil {
				t.Fatalf("registerBackend() error = %v", err)
			}
			if kills != 0 {
				t.Fatalf("killFunc called during registration")
			}

			b.healthy = false
			lb.performHealthCheck(context.Background())

			if got := kills > 0; got != tc.wantKill {
				t.Errorf("killFunc called = %v, want %v", got, tc.wantKill)
			}
		})
	}
}

func TestRegisterBackendCorrectsLocalityAndEnablesFailover(t *testing.T) {
	kills := 0
	lb := &IPVSLoadBalancer{
		client:              &fakeIPVSClient{},
		loadBalancerService: ipvs.Service{Family: ipvs.INET},
		backendMap:          backend.Map{},
		forwardingMethod:    ipvs.Local,
		killFunc:            func() { kills++ },
	}
	original := &fakeBackend{addr: "192.0.2.1", healthy: true}
	if err := lb.registerBackend(context.Background(), original, false, false); err != nil {
		t.Fatalf("initial registration: %v", err)
	}
	corrected := &fakeBackend{addr: original.addr, local: true}
	if err := lb.registerBackend(context.Background(), corrected, corrected.local, false); err != nil {
		t.Fatalf("duplicate registration: %v", err)
	}
	entry := lb.backendMap[original.Key()]
	if len(lb.backendMap) != 1 || entry.Backend != original || !entry.Healthy {
		t.Fatal("duplicate registration replaced the backend or reset its health")
	}
	if !entry.IsLocal {
		t.Fatal("correcting locality did not enable local failover")
	}
	if kills != 0 {
		t.Fatal("locality correction requested a restart while the backend was healthy")
	}

	original.healthy = false
	lb.performHealthCheck(context.Background())
	if kills != 1 {
		t.Fatalf("restart calls after corrected local backend failed = %d, want 1", kills)
	}
}

func TestRegisterBackendPreservesLocalityOnLookupError(t *testing.T) {
	for _, healthy := range []bool{false, true} {
		name := "unhealthy"
		if healthy {
			name = "healthy"
		}
		t.Run(name, func(t *testing.T) {
			original := &fakeBackend{addr: "192.0.2.1", local: true, healthy: healthy}
			lb := &IPVSLoadBalancer{backendMap: backend.Map{}, forwardingMethod: ipvs.Local}
			lb.backendMap[original.Key()] = &backend.Entry{Backend: original, Healthy: healthy, IsLocal: original.local}
			incoming, err := backend.New(&backend.Config{
				Type: backend.Discovery, Address: original.addr, Port: original.Port(),
			})
			if err != nil {
				t.Fatalf("creating backend: %v", err)
			}
			if err := lb.registerBackend(context.Background(), incoming, false, true); err != nil {
				t.Fatalf("duplicate registration: %v", err)
			}
			entry := lb.backendMap[original.Key()]
			if len(lb.backendMap) != 1 || entry.Backend != original || entry.Healthy != healthy {
				t.Fatal("lookup error replaced the backend or changed its health")
			}
			if !entry.IsLocal {
				t.Fatal("lookup error discarded known locality or local registration")
			}
		})
	}
}

func TestIsLocalOnLinks(t *testing.T) {
	lookupErr := errors.New("address lookup failed")
	links := []netlink.Link{
		&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "first"}},
		&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "second"}},
	}
	tests := []struct {
		name       string
		address    string
		firstError bool
		match      bool
		wantLocal  bool
		wantError  bool
	}{
		{name: "lookup error without match", address: "192.0.2.1", firstError: true, wantError: true},
		{name: "IPv4 match after lookup error", address: "192.0.2.1", firstError: true, match: true, wantLocal: true},
		{name: "IPv6 match after lookup error", address: "2001:db8::1", firstError: true, match: true, wantLocal: true},
		{name: "nonlocal after successful lookups", address: "192.0.2.1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			target := net.ParseIP(tc.address)
			calls := 0
			local, err := isLocalOnLinks(target, links, func(link netlink.Link, family int) ([]netlink.Addr, error) {
				calls++
				wantFamily := netlink.FAMILY_V4
				if target.To4() == nil {
					wantFamily = netlink.FAMILY_V6
				}
				if family != wantFamily {
					t.Errorf("address family = %d, want %d", family, wantFamily)
				}
				if link.Attrs().Name == "first" && tc.firstError {
					return nil, lookupErr
				}
				if link.Attrs().Name == "second" && tc.match {
					return []netlink.Addr{{IPNet: &net.IPNet{IP: target}, Scope: int(netlink.SCOPE_UNIVERSE)}}, nil
				}
				return nil, nil
			})
			if calls != len(links) {
				t.Errorf("address lookup calls = %d, want %d", calls, len(links))
			}
			if local != tc.wantLocal {
				t.Errorf("local = %t, want %t", local, tc.wantLocal)
			}
			if tc.wantError {
				if !errors.Is(err, lookupErr) {
					t.Errorf("error = %v, want lookup error", err)
				}
			} else if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestPerformHealthCheckIgnoresCancelledContext(t *testing.T) {
	tests := []struct {
		name string
		// cancelDuringProbe selects a backend that cancels the tick's context from
		// inside Check; otherwise the context is cancelled before the tick starts.
		cancelDuringProbe bool
	}{
		{name: "cancelled before tick"},
		{name: "cancelled during probe", cancelDuringProbe: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeIPVSClient{}
			kills := 0
			lb := &IPVSLoadBalancer{
				client:              fake,
				loadBalancerService: ipvs.Service{Family: ipvs.INET},
				backendMap:          backend.Map{},
				forwardingMethod:    ipvs.Local,
				killFunc:            func() { kills++ },
			}

			var b backend.Backend
			var cb *cancellingBackend
			if tc.cancelDuringProbe {
				cb = &cancellingBackend{fakeBackend: fakeBackend{addr: "10.0.0.1", local: true}}
				b = cb
			} else {
				b = &ctxBackend{fakeBackend{addr: "10.0.0.1", local: true}}
			}
			// Registration uses a live context, so the backend starts healthy and applied.
			if err := lb.registerBackend(context.Background(), b, true, false); err != nil {
				t.Fatalf("registerBackend() error = %v", err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if cb != nil {
				cb.cancel = cancel
			} else {
				cancel()
			}

			// A probe failed by context cancellation (shutdown / lost leadership) says
			// nothing about the backend and must not remove it or restart kube-vip.
			lb.performHealthCheck(ctx)

			if kills != 0 {
				t.Errorf("killFunc calls = %d, want 0", kills)
			}
			entry := lb.backendMap[b.Key()]
			if !entry.Healthy || !entry.Applied {
				t.Errorf("after cancelled tick: Healthy=%t Applied=%t, want true/true", entry.Healthy, entry.Applied)
			}
			if fake.removeDestinationCalls != 0 {
				t.Errorf("RemoveDestination calls = %d, want 0", fake.removeDestinationCalls)
			}
		})
	}
}

func TestRegisterBackendIgnoresCancelledContext(t *testing.T) {
	tests := []struct {
		name string
		// cancelDuringProbe selects a backend that cancels the context from inside
		// Check; otherwise the context is cancelled before registration starts.
		cancelDuringProbe bool
	}{
		{name: "cancelled before registration"},
		{name: "cancelled during probe", cancelDuringProbe: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			var b backend.Backend = &fakeBackend{addr: "10.0.0.1", local: true, healthy: true}
			if tc.cancelDuringProbe {
				b = &cancellingBackend{fakeBackend: fakeBackend{addr: "10.0.0.1", local: true}, cancel: cancel}
			} else {
				cancel()
			}

			fake := &fakeIPVSClient{}
			lb := &IPVSLoadBalancer{
				client:              fake,
				loadBalancerService: ipvs.Service{Family: ipvs.INET},
				backendMap:          backend.Map{},
				forwardingMethod:    ipvs.Local,
			}

			err := lb.registerBackend(ctx, b, true, false)
			if !errors.Is(err, context.Canceled) {
				t.Errorf("registerBackend() error = %v, want context.Canceled", err)
			}
			if len(lb.backendMap) != 0 {
				t.Errorf("backendMap size = %d, want 0", len(lb.backendMap))
			}
			if fake.createDestinationCalls != 0 {
				t.Errorf("CreateDestination calls = %d, want 0", fake.createDestinationCalls)
			}
		})
	}
}

// A cancelled tick with no backends must not request a restart, even though
// local registration is recorded: the early return has to run before the
// restart check after the loop.
func TestPerformHealthCheckEmptyMapIgnoresCancelledContext(t *testing.T) {
	kills := 0
	lb := &IPVSLoadBalancer{
		client:              &fakeIPVSClient{},
		loadBalancerService: ipvs.Service{Family: ipvs.INET},
		backendMap:          backend.Map{},
		forwardingMethod:    ipvs.Local,
		killFunc:            func() { kills++ },
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	lb.performHealthCheck(ctx)

	if kills != 0 {
		t.Errorf("killFunc calls = %d, want 0", kills)
	}
}

// A cancelled duplicate registration must not change the locality of the
// already tracked backend, nor enable local failover.
func TestRegisterBackendDuplicateIgnoresCancelledContext(t *testing.T) {
	fake := &fakeIPVSClient{}
	lb := &IPVSLoadBalancer{
		client:              fake,
		loadBalancerService: ipvs.Service{Family: ipvs.INET},
		backendMap:          backend.Map{},
		forwardingMethod:    ipvs.Local,
	}
	existing := &fakeBackend{addr: "10.0.0.1", local: false, healthy: true}
	lb.backendMap.Add(&backend.Entry{Backend: existing, Healthy: existing.healthy, IsLocal: existing.local})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	incoming := &fakeBackend{addr: "10.0.0.1", local: true, healthy: true}

	err := lb.registerBackend(ctx, incoming, incoming.local, false)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("registerBackend() error = %v, want context.Canceled", err)
	}
	entry := lb.backendMap[existing.Key()]
	if entry.IsLocal {
		t.Error("existing backend locality was updated by a cancelled registration")
	}
	if len(lb.backendMap) != 1 {
		t.Fatalf("backendMap size = %d, want 1", len(lb.backendMap))
	}
	if entry.Backend != existing {
		t.Error("cancelled duplicate registration replaced the tracked backend")
	}
	if fake.createDestinationCalls != 0 {
		t.Errorf("CreateDestination calls = %d, want 0", fake.createDestinationCalls)
	}
}
