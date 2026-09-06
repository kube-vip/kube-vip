//go:build e2e
// +build e2e

package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	kindconfigv1alpha4 "sigs.k8s.io/kind/pkg/apis/config/v1alpha4"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/utils"
	"github.com/kube-vip/kube-vip/testing/e2e"
)

const (
	serviceFaultClusterNodeCount       = 3
	serviceFaultNamespace              = "default"
	serviceFaultBackendName            = "fault-svc-backend"
	serviceFaultGlobalLeaseName        = "plndr-svcs-lock"
	serviceFaultReconcileMetric        = "kube_vip_service_reconcile_errors_total"
	serviceFaultActiveMetric           = "kube_vip_active_services"
	serviceFaultLeaderMetric           = "kube_vip_is_leader"
	serviceFaultElectionAttemptsMetric = "kube_vip_service_election_attempts_total"
	serviceFaultSyntheticAnnotation    = "development.kube-vip.io/synthetic-api-server-error-on-update"
	serviceFaultDeleteFinalizer        = "faults.kube-vip.io/delete-in-progress"
	serviceFaultNftTable               = "kube_vip_v4"
	serviceFaultPollInterval           = time.Second
	serviceFaultMetricGap              = time.Second
	serviceFaultConvergenceTimeout     = 120 * time.Second
	serviceFaultReconcileDeltaLimit    = 4.0
	serviceFaultRouteTable             = "198"
)

type serviceFaultSuite struct {
	ctx     context.Context
	cancel  context.CancelFunc
	cluster *e2e.Cluster
	client  kubernetes.Interface
	nodes   []string
	tempDir string
	created map[string]struct{}
	metrics bool
}

