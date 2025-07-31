package deployment

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/gookit/slog"
	"github.com/kube-vip/kube-vip/testing/e2e"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

type TestConfig struct {
	SuccessCounter   int
	KindVersionImage string
	ImagePath        string

	ControlPlane bool
	// control plane settings
	Name                string
	ControlPlaneAddress string
	ManifestPath        string
	IPv6                bool

	Services bool
	// service tests
	Simple             bool
	Deployments        bool
	LeaderFailover     bool
	LeaderActive       bool
	LocalDeploy        bool
	DualStack          bool
	Egress             bool
	EgressInternal     bool
	EgressIPv6         bool
	RetainCluster      bool
	SkipHostnameChange bool

	// Deployment config
	Affinity       string
	DeploymentName string
	ServiceName    string
	LeaderName     string

	// Cilium config
	Cilium bool

	// Docker config
	DockerNIC string

	// temp dir root
	TempDirPath string
}

func (config *TestConfig) SimpleDeployment(ctx context.Context, clientset *kubernetes.Clientset) error {

	// Simple Deployment test
	defer func() error { //nolint
		slog.Infof("🧪 ---> simple deployment defer <---")
		tempDirPath, err := os.MkdirTemp(config.TempDirPath, "simple-deployment")
		if err != nil {
			slog.Infof("🧪 ---> simple deployment mkdir err <---: %w", err)
			return err
		}

		if err = e2e.GetLogs(ctx, clientset, tempDirPath); err != nil {
			slog.Infof("🧪 ---> simple deployment getlogs err <---: %w", err)
			return err
		}

		slog.Infof("🧹 deleting Service [%s], deployment [%s]", config.ServiceName, config.DeploymentName)
		err = clientset.CoreV1().Services(v1.NamespaceDefault).Delete(ctx, config.ServiceName, metav1.DeleteOptions{})
		if err != nil {
			slog.Fatal(err)
		}

		if err = deleteDeployment(ctx, clientset, config.DeploymentName); err != nil {
			slog.Fatal(err)
		}
		return nil
	}() //nolint

	var err error
	slog.Infof("🧪 ---> simple deployment <---")
	deploy := Deployment{
		name:         config.DeploymentName,
		nodeAffinity: config.Affinity,
		replicas:     2,
		server:       true,
	}
	err = deploy.CreateDeployment(ctx, clientset)
	if err != nil {
		slog.Fatal(err)
	}
	svc := Service{
		name:     config.ServiceName,
		testHTTP: true,
		timeout:  10,
	}
	_, _, err = svc.CreateService(ctx, clientset)
	if err != nil {
		slog.Error(err)
	} else {
		config.SuccessCounter++
	}

	slog.Infof("🧪 ---> simple deployment end <---")

	return err
}

