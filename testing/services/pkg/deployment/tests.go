package deployment

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/gookit/slog"
	v1 "k8s.io/api/core/v1"
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
}

func (config *TestConfig) SimpleDeployment(ctx context.Context, clientset *kubernetes.Clientset) error {
	// Simple Deployment test
	var err error
	defer func() error { //nolint

		slog.Infof("🧹 deleting Service [%s], deployment [%s]", config.ServiceName, config.DeploymentName)
		err = clientset.CoreV1().Services(v1.NamespaceDefault).Delete(ctx, config.ServiceName, metav1.DeleteOptions{})
		if err != nil {
			slog.Fatal(err)
		}
		err = clientset.AppsV1().Deployments(v1.NamespaceDefault).Delete(ctx, config.DeploymentName, metav1.DeleteOptions{})
		if err != nil {
			slog.Fatal(err)
		}
		return nil
	}() //nolint

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

	return err
}

func (config *TestConfig) MultipleDeployments(ctx context.Context, clientset *kubernetes.Clientset) error {
	// Multiple deployment tests
	var err error

	defer func() error { //nolint
		for i := 1; i < 5; i++ {
			slog.Infof("🧹 deleting service [%s]", fmt.Sprintf("%s-%d", config.ServiceName, i))
			err = clientset.CoreV1().Services(v1.NamespaceDefault).Delete(ctx, fmt.Sprintf("%s-%d", config.ServiceName, i), metav1.DeleteOptions{})
			if err != nil {
				slog.Fatal(err)
			}
		}
		slog.Infof("🧹 deleting deployment [%s]", config.DeploymentName)
		err = clientset.AppsV1().Deployments(v1.NamespaceDefault).Delete(ctx, config.LeaderName, metav1.DeleteOptions{})
		if err != nil {
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
		slog.Infof("🧹 deleting Service [%s], deployment [%s]", config.ServiceName, config.DeploymentName)
		err = clientset.CoreV1().Services(v1.NamespaceDefault).Delete(ctx, config.ServiceName, metav1.DeleteOptions{})
		if err != nil {
			return err
		}

		err = clientset.AppsV1().Deployments(v1.NamespaceDefault).Delete(ctx, config.DeploymentName, metav1.DeleteOptions{})
		if err != nil {
			return err
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
		slog.Infof("🧹 deleting Service [%s], deployment [%s]", config.ServiceName, config.DeploymentName)
		err = clientset.CoreV1().Services(v1.NamespaceDefault).Delete(ctx, config.ServiceName, metav1.DeleteOptions{})
		if err != nil {
			return err
		}
		err = clientset.AppsV1().Deployments(v1.NamespaceDefault).Delete(ctx, config.DeploymentName, metav1.DeleteOptions{})
		if err != nil {
			return err
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
		for i := 1; i < 5; i++ {
			slog.Infof("🧹 deleting service [%s]", fmt.Sprintf("%s-%d", config.ServiceName, i))
			err = clientset.CoreV1().Services(v1.NamespaceDefault).Delete(ctx, fmt.Sprintf("%s-%d", config.ServiceName, i), metav1.DeleteOptions{})
			if err != nil {
				return err
			}
		}
		slog.Infof("🧹 deleting deployment [%s]", config.DeploymentName)
		err := clientset.AppsV1().Deployments(v1.NamespaceDefault).Delete(ctx, config.LeaderName, metav1.DeleteOptions{})
		if err != nil {
			return err
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
	// pod Failover tests

	var err error
	defer func() error {
		slog.Infof("🧹 deleting Service [%s], deployment [%s]", config.ServiceName, config.DeploymentName)
		err = clientset.CoreV1().Services(v1.NamespaceDefault).Delete(ctx, config.ServiceName, metav1.DeleteOptions{})
		if err != nil {
			return err
		}
		err = clientset.AppsV1().Deployments(v1.NamespaceDefault).Delete(ctx, config.DeploymentName, metav1.DeleteOptions{})
		if err != nil {
			return err
		}
		return nil
	}() //nolint

	slog.Infof("🧪 ---> egress IP re-write (local policy, internal: %t) <---", internal)
	var egress string
	var found bool
	timeout := 30
	// Set up a local listener
	go func() {
		found = tcpServer(false, &egress, timeout)
	}()

	deploy := Deployment{
		name:         config.DeploymentName,
		nodeAffinity: config.Affinity,
		replicas:     1,
		client:       true,
	}

	// Find this machines IP address
	deploy.address = GetLocalIPv4()
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
	egress = lbAddresses[0]

	for i := 1; i < 15; i++ {
		if found {
			slog.Infof("🕵️  egress has correct IP address")
			config.SuccessCounter++
			break
		}
		time.Sleep(time.Second * 1)
	}

	if !found {
		return fmt.Errorf("😱 No traffic found from loadbalancer address ")
	}

	return err
}

func (config *TestConfig) Egressv6Deployment(ctx context.Context, clientset *kubernetes.Clientset) error {
	// pod Failover tests

	var err error
	defer func() error {
		slog.Infof("🧹 deleting Service [%s], deployment [%s]", config.ServiceName, config.DeploymentName)
		err = clientset.CoreV1().Services(v1.NamespaceDefault).Delete(ctx, config.ServiceName, metav1.DeleteOptions{})
		if err != nil {
			return err
		}
		err = clientset.AppsV1().Deployments(v1.NamespaceDefault).Delete(ctx, config.DeploymentName, metav1.DeleteOptions{})
		if err != nil {
			return err
		}
		return nil
	}() //nolint

	slog.Infof("🧪 ---> egress IP re-write IPv6 (local policy) <---")
	var egress string
	var found bool
	timeout := 60
	// Set up a local listener
	go func() {
		found = tcpServer(true, &egress, timeout)
	}()

	deploy := Deployment{
		name:         config.DeploymentName,
		nodeAffinity: config.Affinity,
		replicas:     1,
		client:       true,
	}

	// Find this machines IP address
	deploy.address = GetLocalIPv6()
	if deploy.address == "" {
		slog.Fatalf("Unable to detect local IP address")
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
		testHTTP:      false,
		timeout:       timeout,
		testDualstack: true,
	}

	_, lbAddresses, err := svc.CreateService(ctx, clientset)
	if err != nil {
		return err
	}
	for x := range lbAddresses {
		ip := net.ParseIP(lbAddresses[x])
		// if ip == nil {
		// 	return errors.New("invalid address")
		// }
		if ip.To4() == nil {
			// use brackets for IPv6 address
			egress = lbAddresses[x]
			break
		}
	}

	for i := 1; i < timeout; i++ {
		if found {
			slog.Infof("🕵️  egress has correct IP address")
			config.SuccessCounter++
			break
		}
		time.Sleep(time.Second * 1)
	}

	if !found {
		slog.Error("😱 No traffic found from loadbalancer address ")
	}

	return err
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

	slog.Infof("🧹 deleting Service [%s], deployment [%s]", config.ServiceName, config.DeploymentName)
	err = clientset.CoreV1().Services(v1.NamespaceDefault).Delete(ctx, config.ServiceName, metav1.DeleteOptions{})
	if err != nil {
		return err
	}
	err = clientset.AppsV1().Deployments(v1.NamespaceDefault).Delete(ctx, config.DeploymentName, metav1.DeleteOptions{})
	if err != nil {
		return err
	}
	return err
}