var _ = Describe("kube-vip service election and lifecycle faults", Label("faults"), Serial, Ordered, func() {
	if Mode != ModeARP && Mode != ModeRT {
		return
	}

	suite := &serviceFaultSuite{}

	BeforeAll(func() {
		suite.ctx, suite.cancel = context.WithCancel(context.Background())
		suite.created = make(map[string]struct{})

		var err error
		suite.tempDir, err = os.MkdirTemp("", fmt.Sprintf("kube-vip-faults-svc-%s-", Mode))
		Expect(err).NotTo(HaveOccurred())

		offset := SOffset.Get()
		controlPlaneVIP := e2e.GenerateVIP(utils.IPv4Family, offset, defaultNetwork)
		manifest := e2e.KubevipManifestValues{
			ControlPlaneVIP:            controlPlaneVIP,
			ImagePath:                  os.Getenv("E2E_IMAGE_PATH"),
			ConfigPath:                 os.Getenv("CONFIG_PATH"),
			ControlPlaneEnable:         "true",
			SvcEnable:                  "true",
			SvcElectionEnable:          "false",
			EnableEndpoints:            "true",
			EnableNodeLabeling:         "false",
			EnableServiceSecurity:      "true",
			PerServiceElectionOnDemand: "true",
			PrometheusHTTPServer:       ":2112",
		}
		templateName := "kube-vip.yaml.tmpl"
		networking := kindconfigv1alpha4.Networking{IPFamily: kindconfigv1alpha4.IPv4Family}
		if Mode == ModeRT {
			templateName = "kube-vip-routing-table.yaml.tmpl"
			manifest.VipElectionEnable = "true"
		}

		suite.cluster = e2e.CreateCluster(suite.ctx, &e2e.ClusterSpec{
			Name:         fmt.Sprintf("kube-vip-faults-svc-%s-%d", Mode, offset),
			Nodes:        serviceFaultClusterNodeCount,
			Networking:   networking,
			KubeVip:      manifest,
			Logger:       e2e.TestLogger{},
			ConfigMtx:    ConfigMtx,
			TemplateName: templateName,
		})
		suite.client = buildServiceFaultClient(suite.cluster.RestCfg)
		suite.cluster.Client = suite.client

		for _, node := range suite.cluster.Nodes {
			suite.nodes = append(suite.nodes, node.String())
		}
		Expect(suite.nodes).To(HaveLen(serviceFaultClusterNodeCount))
		suite.metrics = suite.metricsAvailable()

		By(withTimestamp("creating the service fault backend on every node"))
		createDS(suite.ctx, serviceFaultBackendName, serviceFaultNamespace, suite.client, 80)
	})

	AfterEach(func() {
		suite.cleanupServices()
	})

	AfterAll(func() {
		if suite.cluster != nil {
			suite.cluster.SaveLogs(suite.ctx, suite.tempDir)
			suite.cluster.Delete()
		}
		if suite.cancel != nil {
			suite.cancel()
		}
		if suite.tempDir != "" && os.Getenv("E2E_KEEP_LOGS") != "true" {
			Expect(os.RemoveAll(suite.tempDir)).To(Succeed())
		}
	})

	It("keeps a per-service VIP serving through a holder API blackhole and heal", func() {
		globalName := suite.uniqueName("fault-global")
		globalVIP := e2e.GenerateVIP(utils.IPv4Family, SOffset.Get(), defaultNetwork)
		perServiceName := suite.uniqueName("fault-per-service")
		perServiceVIP := e2e.GenerateVIP(utils.IPv4Family, SOffset.Get(), defaultNetwork)

		suite.createService(globalName, globalVIP, false, false, false, corev1.ServiceExternalTrafficPolicyTypeCluster, "")
		suite.createService(perServiceName, perServiceVIP, true, false, false, corev1.ServiceExternalTrafficPolicyTypeCluster, "")
		suite.waitForServiceReady(globalName, globalVIP)
		suite.waitForServiceReady(perServiceName, perServiceVIP)
		suite.assertServiceVIP(globalVIP)
		suite.assertServiceVIP(perServiceVIP)

		suite.waitForLeaseHolder("kube-system", serviceFaultGlobalLeaseName)
		suite.assertExactlyOneServiceLeader(serviceFaultGlobalLeaseName)
		leaseName := serviceFaultLeaseName(perServiceName)
		suite.assertExactlyOneServiceLeader(leaseName)
		activeBefore := suite.activeServicesSnapshot()
		reconcileBefore := suite.reconcileErrorSnapshot(perServiceName)
		oldHolder := suite.waitForLeaseHolder(serviceFaultNamespace, leaseName)

		By(withTimestamp(fmt.Sprintf("blackholing the API server from per-service holder %q", oldHolder)))
		Expect(e2e.BlackholeAPIServer(suite.cluster.Name, oldHolder)).To(Succeed())
		DeferCleanup(func() {
			Expect(e2e.RestoreAPIServer(suite.cluster.Name, oldHolder)).To(Succeed())
		})

		newHolder := suite.waitForDifferentLeaseHolder(serviceFaultNamespace, leaseName, oldHolder)
		suite.assertServiceVIP(perServiceVIP)
		suite.assertExactlyOneServiceLeader(leaseName)
		By(withTimestamp(fmt.Sprintf("restoring the API server connection on %q", oldHolder)))
		Expect(e2e.RestoreAPIServer(suite.cluster.Name, oldHolder)).To(Succeed())

		suite.waitForMetrics(oldHolder)
		suite.waitForLeaseHolder(serviceFaultNamespace, leaseName)
		suite.assertExactlyOneServiceLeader(leaseName)
		suite.assertServiceVIP(perServiceVIP)
		suite.assertActiveServicesStable(activeBefore, "service holder API blackhole and heal")
		suite.assertReconcileErrorsStable(reconcileBefore, perServiceName, "service holder API blackhole and heal")
		By(withTimestamp(fmt.Sprintf("per-service holder moved from %q to %q and recovered", oldHolder, newHolder)))
	})

	It("removes a service VIP and converges active services when its leader is killed during deletion", func() {
		remainingName := suite.uniqueName("fault-remaining")
		remainingVIP := e2e.GenerateVIP(utils.IPv4Family, SOffset.Get(), defaultNetwork)
		victimName := suite.uniqueName("fault-delete")
		victimVIP := e2e.GenerateVIP(utils.IPv4Family, SOffset.Get(), defaultNetwork)

		suite.createService(remainingName, remainingVIP, false, false, false, corev1.ServiceExternalTrafficPolicyTypeCluster, "")
		suite.createService(victimName, victimVIP, true, false, true, corev1.ServiceExternalTrafficPolicyTypeCluster, "")
		suite.waitForServiceReady(remainingName, remainingVIP)
		suite.waitForServiceReady(victimName, victimVIP)
		suite.assertServiceVIP(remainingVIP)
		suite.assertServiceVIP(victimVIP)
		suite.waitForLeaseHolder("kube-system", serviceFaultGlobalLeaseName)

		victimLease := serviceFaultLeaseName(victimName)
		suite.assertExactlyOneServiceLeader(victimLease)
		victimHolder := suite.waitForLeaseHolder(serviceFaultNamespace, victimLease)

		By(withTimestamp(fmt.Sprintf("starting deletion of %s/%s before killing holder %q", serviceFaultNamespace, victimName, victimHolder)))
		deleteDone := make(chan error, 1)
		go func() {
			err := suite.client.CoreV1().Services(serviceFaultNamespace).Delete(suite.ctx, victimName, metav1.DeleteOptions{})
			deleteDone <- err
		}()
		Eventually(func() error {
			service, err := suite.client.CoreV1().Services(serviceFaultNamespace).Get(suite.ctx, victimName, metav1.GetOptions{})
			if err != nil {
				return err
			}
			if service.DeletionTimestamp == nil {
				return fmt.Errorf("service %s/%s has not entered deletion", serviceFaultNamespace, victimName)
			}
			return nil
		}, serviceFaultConvergenceTimeout, serviceFaultPollInterval).Should(Succeed())

		Expect(e2e.KillKubeVip(suite.cluster.Name, victimHolder, false)).To(Succeed())
		Eventually(deleteDone, serviceFaultConvergenceTimeout).Should(Receive(BeNil()))
		Expect(suite.clearServiceFinalizers(victimName)).To(Succeed())
		suite.waitForServiceDeleted(victimName)

		suite.assertNoServiceVIP(victimVIP)
		suite.assertServiceVIP(remainingVIP)
		suite.assertActiveServicesCount(1)
	})

	It("stops service reconcile errors growing after a synthetic update fault is removed", func() {
		serviceName := suite.uniqueName("fault-synthetic")
		serviceVIP := e2e.GenerateVIP(utils.IPv4Family, SOffset.Get(), defaultNetwork)
		oldLease := fmt.Sprintf("fault-synthetic-lease-%d", SOffset.Get())
		newLease := fmt.Sprintf("fault-synthetic-replacement-%d", SOffset.Get())

		suite.createService(serviceName, serviceVIP, true, false, false, corev1.ServiceExternalTrafficPolicyTypeCluster, oldLease)
		suite.waitForServiceReady(serviceName, serviceVIP)
		suite.assertServiceVIP(serviceVIP)
		beforeErrors := suite.reconcileErrorSnapshot(serviceName)
		beforeAttempts := suite.serviceElectionAttempts(serviceName)

		By(withTimestamp(fmt.Sprintf("forcing a synthetic API update error while replacing lease %q with %q", oldLease, newLease)))
		Expect(suite.patchService(serviceName, map[string]any{
			"metadata": map[string]any{
				"annotations": map[string]any{
					serviceFaultSyntheticAnnotation: "true",
					kubevip.ServiceLease:            newLease,
				},
			},
		})).To(Succeed())
		suite.waitForLeaseHolder(serviceFaultNamespace, newLease)
		if suite.metrics {
			Eventually(func() (float64, error) {
				attempts := suite.serviceElectionAttempts(serviceName)
				if attempts <= beforeAttempts {
					return attempts, fmt.Errorf("service election attempts did not advance during synthetic fault: %.0f to %.0f", beforeAttempts, attempts)
				}
				return attempts, nil
			}, serviceFaultConvergenceTimeout, serviceFaultPollInterval).Should(BeNumerically(">", beforeAttempts))
		}

		By(withTimestamp("removing the synthetic API update error and restoring the original lease"))
		Expect(suite.patchService(serviceName, map[string]any{
			"metadata": map[string]any{
				"annotations": map[string]any{
					serviceFaultSyntheticAnnotation: nil,
					kubevip.ServiceLease:            oldLease,
				},
			},
		})).To(Succeed())
		suite.waitForLeaseHolder(serviceFaultNamespace, oldLease)
		suite.waitForServiceReady(serviceName, serviceVIP)
		suite.assertServiceVIP(serviceVIP)
		suite.assertExactlyOneServiceLeader(oldLease)
		suite.assertReconcileErrorsStable(beforeErrors, serviceName, "synthetic API update error removal")
	})

	It("recreates nftables SNAT rules on the new service leader with cluster pod CIDR exclusions", func() {
		serviceName := suite.uniqueName("fault-egress")
		serviceVIP := e2e.GenerateVIP(utils.IPv4Family, SOffset.Get(), defaultNetwork)

		suite.createService(serviceName, serviceVIP, true, true, false, corev1.ServiceExternalTrafficPolicyTypeLocal, "")
		suite.waitForServiceReady(serviceName, serviceVIP)
		endpoint := suite.waitForActiveEndpoint(serviceName)
		suite.assertServiceVIP(serviceVIP)

		podCIDRs := suite.nodePodCIDRs()
		leaseName := serviceFaultLeaseName(serviceName)
		oldHolder := suite.waitForLeaseHolder(serviceFaultNamespace, leaseName)
		suite.assertExactlyOneServiceLeader(leaseName)
		suite.assertEgressRules(serviceName, serviceVIP, oldHolder, endpoint, podCIDRs)

		By(withTimestamp(fmt.Sprintf("killing egress service leader %q", oldHolder)))
		Expect(e2e.KillKubeVip(suite.cluster.Name, oldHolder, false)).To(Succeed())
		newHolder := suite.waitForDifferentLeaseHolder(serviceFaultNamespace, leaseName, oldHolder)
		suite.waitForMetrics(oldHolder)
		suite.waitForActiveEndpoint(serviceName)
		suite.assertServiceVIP(serviceVIP)
		suite.assertExactlyOneServiceLeader(leaseName)
		suite.assertEgressRules(serviceName, serviceVIP, newHolder, endpoint, podCIDRs)
	})
})