func (config *TestConfig) MultipleDeployments(ctx context.Context, clientset *kubernetes.Clientset) error {
	// Multiple deployment tests
	var err error

	defer func() error { //nolint
		tempDirPath, err := os.MkdirTemp(config.TempDirPath, "multiple-deployments")
		if err != nil {
			return err
		}

		if err = e2e.GetLogs(ctx, clientset, tempDirPath); err != nil {
			slog.Infof("🧪 ---> simple deployment getlogs err <---: %w", err)
			return err
		}

		for i := 1; i < 5; i++ {
			slog.Infof("🧹 deleting service [%s]", fmt.Sprintf("%s-%d", config.ServiceName, i))
			err = clientset.CoreV1().Services(v1.NamespaceDefault).Delete(ctx, fmt.Sprintf("%s-%d", config.ServiceName, i), metav1.DeleteOptions{})
			if err != nil {
				slog.Fatal(err)
			}
		}

		if err = deleteDeployment(ctx, clientset, config.LeaderName); err != nil {
			slog.Fatal(err)
		}
		return nil
	}() //nolint

	slog.Infof("🧪 ---> multiple deployments <---")
	deploy := Deployment{
		name:         config.LeaderName,
		nodeAffinity: config.Affinity,
		replicas:     2,
		server:       true,
	}
	err = deploy.CreateDeployment(ctx, clientset)
	if err != nil {
		slog.Fatal(err)
	}
	if err != nil {
		slog.Fatal(err)
	}
	for i := 1; i < 5; i++ {
		svc := Service{
			name:     fmt.Sprintf("%s-%d", config.ServiceName, i),
			testHTTP: true,
			timeout:  30,
		}
		_, _, err = svc.CreateService(ctx, clientset)
		if err != nil {
			slog.Fatal(err)
		}
		config.SuccessCounter++
	}

	return err
}
func (config *TestConfig) Failover(ctx context.Context, clientset *kubernetes.Clientset) error {

	var err error
	defer func() error {
		tempDirPath, err := os.MkdirTemp(config.TempDirPath, "failover")
		if err != nil {
			slog.Fatal(err)
		}

		if err = e2e.GetLogs(ctx, clientset, tempDirPath); err != nil {
			slog.Infof("🧪 ---> simple deployment getlogs err <---: %w", err)
			return err
		}

		slog.Infof("🧹 deleting Service [%s], deployment [%s]", config.ServiceName, config.DeploymentName)
		err = clientset.CoreV1().Services(v1.NamespaceDefault).Delete(ctx, config.ServiceName, metav1.DeleteOptions{})
		if err != nil {
			slog.Fatal(err)
		}

		if err = deleteDeployment(ctx, clientset, config.DeploymentName); err != nil {
			slog.Fatal(err)
		}
		return nil
	}() //nolint

	slog.Infof("🧪 ---> leader failover deployment (local policy) <---")

	deploy := Deployment{
		name:         config.DeploymentName,
		nodeAffinity: config.Affinity,
		replicas:     2,
		server:       true,
	}
	err = deploy.CreateDeployment(ctx, clientset)
	if err != nil {
		return err
	}
	svc := Service{
		name:        config.ServiceName,
		egress:      false,
		policyLocal: true,
		testHTTP:    true,
		timeout:     180,
	}
	leader, lbAddresses, err := svc.CreateService(ctx, clientset)
	if err != nil {
		return err
	}
	lbAddress := lbAddresses[0]

	err = leaderFailover(ctx, &config.ServiceName, &leader, clientset)
	if err != nil {
		return err
	}
	config.SuccessCounter++

	// Get all addresses on all nodes
	nodes, err := getAddressesOnNodes()
	if err != nil {
		return err
	}
	// Make sure we don't exist in two places
	err = checkNodesForDuplicateAddresses(nodes, lbAddress)
	if err != nil {
		return err
	}

	return err
}
func (config *TestConfig) ActiveFailover(ctx context.Context, clientset *kubernetes.Clientset) error {
	// pod Failover tests

	var err error
	defer func() error {
		tempDirPath, err := os.MkdirTemp(config.TempDirPath, "active-failover")
		if err != nil {
			slog.Fatal(err)
		}

		if err = e2e.GetLogs(ctx, clientset, tempDirPath); err != nil {
			slog.Infof("🧪 ---> simple deployment getlogs err <---: %w", err)
			return err
		}

		slog.Infof("🧹 deleting Service [%s], deployment [%s]", config.ServiceName, config.DeploymentName)
		err = clientset.CoreV1().Services(v1.NamespaceDefault).Delete(ctx, config.ServiceName, metav1.DeleteOptions{})
		if err != nil {
			slog.Fatal(err)
		}

		if err = deleteDeployment(ctx, clientset, config.DeploymentName); err != nil {
			slog.Fatal(err)
		}
		return nil
	}() //nolint

	slog.Infof("🧪 ---> active pod failover deployment (local policy) <---")
	deploy := Deployment{
		name:         config.DeploymentName,
		nodeAffinity: config.Affinity,
		replicas:     1,
		server:       true,
	}
	err = deploy.CreateDeployment(ctx, clientset)
	if err != nil {
		return err
	}
	svc := Service{
		name:        config.ServiceName,
		policyLocal: true,
		testHTTP:    true,
		timeout:     30,
	}
	leader, _, err := svc.CreateService(ctx, clientset)
	if err != nil {
		return err
	}

	err = podFailover(ctx, &config.ServiceName, &leader, clientset)
	if err != nil {
		return err
	}
	config.SuccessCounter++

	return err
}
func (config *TestConfig) LocalDeployment(ctx context.Context, clientset *kubernetes.Clientset) error {
	// Multiple deployment tests
	var err error
	defer func() error {
		tempDirPath, err := os.MkdirTemp(config.TempDirPath, "local-deployment")
		if err != nil {
			slog.Fatal(err)
		}

		if err = e2e.GetLogs(ctx, clientset, tempDirPath); err != nil {
			slog.Infof("🧪 ---> simple deployment getlogs err <---: %w", err)
			return err
		}

		for i := 1; i < 5; i++ {
			slog.Infof("🧹 deleting service [%s]", fmt.Sprintf("%s-%d", config.ServiceName, i))
			err = clientset.CoreV1().Services(v1.NamespaceDefault).Delete(ctx, fmt.Sprintf("%s-%d", config.ServiceName, i), metav1.DeleteOptions{})
			if err != nil {
				slog.Fatal(err)
			}
		}

		if err = deleteDeployment(ctx, clientset, config.LeaderName); err != nil {
			slog.Fatal(err)
		}
		return nil
	}() //nolint

	slog.Infof("🧪 ---> multiple deployments (local policy) <---")
	timeout := 30
	deploy := Deployment{
		name:         config.LeaderName,
		nodeAffinity: config.Affinity,
		replicas:     2,
		server:       true,
	}
	err = deploy.CreateDeployment(ctx, clientset)
	if err != nil {
		return err
	}
	for i := 1; i < 5; i++ {
		svc := Service{
			policyLocal: true,
			name:        fmt.Sprintf("%s-%d", config.ServiceName, i),
			testHTTP:    true,
			timeout:     timeout,
		}
		_, lbAddresses, err := svc.CreateService(ctx, clientset)
		if err != nil {
			return err
		}
		lbAddress := lbAddresses[0]

		config.SuccessCounter++
		nodes, err := getAddressesOnNodes()
		if err != nil {
			return err
		}
		err = checkNodesForDuplicateAddresses(nodes, lbAddress)
		if err != nil {
			return err
		}
	}

	return nil
}

