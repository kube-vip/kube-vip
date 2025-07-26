package main

import (
	"context"
	"flag"
	"os"
	"path/filepath"

	"github.com/gookit/slog"
	"github.com/kube-vip/kube-vip/pkg/k8s"
	"github.com/kube-vip/kube-vip/testing/services/pkg/deployment"
)

func main() {
	var existing bool
	var t deployment.TestConfig

	t.ImagePath = os.Getenv("E2E_IMAGE_PATH")

	// _, t.ignoreSimple = os.LookupEnv("IGNORE_SIMPLE")
	// _, t.ignoreDeployments = os.LookupEnv("IGNORE_DEPLOY")
	// _, t.ignoreLeaderFailover = os.LookupEnv("IGNORE_LEADER")
	// _, t.ignoreLeaderActive = os.LookupEnv("IGNORE_ACTIVE")
	// _, t.ignoreLocalDeploy = os.LookupEnv("IGNORE_LOCALDEPLOY")
	// _, t.ignoreEgress = os.LookupEnv("IGNORE_EGRESS")
	// _, t.ignoreEgressIPv6 = os.LookupEnv("IGNORE_EGRESSIPV6")
	// _, t.ignoreDualStack = os.LookupEnv("IGNORE_DUALSTACK")
	// _, t.retainCluster = os.LookupEnv("RETAIN_CLUSTER")
	// _, t.IPv6 = os.LookupEnv("IPV6_FAMILY")
	// _, t.skipHostnameChange = os.LookupEnv("SKIP_HOSTNAME_CHANGE")

	//flag.StringVar(&t.ImagePath, "imagepath", "plndr/kube-vip:action", "")
	flag.BoolVar(&t.ControlPlane, "ControlPlane", false, "")
	flag.BoolVar(&t.Services, "Services", false, "")
	flag.BoolVar(&t.Simple, "simple", false, "Perform a simple loadbalancer test")
	flag.BoolVar(&t.Deployments, "deployments", false, "Perform a deployment test")
	flag.BoolVar(&t.LeaderFailover, "leaderFailover", false, "Perform a failover of the leader test")
	flag.BoolVar(&t.LeaderActive, "leaderActive", false, "Perform a test on the active leader")
	flag.BoolVar(&t.LocalDeploy, "localDeploy", false, "Perform a test on the active leader")
	flag.BoolVar(&t.Egress, "egress", false, "Perform an egress test")
	flag.BoolVar(&t.EgressInternal, "egressInternal", false, "Perform an egress test, using the internal functionality")
	flag.BoolVar(&t.EgressIPv6, "egressIPv6", false, "Perform an egress test")
	flag.BoolVar(&t.DualStack, "dualStack", false, "Perform an dual stack test")
	flag.BoolVar(&t.RetainCluster, "retain", false, "Retain the cluster")
	flag.StringVar(&t.KindVersionImage, "kindImage", "", "The image to use for the kind nodes e.g. (kindest/node:v1.21.14)")
	flag.BoolVar(&existing, "existing", false, "Use an existing cluster")
	flag.BoolVar(&t.IPv6, "ipv6", false, "Perform an IPv6-only test")
	flag.StringVar(&t.DockerNIC, "networkInterface", "br-", "Selects networking interface to use")

	flag.BoolVar(&t.Cilium, "cilium", false, "Use cilium as a CNI")

	flag.Parse()

	slog.Infof("🔬 beginning e2e tests, image: [%s] DualStack [%t]", t.ImagePath, t.DualStack)

	t.Affinity = os.Getenv("NODE_TOLERATE")

	t.DeploymentName = "kube-vip-deploy"
	t.ServiceName = "kube-vip-service"
	t.LeaderName = "kube-vip-deploy-leader"

	if t.ControlPlane {
		err := t.CreateKind()
		if !t.RetainCluster {
			if err != nil {
				slog.Fatal(err)
			}
			defer func() {
				err := deployment.DeleteKind()
				if err != nil {
					slog.Fatal(err)
				}
			}()
		} else {
			if err != nil {
				slog.Warn(err)
			}
		}
		// ctx, cancel := context.WithCancel(context.TODO())
		// defer cancel()
		// homeConfigPath := filepath.Join(os.Getenv("HOME"), ".kube", "config")
		// clientset, err := k8s.NewClientset(homeConfigPath, false, "")
		// if err != nil {
		// 	slog.Fatalf("could not create k8s clientset from external file: %q: %v", homeConfigPath, err)
		// }
		// slog.Debugf("Using external Kubernetes configuration from file [%s]", homeConfigPath)
		// err = t.startTest(ctx, clientset)
		// if err != nil {
		// 	slog.Fatal(err)
		// }
	}

	if t.Services {
		if !existing {
			err := t.CreateKind()
			if !t.RetainCluster {
				if err != nil {
					slog.Fatal(err)
				}
				defer func() {
					err := deployment.DeleteKind()
					if err != nil {
						slog.Fatal(err)
					}
				}()
			} else {
				if err != nil {
					slog.Warn(err)
				}
			}
		} else {
			slog.Infof("Using existing cluster [%t]", existing)
		}

		ctx, cancel := context.WithCancel(context.TODO())
		defer cancel()
		homeConfigPath := filepath.Join(os.Getenv("HOME"), ".kube", "config")
		config, err := k8s.NewRestConfig(homeConfigPath, false, "")
		if err != nil {
			slog.Fatalf("could not create k8s REST config from external file: %q: %v", homeConfigPath, err)
		}
		clientset, err := k8s.NewClientset(config)
		if err != nil {
			slog.Fatalf("could not create k8s clientset from external file: %q: %v", homeConfigPath, err)
		}
		slog.Debugf("Using external Kubernetes configuration from file [%s]", homeConfigPath)

		if !existing {
			// Deplopy the daemonset for kube-vip
			deploy := deployment.Deployment{}
			err = deploy.CreateKVDs(ctx, clientset, t.ImagePath)
			if err != nil {
				slog.Error(err)
			}
		}

		errs := t.StartServiceTest(ctx, clientset)
		os.Exit(len(errs))
	}
}