func buildServiceFaultClient(config *rest.Config) kubernetes.Interface {
	clientConfig := rest.CopyConfig(config)
	clientConfig.QPS = 50
	clientConfig.Burst = 100
	clientConfig.Timeout = 10 * time.Second

	client, err := kubernetes.NewForConfig(clientConfig)
	Expect(err).NotTo(HaveOccurred())
	return client
}

func (s *serviceFaultSuite) uniqueName(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, SOffset.Get())
}

func (s *serviceFaultSuite) createService(name, vip string, perServiceElection, egress, finalizer bool,
	trafficPolicy corev1.ServiceExternalTrafficPolicy, leaseName string,
) {
	labels := map[string]string{"app": serviceFaultBackendName}
	annotations := map[string]string{kubevip.LoadbalancerIPAnnotation: vip}
	if perServiceElection {
		annotations[kubevip.ForcePerServiceElection] = "true"
	}
	if leaseName != "" {
		annotations[kubevip.ServiceLease] = leaseName
	}
	if egress {
		annotations[kubevip.Egress] = "true"
		annotations[kubevip.EgressInternal] = "true"
	}

	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   serviceFaultNamespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: corev1.ServiceSpec{
			Type:                  corev1.ServiceTypeLoadBalancer,
			ExternalTrafficPolicy: trafficPolicy,
			IPFamilies:            []corev1.IPFamily{corev1.IPv4Protocol},
			IPFamilyPolicy:        ipFamilyPolicyPtr(corev1.IPFamilyPolicySingleStack),
			Ports: []corev1.ServicePort{{
				Protocol: corev1.ProtocolTCP,
				Port:     80,
			}},
			Selector: labels,
		},
	}
	if finalizer {
		service.Finalizers = []string{serviceFaultDeleteFinalizer}
	}

	By(withTimestamp(fmt.Sprintf("creating service %s/%s", serviceFaultNamespace, name)))
	_, err := s.client.CoreV1().Services(serviceFaultNamespace).Create(s.ctx, service, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred())
	s.created[name] = struct{}{}
}

