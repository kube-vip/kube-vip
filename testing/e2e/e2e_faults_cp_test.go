//go:build e2e
// +build e2e

package e2e_test

import (
	"context"
	"fmt"
	"os"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	kindconfigv1alpha4 "sigs.k8s.io/kind/pkg/apis/config/v1alpha4"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/kube-vip/kube-vip/pkg/utils"
	"github.com/kube-vip/kube-vip/testing/e2e"
)

const (
	faultClusterNodeCount      = 3
	faultLeaseNamespace        = "kube-system"
	faultLeaseName             = "plndr-cp-lock"
	faultTransitionMetric      = "kube_vip_leader_election_transitions_total"
	faultPollInterval          = time.Second
	faultMetricGap             = time.Second
	faultConvergenceTimeout    = 120 * time.Second
	faultLeaseObservationLimit = 15 * time.Second
	faultSteadyStateWindow     = 5 * time.Second
	faultTransitionDeltaLimit  = 4.0
)

type controlPlaneFaultSuite struct {
	ctx     context.Context
	cancel  context.CancelFunc
	cluster *e2e.Cluster
	client  kubernetes.Interface
	vip     string
	nodes   []string
	tempDir string
}

type faultMetricSnapshot map[string]map[string]float64

var _ = Describe("kube-vip control-plane election and VIP failover faults", Label("faults"), Serial, Ordered, func() {
	if Mode != ModeARP && Mode != ModeRT {
		return
	}

	suite := &controlPlaneFaultSuite{}

	BeforeAll(func() {
		suite.ctx, suite.cancel = context.WithCancel(context.Background())
		var err error
		suite.tempDir, err = os.MkdirTemp("", fmt.Sprintf("kube-vip-faults-%s-", Mode))
		Expect(err).NotTo(HaveOccurred())

		offset := SOffset.Get()
		suite.vip = e2e.GenerateVIP(utils.IPv4Family, offset, defaultNetwork)

		templateName := "kube-vip.yaml.tmpl"
		manifestValues := e2e.KubevipManifestValues{
			ControlPlaneVIP:       suite.vip,
			ImagePath:             os.Getenv("E2E_IMAGE_PATH"),
			ConfigPath:            os.Getenv("CONFIG_PATH"),
			ControlPlaneEnable:    "true",
			SvcEnable:             "false",
			SvcElectionEnable:     "false",
			EnableEndpoints:       "true",
			EnableNodeLabeling:    "false",
			EnableServiceSecurity: "true",
		}
		networking := kindconfigv1alpha4.Networking{
			IPFamily: kindconfigv1alpha4.IPv4Family,
		}
		if Mode == ModeRT {
			templateName = "kube-vip-routing-table.yaml.tmpl"
			manifestValues.VipElectionEnable = "true"
		}

		suite.cluster = e2e.CreateCluster(suite.ctx, &e2e.ClusterSpec{
			Name:         fmt.Sprintf("kube-vip-faults-%s-%d", Mode, offset),
			Nodes:        faultClusterNodeCount,
			Networking:   networking,
			KubeVip:      manifestValues,
			Logger:       e2e.TestLogger{},
			ConfigMtx:    ConfigMtx,
			TemplateName: templateName,
		})
		suite.client = buildFaultClient(suite.cluster.RestCfg)
		suite.cluster.Client = suite.client

		for _, node := range suite.cluster.Nodes {
			suite.nodes = append(suite.nodes, node.String())
		}
		Expect(suite.nodes).To(HaveLen(faultClusterNodeCount))

		By(withTimestamp("waiting for the control-plane VIP to become routable"))
		assertControlPlaneIsRoutable(suite.vip, 2*time.Second, faultConvergenceTimeout)
		leader := suite.waitForLeader()
		suite.waitForLease()
		suite.assertLeaderMetric(leader)
		suite.assertOneLeader()
		suite.assertSteadyLeader(leader)
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

	It("keeps the VIP available while the leader loses API access and recovers", func() {
		before := suite.transitionSnapshot()
		oldLeader := suite.waitForLeader()

		By(withTimestamp(fmt.Sprintf("blackholing the API server from leader %q", oldLeader)))
		Expect(e2e.BlackholeAPIServer(suite.cluster.Name, oldLeader)).To(Succeed())
		blackholeActive := true
		defer func() {
			if blackholeActive {
				Expect(e2e.RestoreAPIServer(suite.cluster.Name, oldLeader)).To(Succeed())
			}
		}()

		newLeader := suite.waitForDifferentLeader(oldLeader)
		assertControlPlaneIsRoutable(suite.vip, 2*time.Second, faultConvergenceTimeout)
		lease := suite.waitForLease(oldLeader)
		By(withTimestamp(fmt.Sprintf("lease %s/%s is held by remaining node %q while %q is blackholed", faultLeaseNamespace, faultLeaseName, *lease.Spec.HolderIdentity, oldLeader)))
		suite.assertLeaderMetric(newLeader)
		suite.assertOneLeader(oldLeader)

		By(withTimestamp(fmt.Sprintf("restoring the API server connection on %q", oldLeader)))
		Expect(e2e.RestoreAPIServer(suite.cluster.Name, oldLeader)).To(Succeed())
		blackholeActive = false

		suite.waitForLease()
		recoveredLeader := suite.waitForLeader()
		assertControlPlaneIsRoutable(suite.vip, 2*time.Second, faultConvergenceTimeout)
		suite.assertLeaderMetric(recoveredLeader)
		suite.assertOneLeader()
		suite.assertSteadyLeader(recoveredLeader)
		suite.assertTransitionCounterStable(before, "API server blackhole and recovery")
	})

	It("recovers after SIGKILL of the kube-vip leader", func() {
		before := suite.transitionSnapshot()
		oldLeader := suite.waitForLeader()

		By(withTimestamp(fmt.Sprintf("sending SIGKILL to kube-vip on leader %q", oldLeader)))
		Expect(e2e.KillKubeVip(suite.cluster.Name, oldLeader, false)).To(Succeed())

		newLeader := suite.waitForDifferentLeader(oldLeader)
		assertControlPlaneIsRoutable(suite.vip, 2*time.Second, faultConvergenceTimeout)
		suite.waitForLease(oldLeader)
		suite.assertLeaderMetric(newLeader)

		By(withTimestamp(fmt.Sprintf("waiting for kube-vip metrics to return on restarted node %q", oldLeader)))
		suite.waitForMetrics(oldLeader)
		recoveredLeader := suite.waitForLeader()
		suite.assertLeaderMetric(recoveredLeader)
		suite.assertOneLeader()
		suite.assertSteadyLeader(recoveredLeader)
		suite.assertTransitionCounterStable(before, "SIGKILL and kube-vip restart")
	})

	It("reacquires the lease after deletion and lease stealing", func() {
		beforeDelete := suite.transitionSnapshot()
		oldLease := suite.getLease()
		oldUID := string(oldLease.UID)

		By(withTimestamp(fmt.Sprintf("deleting lease %s/%s", faultLeaseNamespace, faultLeaseName)))
		Expect(e2e.DeleteLease(suite.ctx, suite.client, faultLeaseNamespace, faultLeaseName)).To(Succeed())
		recreatedLease := suite.waitForLease()
		Expect(string(recreatedLease.UID)).NotTo(Equal(oldUID))
		leader := suite.waitForLeader()
		assertControlPlaneIsRoutable(suite.vip, 2*time.Second, faultConvergenceTimeout)
		suite.assertLeaderMetric(leader)
		suite.assertOneLeader()
		suite.assertSteadyLeader(leader)
		suite.assertTransitionCounterStable(beforeDelete, "lease deletion")

		beforeSteal := suite.transitionSnapshot()
		const stolenHolder = "fault-injector"
		By(withTimestamp(fmt.Sprintf("overwriting lease %s/%s with holder %q", faultLeaseNamespace, faultLeaseName, stolenHolder)))
		Eventually(func() error {
			return e2e.StealLease(suite.ctx, suite.client, faultLeaseNamespace, faultLeaseName, stolenHolder)
		}, faultLeaseObservationLimit, faultPollInterval).Should(Succeed())
		suite.waitForLeaseHolder(stolenHolder)
		suite.waitForLease(stolenHolder)
		assertControlPlaneIsRoutable(suite.vip, 2*time.Second, faultConvergenceTimeout)
		suite.assertLeaderMetric(suite.waitForLeader())
		suite.assertOneLeader()
		stableLeader := suite.waitForLeader()
		suite.assertSteadyLeader(stableLeader)
		suite.assertTransitionCounterStable(beforeSteal, "lease stealing")
	})

	It("fully recovers after restarting the leader node", func() {
		before := suite.transitionSnapshot()
		oldLeader := suite.waitForLeader()

		By(withTimestamp(fmt.Sprintf("restarting leader node %q", oldLeader)))
		Expect(e2e.RestartNode(suite.cluster.Name, oldLeader)).To(Succeed())
		suite.waitForNodeReady(oldLeader)
		assertControlPlaneIsRoutable(suite.vip, 2*time.Second, faultConvergenceTimeout)

		By(withTimestamp(fmt.Sprintf("waiting for kube-vip metrics to return on restarted node %q", oldLeader)))
		suite.waitForMetrics(oldLeader)
		leader := suite.waitForLeader()
		suite.waitForLease()
		suite.assertLeaderMetric(leader)
		suite.assertOneLeader()
		suite.assertSteadyLeader(leader)
		suite.assertTransitionCounterStable(before, "control-plane node restart")
	})
})

func buildFaultClient(config *rest.Config) kubernetes.Interface {
	// Fault polling is deliberately more generous than client-go defaults.
	config.QPS = 50
	config.Burst = 100
	config.Timeout = 10 * time.Second

	client, err := kubernetes.NewForConfig(config)
	Expect(err).NotTo(HaveOccurred())
	return client
}

func (s *controlPlaneFaultSuite) waitForLeader() string {
	// RT mode keeps the VIP in a route instead of an interface address, so the
	// existing Docker IP lookup used by ARP mode cannot identify its leader.
	if Mode == ModeRT {
		lease := s.waitForLease()
		return *lease.Spec.HolderIdentity
	}

	var leader string
	Eventually(func() (string, error) {
		leader = findLeader(s.vip, s.cluster.Name)
		if leader == "" {
			return "", fmt.Errorf("VIP %s has no leader", s.vip)
		}
		return leader, nil
	}, faultConvergenceTimeout, faultPollInterval).ShouldNot(BeEmpty())
	return leader
}

func (s *controlPlaneFaultSuite) waitForDifferentLeader(oldLeader string) string {
	if Mode == ModeRT {
		var leader string
		Eventually(func() (string, error) {
			lease, err := s.client.CoordinationV1().Leases(faultLeaseNamespace).Get(s.ctx, faultLeaseName, metav1.GetOptions{})
			if err != nil {
				return "", err
			}
			if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity == "" {
				return "", fmt.Errorf("lease %s/%s has no holder", faultLeaseNamespace, faultLeaseName)
			}
			if *lease.Spec.HolderIdentity == oldLeader {
				return "", fmt.Errorf("lease %s/%s is still held by %s", faultLeaseNamespace, faultLeaseName, oldLeader)
			}
			leader = *lease.Spec.HolderIdentity
			return leader, nil
		}, faultConvergenceTimeout, faultPollInterval).ShouldNot(BeEmpty())
		return leader
	}

	var leader string
	Eventually(func() (string, error) {
		candidate := findLeader(s.vip, s.cluster.Name)
		if candidate == "" {
			return "", fmt.Errorf("VIP %s has no leader", s.vip)
		}
		if candidate == oldLeader {
			return "", fmt.Errorf("VIP %s is still held by %s", s.vip, oldLeader)
		}
		leader = candidate
		return leader, nil
	}, faultConvergenceTimeout, faultPollInterval).ShouldNot(BeEmpty())
	return leader
}

func (s *controlPlaneFaultSuite) getLease() *coordinationv1.Lease {
	lease, err := s.client.CoordinationV1().Leases(faultLeaseNamespace).Get(s.ctx, faultLeaseName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	return lease
}

func (s *controlPlaneFaultSuite) waitForLease(excludedHolders ...string) *coordinationv1.Lease {
	excluded := make(map[string]struct{}, len(excludedHolders))
	for _, holder := range excludedHolders {
		excluded[holder] = struct{}{}
	}

	var current *coordinationv1.Lease
	Eventually(func() error {
		lease, err := s.client.CoordinationV1().Leases(faultLeaseNamespace).Get(s.ctx, faultLeaseName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity == "" {
			return fmt.Errorf("lease %s/%s has no holder", faultLeaseNamespace, faultLeaseName)
		}
		if _, skip := excluded[*lease.Spec.HolderIdentity]; skip {
			return fmt.Errorf("lease %s/%s is still held by excluded node %q", faultLeaseNamespace, faultLeaseName, *lease.Spec.HolderIdentity)
		}
		current = lease
		return nil
	}, faultConvergenceTimeout, faultPollInterval).Should(Succeed())
	return current
}

func (s *controlPlaneFaultSuite) waitForLeaseHolder(holder string) *coordinationv1.Lease {
	var current *coordinationv1.Lease
	Eventually(func() error {
		lease, err := s.client.CoordinationV1().Leases(faultLeaseNamespace).Get(s.ctx, faultLeaseName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != holder {
			return fmt.Errorf("lease %s/%s is not held by %q", faultLeaseNamespace, faultLeaseName, holder)
		}
		current = lease
		return nil
	}, faultLeaseObservationLimit, faultPollInterval).Should(Succeed())
	return current
}

func (s *controlPlaneFaultSuite) waitForMetrics(node string) {
	Eventually(func() error {
		_, err := e2e.ScrapeMetrics(s.cluster.Name, node)
		return err
	}, faultConvergenceTimeout, faultPollInterval).Should(Succeed())
}

func (s *controlPlaneFaultSuite) waitForNodeReady(nodeName string) {
	Eventually(func() error {
		node, err := s.client.CoreV1().Nodes().Get(s.ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		for _, condition := range node.Status.Conditions {
			if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
				return nil
			}
		}
		return fmt.Errorf("node %q is not Ready", nodeName)
	}, faultConvergenceTimeout, faultPollInterval).Should(Succeed())
}

func (s *controlPlaneFaultSuite) assertLeaderMetric(leader string) {
	e2e.EventuallyMetric(
		s.cluster.Name,
		leader,
		"kube_vip_is_leader",
		map[string]string{"node": leader, "lease_name": faultLeaseName},
		Equal(float64(1)),
		faultConvergenceTimeout,
		faultPollInterval,
	)
}

func (s *controlPlaneFaultSuite) assertSteadyLeader(leader string) {
	e2e.ConsistentlyMetric(
		s.cluster.Name,
		leader,
		"kube_vip_is_leader",
		map[string]string{"node": leader, "lease_name": faultLeaseName},
		Equal(float64(1)),
		faultSteadyStateWindow,
		faultPollInterval,
	)
}

func (s *controlPlaneFaultSuite) assertOneLeader(skipNodes ...string) {
	assertExactlyOneLeaderMetric(s.ctx, s.cluster.Name, s.client, skipNodes...)

	skipped := make(map[string]struct{}, len(skipNodes))
	for _, node := range skipNodes {
		skipped[node] = struct{}{}
	}
	Eventually(func() (float64, error) {
		maximum := 0.0
		found := false
		for _, node := range s.nodes {
			if _, skip := skipped[node]; skip {
				continue
			}
			metrics, err := e2e.ScrapeMetrics(s.cluster.Name, node)
			if err != nil {
				return 0, err
			}
			value, ok := e2e.MaxMetric(metrics, "kube_vip_is_leader", map[string]string{"lease_name": faultLeaseName})
			if !ok {
				continue
			}
			found = true
			if value > maximum {
				maximum = value
			}
		}
		if !found {
			return 0, fmt.Errorf("no leader metric found")
		}
		return maximum, nil
	}, faultConvergenceTimeout, faultPollInterval).Should(BeNumerically("<=", float64(1)))
}

func (s *controlPlaneFaultSuite) transitionSnapshot() faultMetricSnapshot {
	snapshot := make(faultMetricSnapshot, len(s.nodes))
	for _, node := range s.nodes {
		var metrics map[string]float64
		Eventually(func() error {
			var err error
			metrics, err = e2e.ScrapeMetrics(s.cluster.Name, node)
			return err
		}, faultConvergenceTimeout, faultPollInterval).Should(Succeed())
		snapshot[node] = metrics
	}
	return snapshot
}

func (s *controlPlaneFaultSuite) assertTransitionCounterStable(before faultMetricSnapshot, fault string) {
	labels := map[string]string{"lease_name": faultLeaseName}
	stableValues := make(map[string]float64)
	totalDelta := 0.0
	observed := 0

	for _, node := range s.nodes {
		metrics, err := e2e.ScrapeMetrics(s.cluster.Name, node)
		Expect(err).NotTo(HaveOccurred())
		if _, matches := e2e.MetricValue(metrics, faultTransitionMetric, labels); matches == 0 {
			continue
		}

		var stable float64
		Eventually(func() error {
			var stableErr error
			stable, stableErr = e2e.MetricStable(s.cluster.Name, node, faultTransitionMetric, labels, 2, faultMetricGap)
			return stableErr
		}, faultConvergenceTimeout, faultPollInterval).Should(Succeed())
		stableValues[node] = stable
		observed++

		beforeValue, beforeMatches := e2e.MetricValue(before[node], faultTransitionMetric, labels)
		stableDelta := stable
		if beforeMatches == 1 {
			rawDelta, ok := e2e.CounterDelta(before[node], metrics, faultTransitionMetric, labels)
			Expect(ok).To(BeTrue())
			By(withTimestamp(fmt.Sprintf("transition counter on %q changed by %.0f during %s before settling", node, rawDelta, fault)))
			stableDelta = stable - beforeValue
			if stableDelta < 0 {
				stableDelta = stable
			}
		}
		Expect(stableDelta).To(BeNumerically(">=", float64(0)))
		totalDelta += stableDelta
	}

	Expect(observed).To(BeNumerically(">=", 1))
	By(withTimestamp(fmt.Sprintf("stable transition counters after %s: %v; delta %.0f", fault, stableValues, totalDelta)))
	Expect(totalDelta).To(BeNumerically("<=", faultTransitionDeltaLimit))
}
