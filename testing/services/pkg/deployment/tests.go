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

// EndpointFlap reproduces https://github.com/kube-vip/kube-vip/issues/1665.
//
// With svc_election enabled, every EndpointSlice event used to start another
// permanent leader-election restart loop for the same service. Scaling the
// backend 1 -> 0 -> 1 repeatedly therefore accumulated duplicate loops that all
// contended on the same lease, and the service could end up with a lease that
// never reacquired a holder.
//
// The test asserts two things after the flapping: the VIP still serves traffic,
// and the lease has a holder again.
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

	slog.Infof("🧪 ---> endpoint flapping (local policy) <---")
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

	// Take the endpoints away and bring them back a few times. Each cycle is a
	// zero-endpoint teardown followed by the endpoint becoming healthy again.
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

	// The VIP has to be served again once the endpoint is healthy.
	if err := httpTest(lbAddress); err != nil {
		return fmt.Errorf("service %q did not recover after endpoint flapping: %w", config.ServiceName, err)
	}

	// A lease without a holder is the symptom reported in issue #1665.
	if err := waitForLeaseHolder(ctx, clientset, fmt.Sprintf("kubevip-%s", config.ServiceName)); err != nil {
		return err
	}

	config.SuccessCounter++

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

// waitForLeaseHolder waits until the service lease reports a holder again.
func waitForLeaseHolder(ctx context.Context, clientset *kubernetes.Clientset, name string) error {
	checkCtx, cancel := context.WithTimeout(ctx, time.Second*60)
	defer cancel()

	t := time.NewTicker(time.Second)
	defer t.Stop()

	for {
		l, err := clientset.CoordinationV1().Leases(v1.NamespaceDefault).Get(checkCtx, name, metav1.GetOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to get lease %q: %w", name, err)
		}
		if err == nil && l.Spec.HolderIdentity != nil && *l.Spec.HolderIdentity != "" {
			slog.Infof("🔎 lease [%s] is held by [%s]", name, *l.Spec.HolderIdentity)
			return nil
		}

		select {
		case <-checkCtx.Done():
			return fmt.Errorf("lease %q never reacquired a holder after endpoint flapping", name)
		case <-t.C:
		}
	}
}