func ipFamilyPolicyPtr(policy corev1.IPFamilyPolicy) *corev1.IPFamilyPolicy {
	return &policy
}

func (s *serviceFaultSuite) waitForServiceReady(name, vip string) *corev1.Service {
	var current *corev1.Service
	Eventually(func() error {
		service, err := s.client.CoreV1().Services(serviceFaultNamespace).Get(s.ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if service.Status.LoadBalancer.Ingress == nil {
			return fmt.Errorf("service %s/%s has no load balancer ingress", serviceFaultNamespace, name)
		}
		found := false
		for _, ingress := range service.Status.LoadBalancer.Ingress {
			if ingress.IP == vip {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("service %s/%s has not reported VIP %q", serviceFaultNamespace, name, vip)
		}
		current = service
		return nil
	}, serviceFaultConvergenceTimeout, serviceFaultPollInterval).Should(Succeed())
	return current
}

func (s *serviceFaultSuite) waitForActiveEndpoint(name string) string {
	var endpoint string
	Eventually(func() error {
		service, err := s.client.CoreV1().Services(serviceFaultNamespace).Get(s.ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		endpoint = service.Annotations[kubevip.ActiveEndpoint]
		if net.ParseIP(endpoint) == nil {
			return fmt.Errorf("service %s/%s has no active IPv4 endpoint", serviceFaultNamespace, name)
		}
		return nil
	}, serviceFaultConvergenceTimeout, serviceFaultPollInterval).Should(Succeed())
	return endpoint
}

func (s *serviceFaultSuite) assertServiceVIP(vip string) {
	assertConnection("http", vip, "80", "", 2*time.Second, serviceFaultConvergenceTimeout)
}

func (s *serviceFaultSuite) assertExactlyOneServiceLeader(leaseName string) {
	if !s.metrics {
		return
	}
	labels := map[string]string{"lease_name": leaseName}
	Eventually(func() (float64, error) {
		var total float64
		observed := false
		for _, node := range s.nodes {
			metrics, err := e2e.ScrapeMetrics(s.ctx, s.cluster.Name, node)
			if err != nil {
				return 0, err
			}
			if _, matches := e2e.MetricValue(metrics, serviceFaultLeaderMetric, labels); matches > 0 {
				observed = true
			}
			total += e2e.SumMetric(metrics, serviceFaultLeaderMetric, labels)
		}
		if !observed {
			return 0, fmt.Errorf("no leader metric found for lease %q", leaseName)
		}
		return total, nil
	}, serviceFaultConvergenceTimeout, serviceFaultPollInterval).Should(Equal(float64(1)))
}

func (s *serviceFaultSuite) waitForLeaseHolder(namespace, name string) string {
	var holder string
	Eventually(func() error {
		lease, err := s.client.CoordinationV1().Leases(namespace).Get(s.ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity == "" {
			return fmt.Errorf("lease %s/%s has no holder", namespace, name)
		}
		holder = *lease.Spec.HolderIdentity
		return nil
	}, serviceFaultConvergenceTimeout, serviceFaultPollInterval).Should(Succeed())
	return holder
}

func (s *serviceFaultSuite) waitForDifferentLeaseHolder(namespace, name, excluded string) string {
	var holder string
	Eventually(func() error {
		lease, err := s.client.CoordinationV1().Leases(namespace).Get(s.ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity == "" {
			return fmt.Errorf("lease %s/%s has no holder", namespace, name)
		}
		if *lease.Spec.HolderIdentity == excluded {
			return fmt.Errorf("lease %s/%s is still held by %q", namespace, name, excluded)
		}
		holder = *lease.Spec.HolderIdentity
		return nil
	}, serviceFaultConvergenceTimeout, serviceFaultPollInterval).Should(Succeed())
	return holder
}

func (s *serviceFaultSuite) waitForMetrics(node string) {
	if !s.metrics {
		return
	}
	Eventually(func() error {
		_, err := e2e.ScrapeMetrics(s.ctx, s.cluster.Name, node)
		return err
	}, serviceFaultConvergenceTimeout, serviceFaultPollInterval).Should(Succeed())
}

func (s *serviceFaultSuite) activeServicesSnapshot() map[string]float64 {
	if !s.metrics {
		return nil
	}
	values := make(map[string]float64, len(s.nodes))
	for _, node := range s.nodes {
		var value float64
		Eventually(func() error {
			metrics, err := e2e.ScrapeMetrics(s.ctx, s.cluster.Name, node)
			if err != nil {
				return err
			}
			value = e2e.SumMetric(metrics, serviceFaultActiveMetric, map[string]string{"namespace": serviceFaultNamespace})
			return nil
		}, serviceFaultConvergenceTimeout, serviceFaultPollInterval).Should(Succeed())
		values[node] = value
	}
	return values
}

func (s *serviceFaultSuite) activeServicesMaximum() (float64, error) {
	values := s.activeServicesSnapshot()
	if len(values) == 0 {
		return 0, fmt.Errorf("no service metrics were scraped")
	}
	maximum := 0.0
	for _, value := range values {
		if value > maximum {
			maximum = value
		}
	}
	return maximum, nil
}

func (s *serviceFaultSuite) assertActiveServicesStable(before map[string]float64, fault string) {
	if !s.metrics {
		return
	}
	expected := 0.0
	for _, value := range before {
		if value > expected {
			expected = value
		}
	}
	By(withTimestamp(fmt.Sprintf("waiting for active service count %.0f to settle after %s", expected, fault)))
	s.assertActiveServicesCount(int(expected))
}

func (s *serviceFaultSuite) assertActiveServicesCount(expected int) {
	if !s.metrics {
		return
	}
	Eventually(func() (float64, error) {
		first, err := s.activeServicesMaximum()
		if err != nil {
			return 0, err
		}
		time.Sleep(serviceFaultMetricGap)
		second, err := s.activeServicesMaximum()
		if err != nil {
			return 0, err
		}
		if first != second {
			return second, fmt.Errorf("active service maximum changed from %.0f to %.0f", first, second)
		}
		return second, nil
	}, serviceFaultConvergenceTimeout, serviceFaultPollInterval).Should(Equal(float64(expected)))
}

func (s *serviceFaultSuite) reconcileErrorSnapshot(serviceName string) map[string]float64 {
	if !s.metrics {
		return nil
	}
	values := make(map[string]float64, len(s.nodes))
	labels := map[string]string{"namespace": serviceFaultNamespace, "name": serviceName}
	for _, node := range s.nodes {
		var value float64
		Eventually(func() error {
			metrics, err := e2e.ScrapeMetrics(s.ctx, s.cluster.Name, node)
			if err != nil {
				return err
			}
			value = e2e.SumMetric(metrics, serviceFaultReconcileMetric, labels)
			return nil
		}, serviceFaultConvergenceTimeout, serviceFaultPollInterval).Should(Succeed())
		values[node] = value
	}
	return values
}

func (s *serviceFaultSuite) assertReconcileErrorsStable(before map[string]float64, serviceName, fault string) {
	if !s.metrics {
		return
	}
	beforeTotal := sumFaultValues(before)
	By(withTimestamp(fmt.Sprintf("waiting for service reconcile errors to settle after %s", fault)))
	Eventually(func() (float64, error) {
		first := s.reconcileErrorSnapshot(serviceName)
		time.Sleep(serviceFaultMetricGap)
		second := s.reconcileErrorSnapshot(serviceName)
		firstTotal := sumFaultValues(first)
		secondTotal := sumFaultValues(second)
		if firstTotal != secondTotal {
			return secondTotal - beforeTotal, fmt.Errorf("service reconcile errors changed from %.0f to %.0f", firstTotal, secondTotal)
		}
		delta := secondTotal - beforeTotal
		if delta < 0 {
			delta = 0
		}
		return delta, nil
	}, serviceFaultConvergenceTimeout, serviceFaultPollInterval).Should(BeNumerically("<=", serviceFaultReconcileDeltaLimit))
}

func (s *serviceFaultSuite) serviceElectionAttempts(serviceName string) float64 {
	if !s.metrics {
		return 0
	}
	labels := map[string]string{"namespace": serviceFaultNamespace, "name": serviceName}
	var total float64
	for _, node := range s.nodes {
		metrics, err := e2e.ScrapeMetrics(s.ctx, s.cluster.Name, node)
		if err != nil {
			return total
		}
		total += e2e.SumMetric(metrics, serviceFaultElectionAttemptsMetric, labels)
	}
	return total
}

func (s *serviceFaultSuite) nodePodCIDRs() []string {
	nodes, err := s.client.CoreV1().Nodes().List(s.ctx, metav1.ListOptions{})
	Expect(err).NotTo(HaveOccurred())
	seen := make(map[string]struct{})
	for _, node := range nodes.Items {
		cidrs := node.Spec.PodCIDRs
		if len(cidrs) == 0 && node.Spec.PodCIDR != "" {
			cidrs = []string{node.Spec.PodCIDR}
		}
		for _, cidr := range cidrs {
			cidr = strings.TrimSpace(cidr)
			ip, _, parseErr := net.ParseCIDR(cidr)
			if parseErr == nil && ip.To4() != nil {
				seen[cidr] = struct{}{}
			}
		}
	}

	result := make([]string, 0, len(seen))
	for cidr := range seen {
		result = append(result, cidr)
	}
	sort.Strings(result)
	Expect(result).NotTo(BeEmpty())
	return result
}

func (s *serviceFaultSuite) assertEgressRules(serviceName, vip, leader, endpoint string, podCIDRs []string) {
	Eventually(func() error {
		service, err := s.client.CoreV1().Services(serviceFaultNamespace).Get(s.ctx, serviceName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if service.UID == "" {
			return fmt.Errorf("service %s/%s has no UID", serviceFaultNamespace, serviceName)
		}
		currentEndpoint := service.Annotations[kubevip.ActiveEndpoint]
		if currentEndpoint == "" {
			return fmt.Errorf("service %s/%s has no active endpoint annotation", serviceFaultNamespace, serviceName)
		}
		rules, err := serviceFaultDockerExec(leader, "nft", "list", "ruleset")
		if err != nil {
			return err
		}
		chainName := fmt.Sprintf("kube_vip_snat_%s", service.UID)
		chain, err := scopedNftChain(rules, serviceFaultNftTable, chainName)
		if err != nil {
			return err
		}
		if !strings.Contains(chain, "snat to "+vip) {
			return fmt.Errorf("SNAT rule for VIP %q is missing from %s on %q", vip, chainName, leader)
		}
		if !strings.Contains(chain, currentEndpoint) && !strings.Contains(chain, endpoint) {
			return fmt.Errorf("active endpoint %q is missing from %s on %q", currentEndpoint, chainName, leader)
		}
		for _, cidr := range podCIDRs {
			if !strings.Contains(chain, cidr) {
				return fmt.Errorf("pod CIDR %q is not excluded from %s on %q", cidr, chainName, leader)
			}
		}
		return nil
	}, serviceFaultConvergenceTimeout, serviceFaultPollInterval).Should(Succeed())
}

func scopedNftChain(rules, tableName, chainName string) (string, error) {
	tableMarker := "table ip " + tableName
	tableStart := strings.Index(rules, tableMarker)
	if tableStart < 0 {
		return "", fmt.Errorf("nftables table %q is missing", tableName)
	}
	tableEnd := strings.Index(rules[tableStart+len(tableMarker):], "\ntable ")
	if tableEnd < 0 {
		tableEnd = len(rules) - tableStart - len(tableMarker)
	}
	tableText := rules[tableStart : tableStart+len(tableMarker)+tableEnd]
	chainMarker := "chain " + chainName
	chainStart := strings.Index(tableText, chainMarker)
	if chainStart < 0 {
		return "", fmt.Errorf("nftables chain %q is missing from table %q", chainName, tableName)
	}
	chainText := tableText[chainStart:]
	openingBrace := strings.IndexByte(chainText, '{')
	if openingBrace < 0 {
		return "", fmt.Errorf("nftables chain %q has no opening brace", chainName)
	}
	depth := 0
	for index := openingBrace; index < len(chainText); index++ {
		switch chainText[index] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return chainText[:index+1], nil
			}
		}
	}
	return "", fmt.Errorf("nftables chain %q has no closing brace", chainName)
}

func serviceFaultDockerExec(container string, args ...string) (string, error) {
	commandArgs := append([]string{"exec", container}, args...)
	command := exec.Command("docker", commandArgs...)
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("docker exec %s %s failed: %w: %s", container, strings.Join(args, " "), err, strings.TrimSpace(output.String()))
	}
	return output.String(), nil
}

func (s *serviceFaultSuite) assertNoServiceVIP(vip string) {
	Eventually(func() error {
		for _, node := range s.nodes {
			present, err := s.serviceVIPPresent(node, vip)
			if err != nil {
				return err
			}
			if present {
				return fmt.Errorf("service VIP %q is still present on %q", vip, node)
			}
		}
		return nil
	}, serviceFaultConvergenceTimeout, serviceFaultPollInterval).Should(Succeed())
}

func (s *serviceFaultSuite) serviceVIPPresent(node, vip string) (bool, error) {
	args := []string{"ip", "-4"}
	if Mode == ModeRT {
		args = append(args, "route", "show", "table", serviceFaultRouteTable)
	} else {
		args = append(args, "addr", "show", "dev", "eth0")
	}
	output, err := serviceFaultDockerExec(node, args...)
	if err != nil {
		return false, err
	}
	return strings.Contains(output, vip), nil
}

func (s *serviceFaultSuite) patchService(name string, patch map[string]any) error {
	payload, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	_, err = s.client.CoreV1().Services(serviceFaultNamespace).Patch(s.ctx, name, types.MergePatchType, payload, metav1.PatchOptions{})
	return err
}

func (s *serviceFaultSuite) clearServiceFinalizers(name string) error {
	return s.patchService(name, map[string]any{
		"metadata": map[string]any{"finalizers": []string{}},
	})
}

func (s *serviceFaultSuite) waitForServiceDeleted(name string) {
	Eventually(func() error {
		_, err := s.client.CoreV1().Services(serviceFaultNamespace).Get(s.ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}, serviceFaultConvergenceTimeout, serviceFaultPollInterval).Should(Succeed())
}

func (s *serviceFaultSuite) cleanupServices() {
	for name := range s.created {
		service, err := s.client.CoreV1().Services(serviceFaultNamespace).Get(s.ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			delete(s.created, name)
			continue
		}
		Expect(err).NotTo(HaveOccurred())
		if len(service.Finalizers) > 0 {
			Expect(s.clearServiceFinalizers(name)).To(Succeed())
		}
		err = s.client.CoreV1().Services(serviceFaultNamespace).Delete(s.ctx, name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			Expect(err).NotTo(HaveOccurred())
		}
		s.waitForServiceDeleted(name)
		delete(s.created, name)
	}
}

func serviceFaultLeaseName(serviceName string) string {
	return "kubevip-" + serviceName
}

func sumFaultValues(values map[string]float64) float64 {
	var total float64
	for _, value := range values {
		total += value
	}
	return total
}

func (s *serviceFaultSuite) metricsAvailable() bool {
	for _, node := range s.nodes {
		if _, err := e2e.ScrapeMetrics(s.ctx, s.cluster.Name, node); err != nil {
			By(withTimestamp(fmt.Sprintf("metrics unavailable on %q; continuing with functional assertions: %v", node, err)))
			return false
		}
	}
	return true
}
