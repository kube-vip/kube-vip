package main

import (
	"context"
	"flag"
	"os"
	"path/filepath"

	"github.com/kube-vip/kube-vip/pkg/k8s"
	log "github.com/sirupsen/logrus"
)

type testConfig struct {
	successCounter int

	ImagePath string

	ControlPlane bool
	// control plane settings
	Name                string
	ControlPlaneAddress string
	ManifestPath        string
	IPv6                bool
	Dualstack           bool

	Services bool
	// service tests
	ignoreSimple         bool
	ignoreDeployments    bool
	ignoreLeaderFailover bool
	ignoreLeaderActive   bool
	ignoreLocalDeploy    bool
	ignoreDualStack      bool
	ignoreEgress         bool
	ignoreEgressIPv6     bool
	retainCluster        bool
	skipHostnameChange   bool
}

func main() {
	var t testConfig

	t.ImagePath = os.Getenv("E2E_IMAGE_PATH")

	_, t.ignoreSimple = os.LookupEnv("IGNORE_SIMPLE")
	_, t.ignoreDeployments = os.LookupEnv("IGNORE_DEPLOY")
	_, t.ignoreLeaderFailover = os.LookupEnv("IGNORE_LEADER")
	_, t.ignoreLeaderActive = os.LookupEnv("IGNORE_ACTIVE")
	_, t.ignoreLocalDeploy = os.LookupEnv("IGNORE_LOCALDEPLOY")
	_, t.ignoreEgress = os.LookupEnv("IGNORE_EGRESS")
	_, t.ignoreEgressIPv6 = os.LookupEnv("IGNORE_EGRESSIPV6")
	_, t.ignoreDualStack = os.LookupEnv("IGNORE_DUALSTACK")
	_, t.retainCluster = os.LookupEnv("RETAIN_CLUSTER")
	_, t.IPv6 = os.LookupEnv("IPV6_FAMILY")
	_, t.skipHostnameChange = os.LookupEnv("SKIP_HOSTNAME_CHANGE")

	flag.StringVar(&t.ImagePath, "imagepath", "plndr/kube-vip:action", "")
	flag.BoolVar(&t.ControlPlane, "ControlPlane", false, "")
	flag.BoolVar(&t.Services, "Services", false, "")

	flag.Parse()

	log.Infof("🔬 beginning e2e tests, image: [%s]", t.ImagePath)

	if !t.ignoreDualStack {
		t.Dualstack = true
	}

	if t.ControlPlane {
		err := t.createKind()
		if !t.retainCluster {
			if err != nil {
				log.Fatal(err)
			}
			defer func() {
				err := deleteKind()
				if err != nil {
					log.Fatal(err)
				}
			}()
		} else {
			if err != nil {
				log.Warn(err)
			}
		}
		// ctx, cancel := context.WithCancel(context.TODO())
		// defer cancel()
		// homeConfigPath := filepath.Join(os.Getenv("HOME"), ".kube", "config")
		// clientset, err := k8s.NewClientset(homeConfigPath, false, "")
		// if err != nil {
		// 	log.Fatalf("could not create k8s clientset from external file: %q: %v", homeConfigPath, err)
		// }
		// log.Debugf("Using external Kubernetes configuration from file [%s]", homeConfigPath)
		// err = t.startTest(ctx, clientset)
		// if err != nil {
		// 	log.Fatal(err)
		// }
	}

	if t.Services {
		err := t.createKind()
		if !t.retainCluster {
			if err != nil {
				log.Fatal(err)
			}
			defer func() {
				err := deleteKind()
				if err != nil {
					log.Fatal(err)
				}
			}()
		} else {
			if err != nil {
				log.Warn(err)
			}
		}

		ctx, cancel := context.WithCancel(context.TODO())
		defer cancel()
		homeConfigPath := filepath.Join(os.Getenv("HOME"), ".kube", "config")
		clientset, err := k8s.NewClientset(homeConfigPath, false, "")
		if err != nil {
			log.Fatalf("could not create k8s clientset from external file: %q: %v", homeConfigPath, err)
		}
		log.Debugf("Using external Kubernetes configuration from file [%s]", homeConfigPath)

		// Deplopy the daemonset for kube-vip
		deploy := deployment{}
		err = deploy.createKVDs(ctx, clientset, t.ImagePath)
		if err != nil {
			log.Error(err)
		}
		t.startServiceTest(ctx, clientset)
	}

}