func (config *TestConfig) EgressDeployment(ctx context.Context, clientset *kubernetes.Clientset, internal bool) error {
	// egress test
	var err error
	defer func() error {
		tempDirPath, err := os.MkdirTemp(config.TempDirPath, "egress-deployment")
		if err != nil {
			slog.Fatal(err)
		}

		if err = e2e.GetLogs(ctx, clientset, tempDirPath); err != nil {
			slog.Infof("🧪 ---> simple deployment getlogs err <---: %w", err)
			return err
		}

		slog.Infof("🧹 deleting Service [%s], deployment [%s]", config.ServiceName, config.DeploymentName)
		err = clientset.CoreV1().Services(v1.NamespaceDefault).Delete(ctx, config.ServiceName, metav1.DeleteOptions{})
		if err != nil {
			slog.Fatal(err)
		}

		if err = deleteDeployment(ctx, clientset, config.DeploymentName); err != nil {
			slog.Fatal(err)
		}
		return nil
	}() //nolint

	slog.Infof("🧪 ---> egress IP re-write (local policy, internal: %t) <---", internal)
	var egress string
	var found bool
	timeout := 30

	deploy := Deployment{
		name:         config.DeploymentName,
		nodeAffinity: config.Affinity,
		replicas:     1,
		client:       true,
	}

	// Find this machines IP address
	addr, _, err := GetLocalIPv4(config.DockerNIC)
	if err != nil {
		return fmt.Errorf("unable to detect local IP address: %w", err)
	}
	deploy.address = addr.String()
	if deploy.address == "" {
		return fmt.Errorf("unable to detect local IP address")
	}
	slog.Infof("📠 found local address [%s]", deploy.address)
	// Create a deployment that connects back to this machines IP address
	err = deploy.CreateDeployment(ctx, clientset)
	if err != nil {
		return err
	}

	svc := Service{
		policyLocal: true,
		name:        config.ServiceName,
		egress:      true,
		testHTTP:    false,
		timeout:     30,
	}

	if internal {
		svc.egressInternal = true
	}

	_, lbAddresses, err := svc.CreateService(ctx, clientset)
	if err != nil {
		return err
	}
	if len(lbAddresses) < 1 {
		return fmt.Errorf("no loadbalancer address found")
	}

	egress = lbAddresses[0]

	found = tcpServer(&egress, timeout, "tcp4")

	if found {
		slog.Infof("🕵️  egress has correct IP address")
		config.SuccessCounter++
	} else {
		return fmt.Errorf("😱 No traffic found from loadbalancer address ")
	}

	return nil
}

