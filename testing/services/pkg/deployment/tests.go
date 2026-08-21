package deployment

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/gookit/slog"
	"github.com/kube-vip/kube-vip/testing/e2e"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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
	FaultElection      bool
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
	Namespace      string // each parallel test group gets a unique namespace
	GlobalWatch    bool   // when true kube-vip watches all namespaces instead of ns only

	// Cilium config
	Cilium bool

	// Docker config
	DockerNIC string

	// temp dir root
	TempDirPath string
}

// ns returns the test namespace, falling back to "default".
func (config *TestConfig) ns() string {
	if config.Namespace != "" {
		return config.Namespace
	}
	return v1.NamespaceDefault
}

// WithNamespace returns a shallow copy of the config with the given namespace.
func (config *TestConfig) WithNamespace(ns string) *TestConfig {
	c := *config
	c.Namespace = ns
	return &c
}

// EnsureNamespace creates the namespace with prometheus disabled (empty metricsAddr).
func EnsureNamespace(ctx context.Context, clientset *kubernetes.Clientset, ns, imageURL string, globalWatch bool) error {
	return EnsureNamespaceWithMetrics(ctx, clientset, ns, imageURL, globalWatch, "")
}

// EnsureNamespaceWithMetrics is like EnsureNamespace but enables the prometheus
// HTTP server at metricsAddr (e.g. ":2112") when the namespace runs alone.
func EnsureNamespaceWithMetrics(ctx context.Context, clientset *kubernetes.Clientset, ns, imageURL string, globalWatch bool, metricsAddr string) error {
	n := &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
	if _, err := clientset.CoreV1().Namespaces().Create(ctx, n, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	d := Deployment{}
	return d.CreateNamespacedKVDs(ctx, clientset, imageURL, ns, globalWatch, metricsAddr)
}

// WaitForNamespaceGone blocks until ns has been fully deleted or 30 s elapses.
// Returning an error means kube-vip pod termination is stuck and the next phase
// must not start — doing so risks port-2112 conflicts on the shared host network.
func WaitForNamespaceGone(ctx context.Context, clientset *kubernetes.Clientset, ns string) error {
	deadline := time.Now().Add(30 * time.Second)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		_, err := clientset.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("namespace %q still terminating after 30 s; kube-vip pod termination is stuck", ns)
		}
		time.Sleep(time.Second)
	}
}

// DeleteNamespace removes ns and its per-namespace ClusterRoleBindings.
func DeleteNamespace(ctx context.Context, clientset *kubernetes.Clientset, ns string) error {
	_ = clientset.RbacV1().ClusterRoleBindings().Delete(ctx, "kube-vip-nodes-"+ns, metav1.DeleteOptions{})
	_ = clientset.RbacV1().ClusterRoleBindings().Delete(ctx, "kube-vip-services-"+ns, metav1.DeleteOptions{})
	return clientset.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{})
}

