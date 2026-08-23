//go:build e2e
// +build e2e

package e2e_test

import (
	"context"
	"fmt"
	"os"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	kindconfigv1alpha4 "sigs.k8s.io/kind/pkg/apis/config/v1alpha4"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/kube-vip/kube-vip/pkg/utils"
	"github.com/kube-vip/kube-vip/testing/e2e"
)

const (
	// Runner guardrails keep the nightly suite bounded while exercising a real
	// multi-node Kind topology.
	scaleControlPlaneNodes = 1
	scaleWorkerNodes       = 2
	scaleMaxKindNodes      = 3
	scaleMaxServices       = 150
	scaleServiceBatchSize  = 25
	scaleClientQPS         = 100
	scaleClientBurst       = 200
	scalePollInterval      = time.Second

	scaleAdvertisementP100Bound   = 5 * time.Minute
	scaleClusterConvergenceLimit  = 5 * time.Minute
	scaleChurnDuration            = 10 * time.Minute
	scaleChurnInterval            = time.Second
	scaleChurnMaxServices         = 25
	scaleElectionServiceCount     = 10
	scaleElectionCycles           = 10
	scaleTransitionDeltaLimit     = 300.0
	scaleReconcileErrorDeltaLimit = 0.0
	scaleEndpointCycles           = 3
	scaleEndpointScaleUp          = 30
	scaleBackendInitialReplicas   = 3
	scaleBackendPort              = 80

	scaleNamespace            = "kube-vip-scale"
	scaleSuiteLabel           = "scale.kube-vip.io/suite"
	scaleSuiteValue           = "pr13"
	scaleScenarioLabel        = "scale.kube-vip.io/scenario"
	scaleBackendLabel         = "scale.kube-vip.io/backend"
	scaleBackendName          = "shared-backend"
	scaleBackendContainerName = "whoami"
	scaleBackendImage         = "ghcr.io/traefik/whoami:v1.11"
	scaleRevisionAnnotation   = "scale.kube-vip.io/revision"

	scaleScenarioBulk          = "bulk"
	scaleScenarioChurn         = "churn"
	scaleScenarioElection      = "election"
	scaleScenarioEndpoint      = "endpoint"
	scaleBulkServicePrefix     = "scale-bulk"
	scaleChurnServicePrefix    = "scale-churn"
	scaleElectionServicePrefix = "scale-election"
	scaleEndpointServiceName   = "scale-endpoint"
	scaleElectionLeasePrefix   = "scale-election-lease"
)

type scaleSuite struct {
	ctx        context.Context
	cancel     context.CancelFunc
	cluster    *e2e.Cluster
	client     kubernetes.Interface
	nodeNames  []string
	namespace  string
	baseOffset uint
	tempDir    string
}