func (config *TestConfig) Egressv6Deployment(ctx context.Context, clientset *kubernetes.Clientset, internal bool) error {
	// egress v6 test

	var err error
	defer func() error {
		tempDirPath, err := os.MkdirTemp(config.TempDirPath, "egress-v6-deployment")
		if err != nil {
			slog.Fatal(err)
		}

		if err = e2e.GetLogs(ctx, clientset, tempDirPath); err != nil {
			slog.Infof("🧪 ---> simple deployment getlogs err <---: %w", err)
			return err
		}

		slog.Infof("🧹 deleting Service [%s], deployment [%s]", config.ServiceName, config.DeploymentName)
		err = clientset.CoreV1().Services(v1.NamespaceDefault).Delete(ctx, config.ServiceName, metav1.DeleteOptions{})
		if err != nil {
			slog.Fatal(err)
		}

		if err = deleteDeployment(ctx, clientset, config.DeploymentName); err != nil {
			slog.Fatal(err)
		}
		return nil
	}() //nolint

	slog.Infof("🧪 ---> egress IP re-write IPv6 (local policy, internal: %t) <---", internal)
	var egress string
	var found bool
	timeout := 30

	deploy := Deployment{
		name:         config.DeploymentName,
		nodeAffinity: config.Affinity,
		replicas:     1,
		client:       true,
	}

	// Find this machines IP address
	addr, _, err := GetLocalIPv6(config.DockerNIC)
	if err != nil {
		return fmt.Errorf("unable to detect local IP address: %w", err)
	}
	deploy.address = addr.String()
	if deploy.address == "" {
		return fmt.Errorf("unable to detect local IP address")
	}
	slog.Infof("📠 found local address [%s]", deploy.address)
	// Create a deployment that connects back to this machines IP address
	err = deploy.CreateDeployment(ctx, clientset)
	if err != nil {
		return err
	}

	svc := Service{
		policyLocal:   true,
		name:          config.ServiceName,
		egress:        true,
		egressIPv6:    true,
		timeout:       timeout,
		testDualstack: true,
	}

	if internal {
		svc.egressInternal = true
	}

	_, lbAddresses, err := svc.CreateService(ctx, clientset)
	if err != nil {
		return err
	}
	for x := range lbAddresses {
		ip := net.ParseIP(lbAddresses[x])
		if ip == nil {
			return fmt.Errorf("invalid address")
		}
		if ip.To4() == nil {
			// use brackets for IPv6 address
			egress = lbAddresses[x]
			break
		}
	}

	if egress == "" {
		return fmt.Errorf("no loadbalancer address found")
	}

	found = tcpServer(&egress, timeout, "tcp6")

	if found {
		slog.Infof("🕵️  egress has correct IP address")
		config.SuccessCounter++
	} else {
		return fmt.Errorf("😱 No traffic found from loadbalancer address ")
	}

	return nil
}

func (config *TestConfig) DualStackDeployment(ctx context.Context, clientset *kubernetes.Clientset) error {
	// Dualstack loadbalancer test
	slog.Infof("🧪 ---> testing dualstack loadbalancer service <---")
	deploy := Deployment{
		name:         config.DeploymentName,
		nodeAffinity: config.Affinity,
		replicas:     2,
		server:       true,
	}
	err := deploy.CreateDeployment(ctx, clientset)
	if err != nil {
		return err
	}
	svc := Service{
		name:          config.ServiceName,
		testHTTP:      true,
		testDualstack: true,
		timeout:       30,
	}
	_, _, err = svc.CreateService(ctx, clientset)
	if err != nil {
		return err
	}

	config.SuccessCounter++

	tempDirPath, err := os.MkdirTemp(config.TempDirPath, "dualstack-deployment")
	if err != nil {
		slog.Fatal(err)
	}

	if err = e2e.GetLogs(ctx, clientset, tempDirPath); err != nil {
		slog.Infof("🧪 ---> simple deployment getlogs err <---: %w", err)
		return err
	}

	slog.Infof("🧹 deleting Service [%s], deployment [%s]", config.ServiceName, config.DeploymentName)
	err = clientset.CoreV1().Services(v1.NamespaceDefault).Delete(ctx, config.ServiceName, metav1.DeleteOptions{})
	if err != nil {
		slog.Fatal(err)
	}

	if err = deleteDeployment(ctx, clientset, config.DeploymentName); err != nil {
		slog.Fatal(err)
	}
	return err
}

func deleteDeployment(ctx context.Context, clientset *kubernetes.Clientset, name string) error {
	fgPropagation := metav1.DeletePropagationForeground
	delOpts := metav1.DeleteOptions{
		PropagationPolicy: &fgPropagation,
	}

	slog.Infof("🧹 deleting deployment [%s]", name)
	if err := clientset.AppsV1().Deployments(v1.NamespaceDefault).Delete(ctx, name, delOpts); err != nil {
		return fmt.Errorf("failed to delete deployment %q: %w", name, err)
	}

	slog.Infof("🧹 waiting for the deployment [%s] deletion", name)

	t := time.NewTicker(time.Millisecond * 200)

	checkCtx, checkCancel := context.WithTimeout(ctx, time.Second*30)
	defer checkCancel()

	for {
		select {
		case <-checkCtx.Done():
			t.Stop()
			return fmt.Errorf("failed to check deployment's %q deletion: %w", name, ctx.Err())
		case <-t.C:
			_, err := clientset.AppsV1().Deployments(v1.NamespaceDefault).Get(checkCtx, name, metav1.GetOptions{})
			if err != nil && apierrors.IsNotFound(err) {
				slog.Infof("🧹 deployment [%s] was deleted", name)
				return nil
			} else if err != nil {
				return fmt.Errorf("failed to wait for the deployment %q to be deleted: %w", name, err)
			}
		}
	}
}
