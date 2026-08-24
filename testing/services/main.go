package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gookit/slog"
	"github.com/kube-vip/kube-vip/pkg/k8s"
	"github.com/kube-vip/kube-vip/testing/services/pkg/deployment"
	"golang.org/x/sync/errgroup"
)

func main() {
	os.Exit(run())
}

func run() int {
	var existing bool
	var t deployment.TestConfig

	t.ImagePath = os.Getenv("E2E_IMAGE_PATH")

	flag.BoolVar(&t.ControlPlane, "ControlPlane", false, "")
	flag.BoolVar(&t.Services, "Services", false, "")
	flag.BoolVar(&t.Simple, "simple", false, "Perform a simple loadbalancer test")
	flag.BoolVar(&t.Deployments, "deployments", false, "Perform a deployment test")
	flag.BoolVar(&t.LeaderFailover, "leaderFailover", false, "Perform a failover of the leader test")
	flag.BoolVar(&t.LeaderActive, "leaderActive", false, "Perform a test on the active leader")
	flag.BoolVar(&t.LocalDeploy, "localDeploy", false, "Perform a test on the active leader")
	flag.BoolVar(&t.FaultElection, "electionFaults", false, "Perform leader election fault injection tests")
	flag.BoolVar(&t.Egress, "egress", false, "Perform an egress test")
	flag.BoolVar(&t.EgressInternal, "egressInternal", false, "Perform an egress test, using the internal functionality")
	flag.BoolVar(&t.EgressIPv6, "egressIPv6", false, "Perform an egress test")
	flag.BoolVar(&t.DualStack, "dualStack", false, "Perform an dual stack test")
	flag.BoolVar(&t.RetainCluster, "retain", false, "Retain the cluster")
	flag.StringVar(&t.KindVersionImage, "kindImage", "", "The image to use for the kind nodes e.g. (kindest/node:v1.21.14)")
	flag.BoolVar(&existing, "existing", false, "Use an existing cluster")
	flag.BoolVar(&t.IPv6, "ipv6", false, "Perform an IPv6-only test")
	flag.StringVar(&t.DockerNIC, "networkInterface", "br-", "Selects networking interface to use")
	flag.BoolVar(&t.GlobalWatch, "globalWatch", false, "watch services across all namespaces instead of scoping each DaemonSet to its test namespace")
	flag.BoolVar(&t.Cilium, "cilium", false, "Use cilium as a CNI")

	flag.Parse()

	slog.Infof("beginning e2e tests, image: [%s] DualStack [%t]", t.ImagePath, t.DualStack)

	t.Affinity = os.Getenv("NODE_TOLERATE")
	t.DeploymentName = "kube-vip-deploy"
	t.ServiceName = "kube-vip-service"
	t.LeaderName = "kube-vip-deploy-leader"

	var err error
	t.TempDirPath, err = os.MkdirTemp("", "kube-vip-services")
	if err != nil {
		slog.Fatal(err)
	}
	defer func() {
		if os.Getenv("E2E_KEEP_LOGS") != "true" {
			if err := os.RemoveAll(t.TempDirPath); err != nil {
				slog.Errorf("failed to remove temporary directory %q: %v", t.TempDirPath, err)
			}
		}
	}()

	if t.ControlPlane {
		err := t.CreateKind()
		if !t.RetainCluster {
			if err != nil {
				slog.Fatal(err)
			}
			defer func() {
				if err := deployment.DeleteKind(); err != nil {
					slog.Fatal(err)
				}
			}()
		} else if err != nil {
			slog.Warn(err)
		}
	}

	if !t.Services {
		return 0
	}

	if !existing {
		err := t.CreateKind()
		if !t.RetainCluster {
			if err != nil {
				slog.Fatal(err)
			}
			defer func() {
				if err := deployment.DeleteKind(); err != nil {
					slog.Fatal(err)
				}
			}()
		} else if err != nil {
			slog.Warn(err)
		}
	} else {
		slog.Infof("Using existing cluster [%t]", existing)
	}

	ctx, cancel := context.WithCancel(context.TODO())
	defer cancel()
	homeConfigPath := filepath.Join(os.Getenv("HOME"), ".kube", "config")
	restCfg, err := k8s.NewRestConfig(homeConfigPath, false, "")
	if err != nil {
		slog.Fatalf("could not create k8s REST config from external file: %q: %v", homeConfigPath, err)
	}
	// The fault tests poll the API a lot; raise QPS to avoid rate-limiter delays.
	restCfg.QPS = 50
	restCfg.Burst = 100

	clientset, err := k8s.NewClientset(restCfg)
	if err != nil {
		slog.Fatalf("could not create k8s clientset from external file: %q: %v", homeConfigPath, err)
	}
	slog.Debugf("Using external Kubernetes configuration from file [%s]", homeConfigPath)

	// Each test group runs in its own namespace with a dedicated kube-vip DaemonSet
	// scoped to that namespace.  EnsureNamespace creates the namespace, Role,
	// RoleBinding, ClusterRoleBinding (nodes-only), and the per-namespace DaemonSet.
	ensure := func(ns string) {
		if err := deployment.EnsureNamespace(ctx, clientset, ns, t.ImagePath, t.GlobalWatch); err != nil {
			slog.Fatalf("failed to create namespace %q: %v", ns, err)
		}
	}
	cleanup := func(ns string) {
		if err := deployment.DeleteNamespace(ctx, clientset, ns); err != nil {
			slog.Warnf("failed to delete namespace %q: %v", ns, err)
		}
	}
	// Collect every test result so multiple parallel failures are all reported
	// instead of collapsing to the errgroup's first error.
	var resultsMu sync.Mutex
	var errs []error
	passed := 0
	runTimed := func(name string, test func() error) error {
		started := time.Now()
		err := test()
		slog.Infof("test %q completed in %s", name, time.Since(started).Round(time.Millisecond))
		resultsMu.Lock()
		defer resultsMu.Unlock()
		if err != nil {
			errs = append(errs, fmt.Errorf("test %q: %w", name, err))
		} else {
			passed++
		}
		return err
	}
	finish := func() int {
		const testComplete = "🏆 Testing Complete [%d] passed / [%d] failed"
		if len(errs) > 0 {
			for _, err := range errs {
				slog.Error(err)
			}
			slog.Errorf(testComplete, passed, len(errs))
		} else {
			slog.Infof(testComplete, passed, len(errs))
		}
		return len(errs)
	}

	// --- Phase A: run independent tests in parallel, each in its own namespace ---
	parallelGroup, parallelCtx := errgroup.WithContext(ctx)

	if t.Simple {
		ns := "kube-vip-simple"
		ensure(ns)
		cfg := t.WithNamespace(ns)
		parallelGroup.Go(func() error {
			defer cleanup(ns)
			return runTimed("simple", func() error { return cfg.SimpleDeployment(parallelCtx, clientset) })
		})
	}
	if t.Deployments {
		ns := "kube-vip-multi"
		ensure(ns)
		cfg := t.WithNamespace(ns)
		parallelGroup.Go(func() error {
			defer cleanup(ns)
			return runTimed("deployments", func() error { return cfg.MultipleDeployments(parallelCtx, clientset) })
		})
	}
	if t.LeaderFailover {
		ns := "kube-vip-failover-a"
		ensure(ns)
		cfg := t.WithNamespace(ns)
		parallelGroup.Go(func() error {
			defer cleanup(ns)
			return runTimed("leader-failover", func() error { return cfg.Failover(parallelCtx, clientset) })
		})
	}
	if t.LeaderActive {
		ns := "kube-vip-failover-b"
		ensure(ns)
		cfg := t.WithNamespace(ns)
		parallelGroup.Go(func() error {
			defer cleanup(ns)
			return runTimed("leader-active", func() error { return cfg.Failover(parallelCtx, clientset) })
		})
	}
	if t.LocalDeploy {
		ns := "kube-vip-local"
		ensure(ns)
		cfg := t.WithNamespace(ns)
		parallelGroup.Go(func() error {
			defer cleanup(ns)
			return runTimed("local-deployment", func() error { return cfg.LocalDeployment(parallelCtx, clientset) })
		})
	}
	if t.DualStack {
		ns := "kube-vip-dualstack"
		ensure(ns)
		cfg := t.WithNamespace(ns)
		parallelGroup.Go(func() error {
			defer cleanup(ns)
			return runTimed("dual-stack", func() error { return cfg.DualStackDeployment(parallelCtx, clientset) })
		})
	}

	phaseStarted := time.Now()
	// Individual results were already collected by runTimed.
	_ = parallelGroup.Wait()
	slog.Infof("phase A completed in %s", time.Since(phaseStarted).Round(time.Millisecond))

	// Wait for Phase A pods to terminate before Phase B to avoid port-2112 conflicts.
	for _, ns := range []string{"kube-vip-simple", "kube-vip-multi", "kube-vip-failover-a", "kube-vip-failover-b", "kube-vip-local", "kube-vip-dualstack"} {
		if err := deployment.WaitForNamespaceGone(ctx, clientset, ns); err != nil {
			errs = append(errs, fmt.Errorf("phase A cleanup %q: %w", ns, err))
		}
	}
	if len(errs) > 0 {
		return finish()
	}

	// --- Phase B: egress tests ---
	// IPv4 and IPv6 egress bind different host ports so run in parallel;
	// the two variants of each family share a host port and stay sequential.
	if t.Egress || t.EgressIPv6 {
		phaseStarted = time.Now()
		egressGroup, egressCtx := errgroup.WithContext(ctx)

		if t.Egress {
			ns := "kube-vip-egress"
			if err := deployment.EnsureNamespace(ctx, clientset, ns, t.ImagePath, false); err != nil {
				slog.Fatalf("failed to create namespace %q: %v", ns, err)
			}
			cfg := t.WithNamespace(ns)
			egressGroup.Go(func() error {
				defer cleanup(ns)
				return runTimed("egress-ipv4", func() error {
					if err := cfg.EgressDeployment(egressCtx, clientset, false); err != nil {
						return err
					}
					if t.EgressInternal {
						return cfg.EgressDeployment(egressCtx, clientset, true)
					}
					return nil
				})
			})
		}
		if t.EgressIPv6 {
			ns := "kube-vip-egressv6"
			if err := deployment.EnsureNamespace(ctx, clientset, ns, t.ImagePath, false); err != nil {
				slog.Fatalf("failed to create namespace %q: %v", ns, err)
			}
			cfg := t.WithNamespace(ns)
			egressGroup.Go(func() error {
				defer cleanup(ns)
				return runTimed("egress-ipv6", func() error {
					if err := cfg.Egressv6Deployment(egressCtx, clientset, false); err != nil {
						return err
					}
					if t.EgressInternal {
						return cfg.Egressv6Deployment(egressCtx, clientset, true)
					}
					return nil
				})
			})
		}

		// Individual results were already collected by runTimed.
		_ = egressGroup.Wait()
		slog.Infof("phase B completed in %s", time.Since(phaseStarted).Round(time.Millisecond))

		// Wait for Phase B pods to terminate before Phase C.
		// Phase C's election DaemonSet binds :2112 and cannot do so if egress pods still hold the port.
		for _, ns := range []string{"kube-vip-egress", "kube-vip-egressv6"} {
			if err := deployment.WaitForNamespaceGone(ctx, clientset, ns); err != nil {
				errs = append(errs, fmt.Errorf("phase B cleanup %q: %w", ns, err))
			}
		}
		if len(errs) > 0 {
			return finish()
		}
	}

	// Phase C: ElectionFaults needs prometheus on :2112 for its convergence checks.
	// It runs alone so no host-port conflict.
	if t.FaultElection {
		ns := "kube-vip-election"
		if err := deployment.EnsureNamespaceWithMetrics(ctx, clientset, ns, t.ImagePath, t.GlobalWatch, ":2112"); err != nil {
			slog.Fatalf("failed to create namespace %q: %v", ns, err)
		}
		defer cleanup(ns)
		cfg := t.WithNamespace(ns)
		phaseStarted = time.Now()
		// The result is collected by runTimed.
		_ = runTimed("election-faults", func() error { return cfg.ElectionFaults(ctx, clientset) })
		slog.Infof("phase C completed in %s", time.Since(phaseStarted).Round(time.Millisecond))
	}

	return finish()
}