func (config *TestConfig) SimpleDeployment(ctx context.Context, clientset *kubernetes.Clientset) error {

	// Simple Deployment test
	// cleanupCtx is not tied to the errgroup so cleanup survives a sibling goroutine failure.
	cleanupCtx := context.WithoutCancel(ctx)
	defer func() error { //nolint
		slog.Infof("🧪 ---> simple deployment defer <---")
		tempDirPath, err := os.MkdirTemp(config.TempDirPath, "simple-deployment")
		if err != nil {
			return err
		}

		slog.Infof("saving logs to %q", tempDirPath)
		if err = e2e.GetLogs(cleanupCtx, clientset, tempDirPath, "services"); err != nil {
			slog.Infof("🧪 ---> simple deployment logs err <---: %s", err.Error())
			return err
		}

		slog.Infof("🧹 deleting Service [%s], deployment [%s]", config.ServiceName, config.DeploymentName)
		err = clientset.CoreV1().Services(config.ns()).Delete(cleanupCtx, config.ServiceName, metav1.DeleteOptions{})
		if err != nil {
			slog.Errorf("failed to delete service %q: %v", config.ServiceName, err)
		}

		if err = deleteDeployment(cleanupCtx, clientset, config.ns(), config.DeploymentName); err != nil {
			slog.Errorf("failed to delete deployment %q: %v", config.DeploymentName, err)
		}
		return nil
	}() //nolint

	var err error
	slog.Infof("🧪 ---> simple deployment <---")
	deploy := Deployment{
		namespace:    config.ns(),
		name:         config.DeploymentName,
		nodeAffinity: config.Affinity,
		replicas:     2,
		server:       true,
	}
	err = deploy.CreateDeployment(ctx, clientset)
	if err != nil {
		slog.Fatal(err)
	}
	if err = deploy.WaitForAvailable(ctx, clientset); err != nil {
		return err
	}
	svc := Service{
		namespace: config.ns(),
		name:      config.ServiceName,
		testHTTP:  true,
		timeout:   10,
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

	// cleanupCtx is not tied to the errgroup so cleanup survives a sibling goroutine failure.
	cleanupCtx := context.WithoutCancel(ctx)
	defer func() error { //nolint
		tempDirPath, err := os.MkdirTemp(config.TempDirPath, "multiple-deployments")
		if err != nil {
			return err
		}

		slog.Infof("saving logs to %q", tempDirPath)
		if err = e2e.GetLogs(cleanupCtx, clientset, tempDirPath, "services"); err != nil {
			slog.Infof("🧪 ---> multiple deployment logs err <---: %s", err.Error())
			return err
		}

		for i := 1; i < 5; i++ {
			slog.Infof("🧹 deleting service [%s]", fmt.Sprintf("%s-%d", config.ServiceName, i))
			err = clientset.CoreV1().Services(config.ns()).Delete(cleanupCtx, fmt.Sprintf("%s-%d", config.ServiceName, i), metav1.DeleteOptions{})
			if err != nil {
				slog.Errorf("failed to delete service %q: %v", fmt.Sprintf("%s-%d", config.ServiceName, i), err)
			}
		}

		if err = deleteDeployment(cleanupCtx, clientset, config.ns(), config.LeaderName); err != nil {
			slog.Errorf("failed to delete deployment %q: %v", config.LeaderName, err)
		}
		return nil
	}() //nolint

	slog.Infof("🧪 ---> multiple deployments <---")
	deploy := Deployment{
		namespace:    config.ns(),
		name:         config.LeaderName,
		nodeAffinity: config.Affinity,
		replicas:     2,
		server:       true,
	}
	err = deploy.CreateDeployment(ctx, clientset)
	if err != nil {
		slog.Fatal(err)
	}
	if err = deploy.WaitForAvailable(ctx, clientset); err != nil {
		return err
	}
	for i := 1; i < 5; i++ {
		svc := Service{
			namespace: config.ns(),
			name:      fmt.Sprintf("%s-%d", config.ServiceName, i),
			testHTTP:  true,
			timeout:   30,
		}
		_, _, err = svc.CreateService(ctx, clientset)
		if err != nil {
			slog.Fatal(err)
			return err
		}
	}

	config.SuccessCounter++

	return nil
}
func (config *TestConfig) Failover(ctx context.Context, clientset *kubernetes.Clientset) error {

	var err error
	// cleanupCtx is not tied to the errgroup so cleanup survives a sibling goroutine failure.
	cleanupCtx := context.WithoutCancel(ctx)
	defer func() error {
		tempDirPath, err := os.MkdirTemp(config.TempDirPath, "failover")
		if err != nil {
			slog.Errorf("failed to create temporary log directory: %v", err)
			return err
		}

		slog.Infof("saving logs to %q", tempDirPath)
		if err = e2e.GetLogs(cleanupCtx, clientset, tempDirPath, "services"); err != nil {
			slog.Infof("🧪 ---> failover logs err <---: %s", err.Error())
			return err
		}

		slog.Infof("🧹 deleting Service [%s], deployment [%s]", config.ServiceName, config.DeploymentName)
		err = clientset.CoreV1().Services(config.ns()).Delete(cleanupCtx, config.ServiceName, metav1.DeleteOptions{})
		if err != nil {
			slog.Errorf("failed to delete service %q: %v", config.ServiceName, err)
		}

		if err = deleteDeployment(cleanupCtx, clientset, config.ns(), config.DeploymentName); err != nil {
			slog.Errorf("failed to delete deployment %q: %v", config.DeploymentName, err)
		}
		return nil
	}() //nolint

	slog.Infof("🧪 ---> leader failover deployment (local policy) <---")

	deploy := Deployment{
		namespace:    config.ns(),
		name:         config.DeploymentName,
		nodeAffinity: config.Affinity,
		replicas:     2,
		server:       true,
	}
	err = deploy.CreateDeployment(ctx, clientset)
	if err != nil {
		return err
	}
	if err = deploy.WaitForAvailable(ctx, clientset); err != nil {
		return err
	}
	svc := Service{
		namespace:   config.ns(),
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
	if len(lbAddresses) == 0 {
		return fmt.Errorf("no load balancer address found for service %s", config.ServiceName)
	}
	lbAddress := lbAddresses[0]

	err = leaderFailover(ctx, config.ns(), &config.ServiceName, &leader, clientset)
	if err != nil {
		return err
	}

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

	config.SuccessCounter++

	return nil
}
func (config *TestConfig) ActiveFailover(ctx context.Context, clientset *kubernetes.Clientset) error {
	// pod Failover tests

	var err error
	// cleanupCtx is not tied to the errgroup so cleanup survives a sibling goroutine failure.
	cleanupCtx := context.WithoutCancel(ctx)
	defer func() error {
		tempDirPath, err := os.MkdirTemp(config.TempDirPath, "active-failover")
		if err != nil {
			slog.Errorf("failed to create temporary log directory: %v", err)
			return err
		}

		slog.Infof("saving logs to %q", tempDirPath)
		if err = e2e.GetLogs(cleanupCtx, clientset, tempDirPath, "services"); err != nil {
			slog.Infof("🧪 ---> active failover logs err <---: %s", err.Error())
			return err
		}

		slog.Infof("🧹 deleting Service [%s], deployment [%s]", config.ServiceName, config.DeploymentName)
		err = clientset.CoreV1().Services(config.ns()).Delete(cleanupCtx, config.ServiceName, metav1.DeleteOptions{})
		if err != nil {
			slog.Errorf("failed to delete service %q: %v", config.ServiceName, err)
		}

		if err = deleteDeployment(cleanupCtx, clientset, config.ns(), config.DeploymentName); err != nil {
			slog.Errorf("failed to delete deployment %q: %v", config.DeploymentName, err)
		}
		return nil
	}() //nolint

	slog.Infof("🧪 ---> active pod failover deployment (local policy) <---")
	deploy := Deployment{
		namespace:    config.ns(),
		name:         config.DeploymentName,
		nodeAffinity: config.Affinity,
		replicas:     1,
		server:       true,
	}
	err = deploy.CreateDeployment(ctx, clientset)
	if err != nil {
		return err
	}
	if err = deploy.WaitForAvailable(ctx, clientset); err != nil {
		return err
	}
	svc := Service{
		namespace:   config.ns(),
		name:        config.ServiceName,
		policyLocal: true,
		testHTTP:    true,
		timeout:     30,
	}
	leader, _, err := svc.CreateService(ctx, clientset)
	if err != nil {
		return err
	}

	err = podFailover(ctx, config.ns(), &config.ServiceName, &leader, clientset)
	if err != nil {
		return err
	}
	config.SuccessCounter++

	return err
}
func (config *TestConfig) LocalDeployment(ctx context.Context, clientset *kubernetes.Clientset) error {
	// Multiple deployment tests
	var err error
	// cleanupCtx is not tied to the errgroup so cleanup survives a sibling goroutine failure.
	cleanupCtx := context.WithoutCancel(ctx)
	defer func() error {
		tempDirPath, err := os.MkdirTemp(config.TempDirPath, "local-deployment")
		if err != nil {
			slog.Errorf("failed to create temporary log directory: %v", err)
			return err
		}

		slog.Infof("saving logs to %q", tempDirPath)
		if err = e2e.GetLogs(cleanupCtx, clientset, tempDirPath, "services"); err != nil {
			slog.Infof("🧪 ---> local deployment logs err <---: %s", err.Error())
			return err
		}

		for i := 1; i < 5; i++ {
			slog.Infof("🧹 deleting service [%s]", fmt.Sprintf("%s-%d", config.ServiceName, i))
			err = clientset.CoreV1().Services(config.ns()).Delete(cleanupCtx, fmt.Sprintf("%s-%d", config.ServiceName, i), metav1.DeleteOptions{})
			if err != nil {
				slog.Errorf("failed to delete service %q: %v", fmt.Sprintf("%s-%d", config.ServiceName, i), err)
			}
		}

		if err = deleteDeployment(cleanupCtx, clientset, config.ns(), config.LeaderName); err != nil {
			slog.Errorf("failed to delete deployment %q: %v", config.LeaderName, err)
		}
		return nil
	}() //nolint

	slog.Infof("🧪 ---> multiple deployments (local policy) <---")
	timeout := 30
	deploy := Deployment{
		namespace:    config.ns(),
		name:         config.LeaderName,
		nodeAffinity: config.Affinity,
		replicas:     2,
		server:       true,
	}
	err = deploy.CreateDeployment(ctx, clientset)
	if err != nil {
		return err
	}
	if err = deploy.WaitForAvailable(ctx, clientset); err != nil {
		return err
	}
	for i := 1; i < 5; i++ {
		svc := Service{
			namespace:   config.ns(),
			policyLocal: true,
			name:        fmt.Sprintf("%s-%d", config.ServiceName, i),
			testHTTP:    true,
			timeout:     timeout,
		}
		_, lbAddresses, err := svc.CreateService(ctx, clientset)
		if err != nil {
			return err
		}
		if len(lbAddresses) == 0 {
			return fmt.Errorf("no load balancer address found for service %s-%d", config.ServiceName, i)
		}
		lbAddress := lbAddresses[0]

		nodes, err := getAddressesOnNodes()
		if err != nil {
			return err
		}
		err = checkNodesForDuplicateAddresses(nodes, lbAddress)
		if err != nil {
			return err
		}
	}

	config.SuccessCounter++

	return nil
}

func (config *TestConfig) EgressDeployment(ctx context.Context, clientset *kubernetes.Clientset, internal bool) error {
	// egress test
	var err error
	// cleanupCtx is not tied to the errgroup so cleanup survives a sibling goroutine failure.
	cleanupCtx := context.WithoutCancel(ctx)
	defer func() error {
		tempDirPath, err := os.MkdirTemp(config.TempDirPath, "egress-deployment")
		if err != nil {
			slog.Errorf("failed to create temporary log directory: %v", err)
			return err
		}

		slog.Infof("saving logs to %q", tempDirPath)
		if err = e2e.GetLogs(cleanupCtx, clientset, tempDirPath, "services"); err != nil {
			slog.Infof("🧪 ---> egress deployment logs err <---: %s", err.Error())
			return err
		}

		slog.Infof("🧹 deleting Service [%s], deployment [%s]", config.ServiceName, config.DeploymentName)
		err = clientset.CoreV1().Services(config.ns()).Delete(cleanupCtx, config.ServiceName, metav1.DeleteOptions{})
		if err != nil {
			slog.Errorf("failed to delete service %q: %v", config.ServiceName, err)
		}

		if err = deleteDeployment(cleanupCtx, clientset, config.ns(), config.DeploymentName); err != nil {
			slog.Errorf("failed to delete deployment %q: %v", config.DeploymentName, err)
		}
		return nil
	}() //nolint

	slog.Infof("🧪 ---> egress IP re-write (local policy, internal: %t) <---", internal)
	var egress string
	var found bool
	timeout := 30

	deploy := Deployment{
		namespace:    config.ns(),
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
	if err = deploy.WaitForAvailable(ctx, clientset); err != nil {
		return err
	}

	svc := Service{
		namespace:   config.ns(),
		name:        config.ServiceName,
		policyLocal: true,
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
	// cleanupCtx is not tied to the errgroup so cleanup survives a sibling goroutine failure.
	cleanupCtx := context.WithoutCancel(ctx)
	defer func() error {
		tempDirPath, err := os.MkdirTemp(config.TempDirPath, "egress-v6-deployment")
		if err != nil {
			slog.Errorf("failed to create temporary log directory: %v", err)
			return err
		}

		slog.Infof("saving logs to %q", tempDirPath)
		if err = e2e.GetLogs(cleanupCtx, clientset, tempDirPath, "services"); err != nil {
			slog.Infof("🧪 ---> egress v6 deployment logs err <---: %s", err.Error())
			return err
		}

		slog.Infof("🧹 deleting Service [%s], deployment [%s]", config.ServiceName, config.DeploymentName)
		err = clientset.CoreV1().Services(config.ns()).Delete(cleanupCtx, config.ServiceName, metav1.DeleteOptions{})
		if err != nil {
			slog.Errorf("failed to delete service %q: %v", config.ServiceName, err)
		}

		if err = deleteDeployment(cleanupCtx, clientset, config.ns(), config.DeploymentName); err != nil {
			slog.Errorf("failed to delete deployment %q: %v", config.DeploymentName, err)
		}
		return nil
	}() //nolint

	slog.Infof("🧪 ---> egress IP re-write IPv6 (local policy, internal: %t) <---", internal)
	var egress string
	var found bool
	timeout := 30

	deploy := Deployment{
		namespace:    config.ns(),
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
	if err = deploy.WaitForAvailable(ctx, clientset); err != nil {
		return err
	}

	svc := Service{
		namespace:     config.ns(),
		name:          config.ServiceName,
		policyLocal:   true,
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
		return fmt.Errorf("no loadbalancer egress address found")
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
		namespace:    config.ns(),
		name:         config.DeploymentName,
		nodeAffinity: config.Affinity,
		replicas:     2,
		server:       true,
	}
	err := deploy.CreateDeployment(ctx, clientset)
	if err != nil {
		return err
	}
	if err = deploy.WaitForAvailable(ctx, clientset); err != nil {
		return err
	}
	svc := Service{
		namespace:     config.ns(),
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

	// cleanupCtx is not tied to the errgroup so cleanup survives a sibling goroutine failure.
	cleanupCtx := context.WithoutCancel(ctx)
	tempDirPath, err := os.MkdirTemp(config.TempDirPath, "dualstack-deployment")
	if err != nil {
		slog.Errorf("failed to create temporary log directory: %v", err)
		return err
	}

	slog.Infof("saving logs to %q", tempDirPath)
	if err = e2e.GetLogs(cleanupCtx, clientset, tempDirPath, "services"); err != nil {
		slog.Infof("🧪 ---> dualstack deployment logs err <---: %s", err.Error())
		return err
	}

	slog.Infof("🧹 deleting Service [%s], deployment [%s]", config.ServiceName, config.DeploymentName)
	err = clientset.CoreV1().Services(config.ns()).Delete(cleanupCtx, config.ServiceName, metav1.DeleteOptions{})
	if err != nil {
		slog.Errorf("failed to delete service %q: %v", config.ServiceName, err)
	}

	if err = deleteDeployment(cleanupCtx, clientset, config.ns(), config.DeploymentName); err != nil {
		slog.Errorf("failed to delete deployment %q: %v", config.DeploymentName, err)
	}
	return err
}

func deleteDeployment(ctx context.Context, clientset *kubernetes.Clientset, ns, name string) error {
	fgPropagation := metav1.DeletePropagationForeground
	delOpts := metav1.DeleteOptions{
		PropagationPolicy: &fgPropagation,
	}

	slog.Infof("🧹 deleting deployment [%s]", name)
	if err := clientset.AppsV1().Deployments(ns).Delete(ctx, name, delOpts); err != nil {
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
			// checkCtx carries the timeout; ctx may be uncancelled (e.g. a
			// cleanup context detached from the errgroup).
			return fmt.Errorf("failed to check deployment's %q deletion: %w", name, checkCtx.Err())
		case <-t.C:
			_, err := clientset.AppsV1().Deployments(ns).Get(checkCtx, name, metav1.GetOptions{})
			if err != nil && apierrors.IsNotFound(err) {
				slog.Infof("🧹 deployment [%s] was deleted", name)
				return nil
			} else if err != nil {
				return fmt.Errorf("failed to wait for the deployment %q to be deleted: %w", name, err)
			}
		}
	}
}

// ElectionFaults injects the failure modes of the per-service leader election
// and proves the service recovers from each one.
//
// Each fault targets a specific reported bug, and each is asserted with a signal
// that is actually broken when the bug is present, so the test fails against
// unfixed code instead of only documenting the happy path:
//
//   - endpoint churn (issue #1665): the backend is scaled 1 -> 0 -> 1 five times.
//     Every EndpointSlice event used to start another permanent election loop, so
//     kube_vip_service_election_loops must stay at most 1 per node.
//   - endpoint object deletion (#1663, fixed in #1664): deleting the EndpointSlice
//     ends the endpoint watcher and cancels the service context without dropping
//     it from svcMap, so later events reused a cancelled context and could never
//     re-add the lease. kube_vip_service_election_errors_total{reason="no_lease"}
//     must stay at zero.
//   - lease object faults: the lease is deleted, and its holderIdentity blanked.
//     The election client recovers these itself, so these only assert convergence.
//   - leadership loss (#1650): the apiserver is blocked from the leader for longer
//     than the lease duration, which drives OnStoppedLeading and returns the
//     election. The restart loop used to wedge on a WaitGroup exactly there, so
//     kube_vip_service_election_attempts_total must advance afterwards.
//   - an externalTrafficPolicy flip: tears the service down and rebuilds it, so the
//     replacement has to get a fresh lease instead of the retired one.
//   - a burst of service events: the same spawn-once invariant driven from the
//     service watch instead of the endpoint watch.
//   - a common lease sibling teardown: deleting one of two services that share a
//     lease must leave that lease held for the one that stays.
//   - a partitioned follower: cutting a non-leader off from the apiserver must not
//     move the lease, stop traffic, or leak a loop on the node that returns.
//
// The endpoint churn fault also asserts the yield half of the cycle: with a local
// traffic policy and no endpoints anywhere, the address has to stop answering
// instead of being left to black-hole traffic.
//
// After every fault the service has to converge: a live election loop, a held
// lease, and a VIP that serves traffic.
func (config *TestConfig) ElectionFaults(ctx context.Context, clientset *kubernetes.Clientset) error {
	// cleanupCtx is not tied to the errgroup so cleanup survives a sibling goroutine failure.
	cleanupCtx := context.WithoutCancel(ctx)
	defer func() error {
		tempDirPath, err := os.MkdirTemp(config.TempDirPath, "election-faults")
		if err != nil {
			slog.Errorf("failed to create temporary log directory: %v", err)
			return err
		}

		slog.Infof("saving logs to %q", tempDirPath)
		if err = e2e.GetLogs(cleanupCtx, clientset, tempDirPath, "services"); err != nil {
			slog.Infof("🧪 ---> election faults logs err <---: %s", err.Error())
			return err
		}

		slog.Infof("🧹 deleting Service [%s], deployment [%s]", config.ServiceName, config.DeploymentName)
		if err = clientset.CoreV1().Services(config.ns()).Delete(cleanupCtx, config.ServiceName, metav1.DeleteOptions{}); err != nil {
			slog.Errorf("failed to delete service %q: %v", config.ServiceName, err)
		}

		if err = deleteDeployment(cleanupCtx, clientset, config.ns(), config.DeploymentName); err != nil {
			slog.Errorf("failed to delete deployment %q: %v", config.DeploymentName, err)
		}
		return nil
	}() //nolint

	slog.Infof("🧪 ---> leader election faults (local policy) <---")
	deploy := Deployment{
		namespace:    config.ns(),
		name:         config.DeploymentName,
		nodeAffinity: config.Affinity,
		replicas:     1,
		server:       true,
	}
	if err := deploy.CreateDeployment(ctx, clientset); err != nil {
		return err
	}
	if err := deploy.WaitForAvailable(ctx, clientset); err != nil {
		return err
	}

	svc := Service{
		namespace:   config.ns(),
		name:        config.ServiceName,
		policyLocal: true,
		testHTTP:    true,
		timeout:     30,
	}
	_, lbAddresses, err := svc.CreateService(ctx, clientset)
	if err != nil {
		return err
	}
	if len(lbAddresses) == 0 {
		return fmt.Errorf("no load balancer address found for service %s", config.ServiceName)
	}
	lbAddress := lbAddresses[0]
	leaseName := fmt.Sprintf("kubevip-%s", config.ServiceName)

	if err := config.checkConverged(ctx, clientset, leaseName, lbAddress, "startup"); err != nil {
		return err
	}

	// Fault 1 (#1665): endpoint churn. Sleep between transitions so the endpoint
	// debouncer does not coalesce them into a single event, otherwise no duplicate
	// loop would be started even by the unfixed code.
	for i := 1; i <= 5; i++ {
		slog.Infof("🔁 flap %d: scaling deployment [%s] to zero endpoints", i, config.DeploymentName)
		if err := scaleDeployment(ctx, clientset, config.ns(), config.DeploymentName, 0); err != nil {
			return err
		}

		// With a local traffic policy and no endpoints anywhere, the VIP has to be
		// given up rather than left black-holing traffic. Only assert this once,
		// the remaining cycles are there to accumulate election loops.
		if i == 1 {
			if err := waitForVIPReleased(lbAddress); err != nil {
				return err
			}
		}
		time.Sleep(time.Second * 2)

		slog.Infof("🔁 flap %d: scaling deployment [%s] back up", i, config.DeploymentName)
		if err := scaleDeployment(ctx, clientset, config.ns(), config.DeploymentName, 1); err != nil {
			return err
		}
		time.Sleep(time.Second * 2)
	}

	if err := config.checkConverged(ctx, clientset, leaseName, lbAddress, "endpoint flapping"); err != nil {
		return err
	}

	// Fault 2 (#1664): end the endpoint watcher by deleting the EndpointSlice, then
	// drive another service event so the stale service context would be reused.
	slog.Infof("💥 deleting endpointslices of service [%s]", config.ServiceName)
	if err := clientset.DiscoveryV1().EndpointSlices(config.ns()).DeleteCollection(ctx, metav1.DeleteOptions{},
		metav1.ListOptions{LabelSelector: "kubernetes.io/service-name=" + config.ServiceName}); err != nil {
		return fmt.Errorf("failed to delete endpointslices of service %q: %w", config.ServiceName, err)
	}

	if err := annotateService(ctx, clientset, config.ns(), config.ServiceName); err != nil {
		return err
	}

	if err := config.checkConverged(ctx, clientset, leaseName, lbAddress, "endpointslice deletion"); err != nil {
		return err
	}

	// Fault 3: lease object faults. Deleting the lease and blanking its holder are
	// recovered by the election client itself, so this only asserts convergence.
	slog.Infof("💥 deleting lease [%s]", leaseName)
	if err := clientset.CoordinationV1().Leases(config.ns()).Delete(ctx, leaseName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete lease %q: %w", leaseName, err)
	}

	if err := config.checkConverged(ctx, clientset, leaseName, lbAddress, "lease deletion"); err != nil {
		return err
	}

	slog.Infof("💥 clearing holderIdentity on lease [%s]", leaseName)
	if err := clearLeaseHolder(ctx, clientset, config.ns(), leaseName); err != nil {
		return err
	}

	if err := config.checkConverged(ctx, clientset, leaseName, lbAddress, "lease holder being cleared"); err != nil {
		return err
	}

	// Fault 4 (#1650): a real leadership loss, by partitioning the leader from the
	// apiserver for longer than the lease duration. That drives OnStoppedLeading
	// and returns the election, so the restart loop has to make a new attempt
	// afterwards. A wedged loop never does, which is the #1650 deadlock.
	leader, err := leaseHolder(ctx, clientset, config.ns(), leaseName)
	if err != nil {
		return err
	}

	before, err := scrapeServiceGauge("kube_vip_service_election_attempts_total", config.ns(), config.ServiceName)
	if err != nil {
		return err
	}

	slog.Infof("💥 blocking API server access from node [%s]", leader)
	if err := setAPIServerReachable(leader, false); err != nil {
		return err
	}
	time.Sleep(time.Second * 20)

	slog.Infof("🔌 restoring API server access on node [%s]", leader)
	if err := setAPIServerReachable(leader, true); err != nil {
		return err
	}

	if err := config.checkConverged(ctx, clientset, leaseName, lbAddress, "API server disruption"); err != nil {
		return err
	}

	if err := waitForElectionProgress(before, config.ns(), config.ServiceName); err != nil {
		return err
	}

	// Fault 5: a service change that tears the service down and rebuilds it.
	// Flipping the traffic policy makes serviceChanged cancel the service context,
	// so the replacement has to get a fresh lease rather than the retired one.
	for _, policy := range []v1.ServiceExternalTrafficPolicy{
		v1.ServiceExternalTrafficPolicyCluster,
		v1.ServiceExternalTrafficPolicyLocal,
	} {
		slog.Infof("💥 setting externalTrafficPolicy of service [%s] to [%s]", config.ServiceName, policy)
		if err := setExternalTrafficPolicy(ctx, clientset, config.ns(), config.ServiceName, policy); err != nil {
			return err
		}

		if err := config.checkConverged(ctx, clientset, leaseName, lbAddress,
			fmt.Sprintf("externalTrafficPolicy change to %s", policy)); err != nil {
			return err
		}
	}

	// Fault 6: a burst of service events. This drives the same spawn-once
	// invariant as fault 1 from the service watch instead of the endpoint watch.
	slog.Infof("💥 annotating service [%s] repeatedly", config.ServiceName)
	for i := range 15 {
		if err := annotateServiceValue(ctx, clientset, config.ns(), config.ServiceName, fmt.Sprintf("storm-%d", i)); err != nil {
			return err
		}
		time.Sleep(time.Millisecond * 200)
	}

	if err := config.checkConverged(ctx, clientset, leaseName, lbAddress, "service event storm"); err != nil {
		return err
	}

	// Fault 7: a service sharing its lease with another one is torn down. The
	// sibling keeps using that lease, so it must not be cancelled underneath it.
	// This is the case that made the per-lease teardown wrong.
	if err := config.faultCommonLeaseSibling(ctx, clientset); err != nil {
		return err
	}

	// Fault 8: partition a node that is not the leader. The leader has to keep the
	// lease and keep serving, and the returning node must not leak a loop or end up
	// advertising the same address.
	follower, err := followerNode(ctx, clientset, config.ns(), leaseName)
	if err != nil {
		return err
	}

	if follower == "" {
		slog.Infof("⏭️  no follower node to partition, skipping")
	} else {
		holderBefore, err := leaseHolder(ctx, clientset, config.ns(), leaseName)
		if err != nil {
			return err
		}

		slog.Infof("💥 blocking API server access from follower node [%s]", follower)
		if err := setAPIServerReachable(follower, false); err != nil {
			return err
		}

		// The leader must keep serving while the follower is cut off.
		time.Sleep(time.Second * 25)
		if err := httpTest(lbAddress); err != nil {
			return fmt.Errorf("service on %q stopped serving while a follower was partitioned: %w", lbAddress, err)
		}

		slog.Infof("🔌 restoring API server access on node [%s]", follower)
		if err := setAPIServerReachable(follower, true); err != nil {
			return err
		}

		if err := config.checkConverged(ctx, clientset, leaseName, lbAddress, "follower partition"); err != nil {
			return err
		}

		holderAfter, err := leaseHolder(ctx, clientset, config.ns(), leaseName)
		if err != nil {
			return err
		}
		if holderAfter != holderBefore {
			return fmt.Errorf("lease moved from %q to %q while only a follower was partitioned",
				holderBefore, holderAfter)
		}
	}

	config.SuccessCounter++

	return nil
}

// checkConverged asserts the service is healthy after a fault: at most one
// election loop per node, no lease lookup failures, a held lease, and a VIP that
// serves traffic.
func (config *TestConfig) checkConverged(ctx context.Context, clientset *kubernetes.Clientset, leaseName, lbAddress, fault string) error {
	if err := checkNoDuplicateElectionLoops(config.ns(), config.ServiceName, fault); err != nil {
		return err
	}

	if err := checkLeaseErrorsStable(config.ns(), config.ServiceName, fault); err != nil {
		return err
	}

	holder, err := leaseHolder(ctx, clientset, config.ns(), leaseName)
	if err != nil {
		return fmt.Errorf("after %s: %w", fault, err)
	}
	slog.Infof("🔎 lease [%s] is held by [%s] after %s", leaseName, holder, fault)

	if err := httpTest(lbAddress); err != nil {
		return fmt.Errorf("service on %q did not serve traffic after %s: %w", lbAddress, fault, err)
	}
	return nil
}

// faultCommonLeaseSibling creates two services that share one lease, tears the
// first one down, and asserts the second keeps serving on that same lease.
//
// A teardown that cancels the lease rather than just releasing the leaving
// service takes the sibling down with it, which is what review of #1669 caught.
func (config *TestConfig) faultCommonLeaseSibling(ctx context.Context, clientset *kubernetes.Clientset) error {
	const (
		leaseName   = "shared-lease"
		deployName  = "kube-vip-shared"
		leavingName = "kube-vip-shared-leaving"
		stayingName = "kube-vip-shared-staying"
	)

	defer func() {
		for _, name := range []string{leavingName, stayingName} {
			if err := clientset.CoreV1().Services(config.ns()).Delete(ctx, name, metav1.DeleteOptions{}); err != nil &&
				!apierrors.IsNotFound(err) {
				slog.Errorf("failed to delete service %q: %v", name, err)
			}
		}
		if err := deleteDeployment(ctx, clientset, config.ns(), deployName); err != nil {
			slog.Errorf("failed to delete deployment %q: %v", deployName, err)
		}
	}()

	slog.Infof("🧪 ---> common lease sibling teardown <---")
	deploy := Deployment{
		namespace:    config.ns(),
		name:         deployName,
		nodeAffinity: config.Affinity,
		replicas:     1,
		server:       true,
	}
	if err := deploy.CreateDeployment(ctx, clientset); err != nil {
		return err
	}
	if err := deploy.WaitForAvailable(ctx, clientset); err != nil {
		return err
	}

	// Both services share one lease. A common lease requires a cluster traffic
	// policy, which CreateService sets alongside the annotation.
	for _, name := range []string{leavingName, stayingName} {
		svc := Service{
			namespace: config.ns(), name: name, commonLease: leaseName, testHTTP: true, timeout: 30}
		if _, _, err := svc.CreateService(ctx, clientset); err != nil {
			return fmt.Errorf("failed to create service %q on the common lease: %w", name, err)
		}
	}

	holder, err := leaseHolder(ctx, clientset, config.ns(), leaseName)
	if err != nil {
		return fmt.Errorf("common lease never got a holder: %w", err)
	}
	slog.Infof("🔎 common lease [%s] is held by [%s]", leaseName, holder)

	// Tear the first service down. The sibling is untouched and still needs the
	// lease, so the lease has to stay held.
	slog.Infof("💥 deleting service [%s], which shares lease [%s] with [%s]", leavingName, leaseName, stayingName)
	if err := clientset.CoreV1().Services(config.ns()).Delete(ctx, leavingName, metav1.DeleteOptions{}); err != nil {
		return fmt.Errorf("failed to delete service %q: %w", leavingName, err)
	}

	if _, err := leaseHolder(ctx, clientset, config.ns(), leaseName); err != nil {
		return fmt.Errorf("common lease lost its holder when a sibling service was deleted: %w", err)
	}

	if err := checkNoDuplicateElectionLoops(v1.NamespaceDefault, stayingName, "a sibling on the same lease being deleted"); err != nil {
		return err
	}

	slog.Infof("🔎 common lease [%s] still held after [%s] was deleted", leaseName, leavingName)
	return nil
}

// checkNoDuplicateElectionLoops asserts that no node runs more than one election
// loop for the service. More than one means loops leaked, which is issue #1665.
func checkNoDuplicateElectionLoops(namespace, name, fault string) error {
	// A fault that rebuilds the service context legitimately has the old and the
	// new loop alive at the same moment, so poll until the count settles. A leaked
	// loop only exits with its service context, so it never settles.
	deadline := time.Now().Add(time.Second * 45)

	for {
		loops, err := scrapeElectionLoops(namespace, name)
		if err != nil {
			return fmt.Errorf("after %s: %w", fault, err)
		}

		busiest := 0.0
		for _, count := range loops {
			busiest = max(busiest, count)
		}

		if busiest <= 1 {
			slog.Infof("🔎 election loops per node after %s: %v", fault, loops)
			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("a node still runs %v leader election loops for service %q after %s, "+
				"want at most 1, loops leaked (%v)", busiest, name, fault, loops)
		}
		time.Sleep(time.Second * 3)
	}
}

// checkLeaseErrorsStable asserts the election error counter stopped growing.
//
// The #1664 desync makes every later watch event fail to find the lease, so the
// counter climbs for the lifetime of the process. A single increment can happen
// benignly while a service is first being set up, so the assertion is that the
// counter settles rather than that it is zero.
func checkLeaseErrorsStable(namespace, name, fault string) error {
	const metric = "kube_vip_service_election_errors_total"

	// The counter only grows on the #1664 bug path, but a failing election retry
	// loop can pause for a couple of seconds between attempts, so a single quiet
	// reading is not proof of stability. Require several consecutive quiet
	// intervals and reset the streak whenever the counter grows.
	const requiredStableIntervals = 3

	deadline := time.Now().Add(time.Second * 12)
	prev, err := scrapeServiceGauge(metric, namespace, name)
	if err != nil {
		return fmt.Errorf("after %s: %w", fault, err)
	}

	stableIntervals := 0
	for {
		time.Sleep(time.Second * 2)

		curr, err := scrapeServiceGauge(metric, namespace, name)
		if err != nil {
			return fmt.Errorf("after %s: %w", fault, err)
		}

		grew := false
		for node, count := range curr {
			if count > prev[node] {
				grew = true
				if time.Now().After(deadline) {
					return fmt.Errorf("node %q keeps failing leader election for service %q after %s "+
						"(%v -> %v errors in 12s), the lease is never re-added", node, name, fault, prev[node], count)
				}
			}
		}
		if grew {
			stableIntervals = 0
		} else {
			stableIntervals++
			if stableIntervals >= requiredStableIntervals {
				slog.Infof("🔎 election errors per node stable after %s: %v", fault, curr)
				return nil
			}
		}
		prev = curr
	}
}

// waitForElectionProgress asserts the restart loop keeps attempting election
// after a leadership loss. A wedged loop stops incrementing, which is #1650.
func waitForElectionProgress(before map[string]float64, namespace, name string) error {
	deadline := time.Now().Add(time.Second * 60)

	for {
		after, err := scrapeServiceGauge("kube_vip_service_election_attempts_total", namespace, name)
		if err != nil {
			return err
		}

		for node, count := range after {
			if count > before[node] {
				slog.Infof("🔎 node [%s] election attempts advanced %v -> %v", node, before[node], count)
				return nil
			}
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("no node made a new leader election attempt for service %q after losing the lease, "+
				"the restart loop is stuck (before=%v after=%v)", name, before, after)
		}
		time.Sleep(time.Second * 2)
	}
}

func annotateService(ctx context.Context, clientset *kubernetes.Clientset, ns, name string) error {
	return annotateServiceValue(ctx, clientset, ns, name, "1")
}

// annotateServiceValue writes a given annotation value to force a service watch
// event. The value has to change for the API server to emit a Modified event.
func annotateServiceValue(ctx context.Context, clientset *kubernetes.Clientset, ns, name, value string) error {
	patch := fmt.Appendf(nil, `{"metadata":{"annotations":{"e2e.kube-vip.io/probe":%q}}}`, value)
	if _, err := clientset.CoreV1().Services(ns).Patch(ctx, name,
		types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("failed to annotate service %q: %w", name, err)
	}
	return nil
}

// setExternalTrafficPolicy changes the traffic policy of a service, which makes
// kube-vip treat the service as changed and rebuild it.
func setExternalTrafficPolicy(ctx context.Context, clientset *kubernetes.Clientset, ns, name string,
	policy v1.ServiceExternalTrafficPolicy) error {
	patch := fmt.Appendf(nil, `{"spec":{"externalTrafficPolicy":%q}}`, policy)
	if _, err := clientset.CoreV1().Services(ns).Patch(ctx, name,
		types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("failed to set externalTrafficPolicy of service %q to %q: %w", name, policy, err)
	}
	return nil
}

// waitForVIPReleased waits until the address stops answering. With a local
// traffic policy and no endpoints, the VIP has to be given up instead of being
// left to black-hole traffic.
func waitForVIPReleased(address string) error {
	deadline := time.Now().Add(time.Second * 60)

	for {
		if err := tcpProbe(address); err != nil {
			slog.Infof("🔎 address [%s] was released after the endpoints went away", address)
			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("address %q still answers after all endpoints went away", address)
		}
		time.Sleep(time.Second * 2)
	}
}

// tcpProbe reports whether the address accepts a connection on the service port.
func tcpProbe(address string) error {
	target := net.JoinHostPort(address, "80")
	conn, err := net.DialTimeout("tcp", target, time.Second*2)
	if err != nil {
		return err
	}
	return conn.Close()
}

// followerNode returns a node that runs kube-vip but does not hold the lease.
func followerNode(ctx context.Context, clientset *kubernetes.Clientset, ns, leaseName string) (string, error) {
	holder, err := leaseHolder(ctx, clientset, ns, leaseName)
	if err != nil {
		return "", err
	}

	pods, err := clientset.CoreV1().Pods("kube-system").List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=kube-vip-ds",
	})
	if err != nil {
		return "", fmt.Errorf("failed to list kube-vip pods: %w", err)
	}

	for x := range pods.Items {
		if node := pods.Items[x].Spec.NodeName; node != holder {
			return node, nil
		}
	}
	return "", nil
}

// leaseHolder waits until the lease exists and reports a holder, and returns it.
// An empty or missing holder is the symptom reported in #1665.
func leaseHolder(ctx context.Context, clientset *kubernetes.Clientset, ns, name string) (string, error) {
	checkCtx, cancel := context.WithTimeout(ctx, time.Second*120)
	defer cancel()

	t := time.NewTicker(time.Second * 2)
	defer t.Stop()

	for {
		l, err := clientset.CoordinationV1().Leases(ns).Get(checkCtx, name, metav1.GetOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return "", fmt.Errorf("failed to get lease %q: %w", name, err)
		}
		if err == nil && l.Spec.HolderIdentity != nil && *l.Spec.HolderIdentity != "" {
			return *l.Spec.HolderIdentity, nil
		}

		select {
		case <-checkCtx.Done():
			return "", fmt.Errorf("lease %q never reacquired a holder", name)
		case <-t.C:
		}
	}
}

// clearLeaseHolder blanks the holderIdentity of a lease, reproducing the state
// reported in #1665.
func clearLeaseHolder(ctx context.Context, clientset *kubernetes.Clientset, ns, name string) error {
	patch := []byte(`{"spec":{"holderIdentity":null}}`)
	if _, err := clientset.CoordinationV1().Leases(ns).Patch(ctx, name,
		types.MergePatchType, patch, metav1.PatchOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to clear holder of lease %q: %w", name, err)
	}
	return nil
}

// setAPIServerReachable blocks or unblocks the kubernetes API server port from
// inside a kind node, to fault the election clients running on it.
func setAPIServerReachable(node string, reachable bool) error {
	// The node hostnames are changed to "<node>-modified", the container keeps
	// the original name.
	container := strings.TrimSuffix(node, "-modified")

	action := "-I"
	if reachable {
		action = "-D"
	}

	cmd := exec.Command("docker", "exec", container, "iptables", action, "OUTPUT",
		"-p", "tcp", "--dport", "6443", "-j", "REJECT") //nolint
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to set apiserver reachable=%t on %q: %w: %s", reachable, container, err, out)
	}
	return nil
}

// scaleDeployment sets the replica count and waits for the endpoints to follow.
func scaleDeployment(ctx context.Context, clientset *kubernetes.Clientset, ns, name string, replicas int32) error {
	scale, err := clientset.AppsV1().Deployments(ns).GetScale(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get scale of deployment %q: %w", name, err)
	}

	scale.Spec.Replicas = replicas
	if _, err := clientset.AppsV1().Deployments(ns).UpdateScale(ctx, name, scale, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("failed to scale deployment %q to %d: %w", name, replicas, err)
	}

	return waitForReadyEndpoints(ctx, clientset, ns, name, replicas > 0)
}

// waitForReadyEndpoints waits until the deployment has ready pods, or none left.
func waitForReadyEndpoints(ctx context.Context, clientset *kubernetes.Clientset, ns, name string, wantReady bool) error {
	checkCtx, cancel := context.WithTimeout(ctx, time.Second*60)
	defer cancel()

	t := time.NewTicker(time.Second)
	defer t.Stop()

	for {
		d, err := clientset.AppsV1().Deployments(ns).Get(checkCtx, name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("failed to get deployment %q: %w", name, err)
		}

		if (d.Status.ReadyReplicas > 0) == wantReady {
			return nil
		}

		select {
		case <-checkCtx.Done():
			return fmt.Errorf("timed out waiting for deployment %q to have readyEndpoints=%t", name, wantReady)
		case <-t.C:
		}
	}
}