var _ = Describe("kube-vip controller behavior scale suite", Label("scale"), Serial, Ordered, func() {
	if Mode != ModeARP {
		return
	}

	suite := &scaleSuite{namespace: scaleNamespace}

	BeforeAll(func() {
		Expect(scaleControlPlaneNodes + scaleWorkerNodes).To(BeNumerically("<=", scaleMaxKindNodes))
		Expect(scaleServiceBatchSize).To(BeNumerically("<=", scaleMaxServices))
		Expect(scaleChurnMaxServices).To(BeNumerically("<=", scaleMaxServices))
		Expect(scalePollInterval).To(BeNumerically(">=", time.Second))

		suite.ctx, suite.cancel = context.WithCancel(context.Background())
		var err error
		suite.tempDir, err = os.MkdirTemp("", "kube-vip-scale-")
		Expect(err).NotTo(HaveOccurred())

		suite.baseOffset = SOffset.Get()
		controlPlaneVIP := e2e.GenerateVIP(utils.IPv4Family, suite.baseOffset, defaultNetwork)
		suite.cluster = e2e.CreateCluster(suite.ctx, &e2e.ClusterSpec{
			Name:        fmt.Sprintf("kube-vip-scale-%d", suite.baseOffset),
			Nodes:       scaleControlPlaneNodes,
			WorkerNodes: scaleWorkerNodes,
			Networking: kindconfigv1alpha4.Networking{
				IPFamily: kindconfigv1alpha4.IPv4Family,
			},
			KubeVip: e2e.KubevipManifestValues{
				ControlPlaneVIP:            controlPlaneVIP,
				ControlPlaneEnable:         "true",
				SvcEnable:                  "true",
				SvcElectionEnable:          "true",
				EnableEndpoints:            "true",
				EnableNodeLabeling:         "false",
				EnableServiceSecurity:      "true",
				PerServiceElectionOnDemand: "true",
			},
			Logger:    e2e.TestLogger{},
			ConfigMtx: ConfigMtx,
		})
		Expect(e2e.InstallScaleWorkerKubeconfigs(suite.cluster)).To(Succeed())
		suite.client, err = e2e.BuildScaleClient(suite.cluster.RestCfg, scaleClientQPS, scaleClientBurst)
		Expect(err).NotTo(HaveOccurred())
		suite.cluster.Client = suite.client

		for _, node := range suite.cluster.Nodes {
			suite.nodeNames = append(suite.nodeNames, node.String())
		}
		Expect(suite.nodeNames).To(HaveLen(scaleControlPlaneNodes + scaleWorkerNodes))
		Expect(len(suite.nodeNames)).To(BeNumerically("<=", scaleMaxKindNodes))

		By("waiting for kube-vip metrics on the control-plane and worker nodes")
		Eventually(func() error {
			return e2e.WaitForScaleMetrics(suite.cluster.Name, suite.nodeNames)
		}, scaleClusterConvergenceLimit, scalePollInterval).Should(Succeed())

		Expect(e2e.EnsureScaleNamespace(suite.ctx, suite.client, suite.namespace)).To(Succeed())
		Expect(e2e.CreateScaleBackend(suite.ctx, suite.client, suite.namespace, scaleBackendInitialReplicas)).To(Succeed())
		Eventually(func() error {
			return e2e.ScaleBackendReady(suite.ctx, suite.client, suite.namespace, scaleBackendInitialReplicas)
		}, scaleClusterConvergenceLimit, scalePollInterval).Should(Succeed())
	})

	AfterAll(func() {
		if suite.cluster != nil {
			suite.cluster.SaveLogs(context.Background(), suite.tempDir)
			suite.cluster.Delete()
		}
		if suite.cancel != nil {
			suite.cancel()
		}
		if suite.tempDir != "" && os.Getenv("E2E_KEEP_LOGS") != "true" {
			Expect(os.RemoveAll(suite.tempDir)).To(Succeed())
		}
	})

	It("advertises 150 services in bounded batches with one shared backend", func() {
		defer suite.cleanupScenario(scaleScenarioBulk)

		serviceNames := make([]string, 0, scaleMaxServices)
		vips := make([]string, 0, scaleMaxServices)
		started := time.Now()
		for batchStart := 0; batchStart < scaleMaxServices; batchStart += scaleServiceBatchSize {
			batchEnd := batchStart + scaleServiceBatchSize
			if batchEnd > scaleMaxServices {
				batchEnd = scaleMaxServices
			}
			for index := batchStart; index < batchEnd; index++ {
				name := fmt.Sprintf("%s-%03d", scaleBulkServicePrefix, index)
				vip := e2e.GenerateVIP(utils.IPv4Family, suite.baseOffset+uint(index+1), defaultNetwork)
				Expect(e2e.CreateScaleService(suite.ctx, suite.client, suite.namespace, scaleScenarioBulk, name, vip,
					corev1.ServiceExternalTrafficPolicyTypeCluster, false, "")).To(Succeed())
				serviceNames = append(serviceNames, name)
				vips = append(vips, vip)
			}
			By(fmt.Sprintf("created services %d through %d in a batch of at most %d", batchStart+1, batchEnd, scaleServiceBatchSize))
		}

		deadline := started.Add(scaleAdvertisementP100Bound)
		remaining := time.Until(deadline)
		Expect(remaining).To(BeNumerically(">", 0))
		Expect(e2e.WaitForScaleServiceCount(suite.ctx, suite.client, suite.namespace, scaleScenarioBulk, len(serviceNames))).To(Succeed())
		Expect(e2e.WaitForScaleActiveServices(suite.ctx, suite.cluster.Name, suite.nodeNames, suite.namespace,
			len(serviceNames), remaining)).To(Succeed())

		spotChecks := []string{vips[0], vips[len(vips)/2], vips[len(vips)-1]}
		remaining = time.Until(deadline)
		Expect(remaining).To(BeNumerically(">", 0))
		Expect(e2e.WaitForScaleVIPs(suite.cluster.Name, suite.nodeNames, spotChecks, remaining)).To(Succeed())

		elapsed := time.Since(started)
		By(fmt.Sprintf("all %d services became active and spot-check VIPs were advertised in %s", len(serviceNames), elapsed))
		Expect(elapsed).To(BeNumerically("<", scaleAdvertisementP100Bound))
	})

	It("keeps active service count exact and reconcile errors flat during ten-minute churn", func() {
		defer suite.cleanupScenario(scaleScenarioChurn)

		beforeMetrics, err := e2e.SnapshotScaleMetrics(suite.cluster.Name, suite.nodeNames)
		Expect(err).NotTo(HaveOccurred())

		active := make(map[string]struct{}, scaleChurnMaxServices)
		nextOrdinal := 0
		step := 0
		deadline := time.Now().Add(scaleChurnDuration)
		ticker := time.NewTicker(scaleChurnInterval)
		defer ticker.Stop()

		for time.Now().Before(deadline) {
			select {
			case <-suite.ctx.Done():
				Fail(suite.ctx.Err().Error())
			case <-ticker.C:
				step++
				Expect(suite.runChurnOperation(active, &nextOrdinal, step)).To(Succeed())
				if step%60 == 0 {
					By(fmt.Sprintf("completed %d churn operations at approximately one operation per second", step))
				}
			}
		}

		services, err := e2e.ListScaleServices(suite.ctx, suite.client, suite.namespace, scaleScenarioChurn)
		Expect(err).NotTo(HaveOccurred())
		Expect(len(services)).To(Equal(len(active)))
		Expect(len(services)).To(BeNumerically("<=", scaleMaxServices))

		Eventually(func() error {
			return e2e.WaitForScaleServiceCount(suite.ctx, suite.client, suite.namespace, scaleScenarioChurn, len(active))
		}, scaleClusterConvergenceLimit, scalePollInterval).Should(Succeed())
		Expect(e2e.WaitForScaleActiveServices(suite.ctx, suite.cluster.Name, suite.nodeNames, suite.namespace,
			len(active), scaleClusterConvergenceLimit)).To(Succeed())

		afterMetrics, err := e2e.SnapshotScaleMetrics(suite.cluster.Name, suite.nodeNames)
		Expect(err).NotTo(HaveOccurred())
		reconcileDelta := e2e.ScaleCounterDelta(beforeMetrics, afterMetrics,
			"kube_vip_service_reconcile_errors_total", map[string]string{"namespace": suite.namespace})
		By(fmt.Sprintf("churn left %d services with reconcile-error delta %.0f", len(active), reconcileDelta))
		Expect(reconcileDelta).To(BeNumerically("<=", scaleReconcileErrorDeltaLimit))
	})

	It("reacquires every per-service lease after killing leaders ten times", func() {
		defer suite.cleanupScenario(scaleScenarioElection)

		leaseNames := make([]string, 0, scaleElectionServiceCount)
		for index := 0; index < scaleElectionServiceCount; index++ {
			name := fmt.Sprintf("%s-%02d", scaleElectionServicePrefix, index)
			leaseName := fmt.Sprintf("%s-%02d", scaleElectionLeasePrefix, index)
			vip := e2e.GenerateVIP(utils.IPv4Family, suite.baseOffset+uint(scaleMaxServices+index+1), defaultNetwork)
			Expect(e2e.CreateScaleService(suite.ctx, suite.client, suite.namespace, scaleScenarioElection, name, vip,
				corev1.ServiceExternalTrafficPolicyTypeCluster, true, leaseName)).To(Succeed())
			leaseNames = append(leaseNames, leaseName)
		}

		Eventually(func() error {
			_, err := e2e.ScaleLeaseHolders(suite.ctx, suite.client, suite.namespace, leaseNames)
			return err
		}, scaleClusterConvergenceLimit, scalePollInterval).Should(Succeed())
		beforeMetrics, err := e2e.SnapshotScaleMetrics(suite.cluster.Name, suite.nodeNames)
		Expect(err).NotTo(HaveOccurred())

		for cycle := 0; cycle < scaleElectionCycles; cycle++ {
			previous, err := e2e.ScaleLeaseHolders(suite.ctx, suite.client, suite.namespace, leaseNames)
			Expect(err).NotTo(HaveOccurred())
			victim := previous[leaseNames[cycle%len(leaseNames)]]
			By(fmt.Sprintf("killing kube-vip leader %q for election cycle %d of %d", victim, cycle+1, scaleElectionCycles))
			Expect(e2e.KillKubeVip(suite.cluster.Name, victim, false)).To(Succeed())

			Eventually(func() error {
				return e2e.WaitForScaleMetrics(suite.cluster.Name, suite.nodeNames)
			}, scaleClusterConvergenceLimit, scalePollInterval).Should(Succeed())
			Expect(e2e.WaitForScaleLeaseHolders(suite.ctx, suite.client, suite.namespace, leaseNames, previous, victim,
				scaleClusterConvergenceLimit)).To(Succeed())
		}

		afterMetrics, err := e2e.SnapshotScaleMetrics(suite.cluster.Name, suite.nodeNames)
		Expect(err).NotTo(HaveOccurred())
		transitionDelta := e2e.ScaleTransitionDelta(beforeMetrics, afterMetrics, leaseNames)
		By(fmt.Sprintf("all %d service leases reacquired with %.0f total transition delta", len(leaseNames), transitionDelta))
		Expect(transitionDelta).To(BeNumerically("<=", scaleTransitionDeltaLimit))
	})

	It("follows ETP Local advertisement as one shared deployment scales one to thirty to one", func() {
		defer func() {
			suite.cleanupScenario(scaleScenarioEndpoint)
			Expect(e2e.ScaleBackend(suite.ctx, suite.client, suite.namespace, scaleBackendInitialReplicas)).To(Succeed())
			Eventually(func() error {
				return e2e.ScaleBackendReady(suite.ctx, suite.client, suite.namespace, scaleBackendInitialReplicas)
			}, scaleClusterConvergenceLimit, scalePollInterval).Should(Succeed())
		}()

		Expect(e2e.ScaleBackend(suite.ctx, suite.client, suite.namespace, 1)).To(Succeed())
		Eventually(func() error {
			return e2e.ScaleBackendReady(suite.ctx, suite.client, suite.namespace, 1)
		}, scaleClusterConvergenceLimit, scalePollInterval).Should(Succeed())

		vip := e2e.GenerateVIP(utils.IPv4Family, suite.baseOffset+uint(scaleMaxServices+scaleElectionServiceCount+1), defaultNetwork)
		Expect(e2e.CreateScaleService(suite.ctx, suite.client, suite.namespace, scaleScenarioEndpoint, scaleEndpointServiceName, vip,
			corev1.ServiceExternalTrafficPolicyTypeLocal, true, "scale-endpoint-lease")).To(Succeed())
		Expect(e2e.WaitForScaleLocalAdvertisement(suite.ctx, suite.cluster.Name, suite.nodeNames, suite.client,
			suite.namespace, vip, scaleClusterConvergenceLimit)).To(Succeed())

		for cycle := 0; cycle < scaleEndpointCycles; cycle++ {
			By(fmt.Sprintf("scaling endpoint fan-out cycle %d of %d from one to thirty replicas", cycle+1, scaleEndpointCycles))
			Expect(e2e.ScaleBackend(suite.ctx, suite.client, suite.namespace, scaleEndpointScaleUp)).To(Succeed())
			Eventually(func() error {
				return e2e.ScaleBackendReady(suite.ctx, suite.client, suite.namespace, scaleEndpointScaleUp)
			}, scaleClusterConvergenceLimit, scalePollInterval).Should(Succeed())
			Expect(e2e.WaitForScaleLocalAdvertisement(suite.ctx, suite.cluster.Name, suite.nodeNames, suite.client,
				suite.namespace, vip, scaleClusterConvergenceLimit)).To(Succeed())

			By(fmt.Sprintf("scaling endpoint fan-out cycle %d from thirty back to one replica", cycle+1))
			Expect(e2e.ScaleBackend(suite.ctx, suite.client, suite.namespace, 1)).To(Succeed())
			Eventually(func() error {
				return e2e.ScaleBackendReady(suite.ctx, suite.client, suite.namespace, 1)
			}, scaleClusterConvergenceLimit, scalePollInterval).Should(Succeed())
			Expect(e2e.WaitForScaleLocalAdvertisement(suite.ctx, suite.cluster.Name, suite.nodeNames, suite.client,
				suite.namespace, vip, scaleClusterConvergenceLimit)).To(Succeed())
		}
	})
})

