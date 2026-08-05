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
	FlapEndpoints      bool
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
			return err
		}

		slog.Infof("saving logs to %q", tempDirPath)
		if err = e2e.GetLogs(ctx, clientset, tempDirPath, "services"); err != nil {
			slog.Infof("🧪 ---> simple deployment logs err <---: %s", err.Error())
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

		slog.Infof("saving logs to %q", tempDirPath)
		if err = e2e.GetLogs(ctx, clientset, tempDirPath, "services"); err != nil {
			slog.Infof("🧪 ---> multiple deployment logs err <---: %s", err.Error())
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
			return err
		}
	}

	config.SuccessCounter++

	return nil
}
func (config *TestConfig) Failover(ctx context.Context, clientset *kubernetes.Clientset) error {

	var err error
	defer func() error {
		tempDirPath, err := os.MkdirTemp(config.TempDirPath, "failover")
		if err != nil {
			slog.Fatal(err)
		}

		slog.Infof("saving logs to %q", tempDirPath)
		if err = e2e.GetLogs(ctx, clientset, tempDirPath, "services"); err != nil {
			slog.Infof("🧪 ---> failover logs err <---: %s", err.Error())
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
	if len(lbAddresses) == 0 {
		return fmt.Errorf("no load balancer address found for service %s", config.ServiceName)
	}
	lbAddress := lbAddresses[0]

	err = leaderFailover(ctx, &config.ServiceName, &leader, clientset)
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
	defer func() error {
		tempDirPath, err := os.MkdirTemp(config.TempDirPath, "active-failover")
		if err != nil {
			slog.Fatal(err)
		}

		slog.Infof("saving logs to %q", tempDirPath)
		if err = e2e.GetLogs(ctx, clientset, tempDirPath, "services"); err != nil {
			slog.Infof("🧪 ---> active failover logs err <---: %s", err.Error())
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

		slog.Infof("saving logs to %q", tempDirPath)
		if err = e2e.GetLogs(ctx, clientset, tempDirPath, "services"); err != nil {
			slog.Infof("🧪 ---> local deployment logs err <---: %s", err.Error())
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
	defer func() error {
		tempDirPath, err := os.MkdirTemp(config.TempDirPath, "egress-deployment")
		if err != nil {
			slog.Fatal(err)
		}

		slog.Infof("saving logs to %q", tempDirPath)
		if err = e2e.GetLogs(ctx, clientset, tempDirPath, "services"); err != nil {
			slog.Infof("🧪 ---> egress deployment logs err <---: %s", err.Error())
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

		slog.Infof("saving logs to %q", tempDirPath)
		if err = e2e.GetLogs(ctx, clientset, tempDirPath, "services"); err != nil {
			slog.Infof("🧪 ---> egress v6 deployment logs err <---: %s", err.Error())
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

	slog.Infof("saving logs to %q", tempDirPath)
	if err = e2e.GetLogs(ctx, clientset, tempDirPath, "services"); err != nil {
		slog.Infof("🧪 ---> dualstack deployment logs err <---: %s", err.Error())
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

// EndpointFlap exercises the per-service leader election of #1665 under faults.
//
// Three faults are injected in sequence against a svc_election service:
//
//  1. endpoint churn: the backend is scaled 1 -> 0 -> 1 repeatedly, which is
//     what drove the duplicate election loops reported in the issue.
//  2. lease faults: the service Lease is deleted, and then blanked by clearing
//     its holderIdentity, which is the exact state observed in the report.
//  3. API server faults: the apiserver is made unreachable from the leader for
//     a while, so every election client on that node loses its backend.
//
// After each fault the service has to converge again: the Lease is held by a
// node and the VIP serves traffic. Those are properties of the feature rather
// than of the current implementation, so the test stays meaningful if the
// election internals change. Duplicate election loops are not observable from
// outside the process; that part is pinned by the unit test in
// pkg/endpoints/endpoints_test.go.
func (config *TestConfig) EndpointFlap(ctx context.Context, clientset *kubernetes.Clientset) error {
	defer func() error {
		tempDirPath, err := os.MkdirTemp(config.TempDirPath, "endpoint-flap")
		if err != nil {
			slog.Fatal(err)
		}

		slog.Infof("saving logs to %q", tempDirPath)
		if err = e2e.GetLogs(ctx, clientset, tempDirPath, "services"); err != nil {
			slog.Infof("🧪 ---> endpoint flap logs err <---: %s", err.Error())
			return err
		}

		slog.Infof("🧹 deleting Service [%s], deployment [%s]", config.ServiceName, config.DeploymentName)
		if err = clientset.CoreV1().Services(v1.NamespaceDefault).Delete(ctx, config.ServiceName, metav1.DeleteOptions{}); err != nil {
			slog.Fatal(err)
		}

		if err = deleteDeployment(ctx, clientset, config.DeploymentName); err != nil {
			slog.Fatal(err)
		}
		return nil
	}() //nolint

	slog.Infof("🧪 ---> endpoint and lease faults (local policy) <---")
	deploy := Deployment{
		name:         config.DeploymentName,
		nodeAffinity: config.Affinity,
		replicas:     1,
		server:       true,
	}
	if err := deploy.CreateDeployment(ctx, clientset); err != nil {
		return err
	}

	svc := Service{
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

	// Fault 1: endpoint churn. Each cycle is a zero-endpoint teardown followed by
	// the endpoint becoming healthy again, which is what accumulated duplicate
	// election loops before the fix.
	for i := 1; i <= 5; i++ {
		slog.Infof("🔁 flap %d: scaling deployment [%s] to zero endpoints", i, config.DeploymentName)
		if err := scaleDeployment(ctx, clientset, config.DeploymentName, 0); err != nil {
			return err
		}

		slog.Infof("🔁 flap %d: scaling deployment [%s] back up", i, config.DeploymentName)
		if err := scaleDeployment(ctx, clientset, config.DeploymentName, 1); err != nil {
			return err
		}
	}

	if err := checkServiceConverged(ctx, clientset, leaseName, lbAddress, "endpoint flapping"); err != nil {
		return err
	}

	// Fault 2a: delete the lease outright and make sure it is recreated and held.
	slog.Infof("💥 deleting lease [%s]", leaseName)
	if err := clientset.CoordinationV1().Leases(v1.NamespaceDefault).Delete(ctx, leaseName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete lease %q: %w", leaseName, err)
	}

	if err := checkServiceConverged(ctx, clientset, leaseName, lbAddress, "lease deletion"); err != nil {
		return err
	}

	// Fault 2b: blank the holder. This is the state reported in #1665, where the
	// lease existed with holderIdentity "" and never got a holder back.
	slog.Infof("💥 clearing holderIdentity on lease [%s]", leaseName)
	if err := clearLeaseHolder(ctx, clientset, leaseName); err != nil {
		return err
	}

	if err := checkServiceConverged(ctx, clientset, leaseName, lbAddress, "lease holder being cleared"); err != nil {
		return err
	}

	// Fault 3: cut the leader off from the API server for longer than the lease
	// duration, so its election client loses leadership, then restore it.
	leader, err := leaseHolder(ctx, clientset, leaseName)
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

	if err := checkServiceConverged(ctx, clientset, leaseName, lbAddress, "API server disruption"); err != nil {
		return err
	}

	config.SuccessCounter++

	return nil
}

// checkServiceConverged asserts the service is healthy again after a fault: the
// lease is held by a node and the VIP serves traffic.
func checkServiceConverged(ctx context.Context, clientset *kubernetes.Clientset, leaseName, lbAddress, fault string) error {
	holder, err := leaseHolder(ctx, clientset, leaseName)
	if err != nil {
		return fmt.Errorf("after %s: %w", fault, err)
	}
	slog.Infof("🔎 lease [%s] is held by [%s] after %s", leaseName, holder, fault)

	if err := httpTest(lbAddress); err != nil {
		return fmt.Errorf("service on %q did not serve traffic after %s: %w", lbAddress, fault, err)
	}
	return nil
}

// leaseHolder waits until the lease exists and reports a holder, and returns it.
// An empty or missing holder is the symptom reported in #1665.
func leaseHolder(ctx context.Context, clientset *kubernetes.Clientset, name string) (string, error) {
	checkCtx, cancel := context.WithTimeout(ctx, time.Second*120)
	defer cancel()

	t := time.NewTicker(time.Second)
	defer t.Stop()

	for {
		l, err := clientset.CoordinationV1().Leases(v1.NamespaceDefault).Get(checkCtx, name, metav1.GetOptions{})
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
func clearLeaseHolder(ctx context.Context, clientset *kubernetes.Clientset, name string) error {
	patch := []byte(`{"spec":{"holderIdentity":null}}`)
	if _, err := clientset.CoordinationV1().Leases(v1.NamespaceDefault).Patch(ctx, name,
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

	// kind nodes reach the API server on the control plane's 6443.
	cmd := exec.Command("docker", "exec", container, "iptables", action, "OUTPUT",
		"-p", "tcp", "--dport", "6443", "-j", "REJECT") //nolint
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to set apiserver reachable=%t on %q: %w: %s", reachable, container, err, out)
	}
	return nil
}

// scaleDeployment sets the replica count and waits for the endpoints to follow.
func scaleDeployment(ctx context.Context, clientset *kubernetes.Clientset, name string, replicas int32) error {
	scale, err := clientset.AppsV1().Deployments(v1.NamespaceDefault).GetScale(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get scale of deployment %q: %w", name, err)
	}

	scale.Spec.Replicas = replicas
	if _, err := clientset.AppsV1().Deployments(v1.NamespaceDefault).UpdateScale(ctx, name, scale, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("failed to scale deployment %q to %d: %w", name, replicas, err)
	}

	return waitForReadyEndpoints(ctx, clientset, name, replicas > 0)
}

// waitForReadyEndpoints waits until the deployment has ready pods, or none left.
func waitForReadyEndpoints(ctx context.Context, clientset *kubernetes.Clientset, name string, wantReady bool) error {
	checkCtx, cancel := context.WithTimeout(ctx, time.Second*60)
	defer cancel()

	t := time.NewTicker(time.Second)
	defer t.Stop()

	for {
		d, err := clientset.AppsV1().Deployments(v1.NamespaceDefault).Get(checkCtx, name, metav1.GetOptions{})
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