func (s *scaleSuite) cleanupScenario(scenario string) {
	if s.client == nil {
		return
	}
	Expect(e2e.DeleteScaleServices(s.ctx, s.client, s.namespace, scenario)).To(Succeed())
	Eventually(func() error {
		return e2e.WaitForScaleServiceCount(s.ctx, s.client, s.namespace, scenario, 0)
	}, scaleClusterConvergenceLimit, scalePollInterval).Should(Succeed())
}

func (s *scaleSuite) runChurnOperation(active map[string]struct{}, nextOrdinal *int, step int) error {
	names := make([]string, 0, len(active))
	for name := range active {
		names = append(names, name)
	}
	sort.Strings(names)

	if step%3 == 0 && len(active) < scaleChurnMaxServices || len(active) == 0 {
		name := fmt.Sprintf("%s-%04d", scaleChurnServicePrefix, *nextOrdinal)
		vip := e2e.GenerateVIP(utils.IPv4Family, s.baseOffset+uint(2*scaleMaxServices+*nextOrdinal+1), defaultNetwork)
		if err := e2e.CreateScaleService(s.ctx, s.client, s.namespace, scaleScenarioChurn, name, vip,
			corev1.ServiceExternalTrafficPolicyTypeCluster, false, ""); err != nil {
			return err
		}
		active[name] = struct{}{}
		*nextOrdinal++
		return nil
	}

	if len(names) == 0 {
		return fmt.Errorf("churn operation %d has no service to modify", step)
	}

	name := names[0]
	if step%3 == 1 {
		if err := s.client.CoreV1().Services(s.namespace).Delete(s.ctx, name, metav1.DeleteOptions{}); err != nil {
			return fmt.Errorf("delete churn service %s/%s: %w", s.namespace, name, err)
		}
		delete(active, name)
		return nil
	}
	return e2e.UpdateScaleService(s.ctx, s.client, s.namespace, name, step)
}
