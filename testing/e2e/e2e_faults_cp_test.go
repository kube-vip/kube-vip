//go:build e2e
// +build e2e

package e2e_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	ctx            context.Context
	cancel         context.CancelFunc
	cluster        *e2e.Cluster
	client         kubernetes.Interface
	vip            string
	nodes          []string
	tempDir        string
	metrics        bool
	election       faultElectionMode
	suppressedNode string
}

type faultElectionMode struct {
	enabled   bool
	leaseName string
}

type faultMetricSnapshot map[string]map[string]float64

func controlPlaneElectionMode(mode string) faultElectionMode {
	if mode == ModeARP {
		return faultElectionMode{enabled: true, leaseName: faultLeaseName}
	}
	return faultElectionMode{}
}

func validateARPFailover(oldLeader, newLeader string, owners []string) error {
	if len(owners) == 0 {
		return fmt.Errorf("VIP has no owner while lease is held by %q", newLeader)
	}
	if newLeader != oldLeader {
		for _, owner := range owners {
			if owner == oldLeader {
				return fmt.Errorf("stale VIP ownership remains on former leader %q", oldLeader)
			}
		}
	}
	if len(owners) != 1 || owners[0] != newLeader {
		return fmt.Errorf("elected leader %q does not own VIP; owners are %v", newLeader, owners)
	}
	return nil
}

func validateARPTransfer(newLeader string, owners []string) error {
	for _, owner := range owners {
		if owner == newLeader {
			return nil
		}
	}
	return fmt.Errorf("replacement leader %q does not own VIP; owners are %v (a stale owner is expected until the stopped kubelet restarts)", newLeader, owners)
}

var _ = Describe("kube-vip control-plane election and VIP failover faults", Label("faults"), Serial, Ordered, func() {
	if Mode != ModeARP && Mode != ModeRT {
		return
	}

	suite := &controlPlaneFaultSuite{}

	BeforeAll(func() {
		suite.ctx, suite.cancel = context.WithCancel(context.Background())
		var err error
		suite.tempDir, err = os.MkdirTemp("", fmt.Sprintf("kube-vip-test-faults-%s-", Mode))
		Expect(err).NotTo(HaveOccurred())
		suite.election = controlPlaneElectionMode(Mode)

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
			PrometheusHTTPServer:  ":2112",
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
		suite.metrics = suite.metricsAvailable()

		By(withTimestamp("waiting for the control-plane VIP to become routable"))
		assertControlPlaneIsRoutable(suite.vip, 2*time.Second, faultConvergenceTimeout)
		if suite.election.enabled {
			leader := suite.waitForLeader()
			suite.waitForVIPOwner(leader)
			suite.assertLeaderMetric(leader)
			suite.assertOneLeader()
			suite.assertSteadyLeader(leader)
		} else {
			suite.assertNoElectionLease()
			By(withTimestamp(fmt.Sprintf("%s mode is configured without control-plane election; validating process and node recovery only", Mode)))
		}
	})

	AfterAll(func() {
		if suite.cluster != nil {
			By(withTimestamp(fmt.Sprintf("saving fault artifacts to %q", suite.tempDir)))
			if err := e2e.GetLogs(suite.ctx, suite.client, suite.tempDir, suite.cluster.Name); err != nil {
				By(withTimestamp(fmt.Sprintf("fault artifact collection failed: %v", err)))
			}
			suite.cluster.Delete()
		}
		if suite.cancel != nil {
			suite.cancel()
		}
		if suite.tempDir != "" && os.Getenv("E2E_KEEP_LOGS") != "true" {
			Expect(os.RemoveAll(suite.tempDir)).To(Succeed())
		}
	})

	AfterEach(func() {
		if suite.suppressedNode == "" || suite.cluster == nil {
			return
		}
		By(withTimestamp(fmt.Sprintf("restoring kubelet on %q before fault-suite teardown", suite.suppressedNode)))
		Expect(e2e.RestoreKubeVip(suite.cluster.Name, suite.suppressedNode)).To(Succeed())
		suite.suppressedNode = ""
	})

	It("keeps the VIP available while the leader loses API access and recovers", func() {
		if !suite.election.enabled {
			node := suite.nodes[0]
			By(withTimestamp(fmt.Sprintf("blackholing the API server from non-electing %s node %q", Mode, node)))
			Expect(e2e.BlackholeAPIServer(suite.cluster.Name, node)).To(Succeed())
			DeferCleanup(func() { Expect(e2e.RestoreAPIServer(suite.cluster.Name, node)).To(Succeed()) })
			assertControlPlaneIsRoutable(suite.vip, 2*time.Second, faultConvergenceTimeout)
			Expect(e2e.RestoreAPIServer(suite.cluster.Name, node)).To(Succeed())
			return
		}

		before := suite.transitionSnapshot()
		oldLeader := suite.waitForLeader()

		By(withTimestamp(fmt.Sprintf("blackholing the API server from leader %q", oldLeader)))
		Expect(e2e.BlackholeAPIServer(suite.cluster.Name, oldLeader)).To(Succeed())
		DeferCleanup(func() {
			Expect(e2e.RestoreAPIServer(suite.cluster.Name, oldLeader)).To(Succeed())
		})

		newLeader := suite.waitForDifferentLeader(oldLeader)
		assertControlPlaneIsRoutable(suite.vip, 2*time.Second, faultConvergenceTimeout)
		lease := suite.waitForLease(oldLeader)
		By(withTimestamp(fmt.Sprintf("lease %s/%s is held by remaining node %q while %q is blackholed", faultLeaseNamespace, suite.election.leaseName, *lease.Spec.HolderIdentity, oldLeader)))
		suite.assertLeaderMetric(newLeader)
		suite.assertOneLeader(oldLeader)

		By(withTimestamp(fmt.Sprintf("restoring the API server connection on %q", oldLeader)))
		Expect(e2e.RestoreAPIServer(suite.cluster.Name, oldLeader)).To(Succeed())

		suite.waitForLease()
		recoveredLeader := suite.waitForLeader()
		assertControlPlaneIsRoutable(suite.vip, 2*time.Second, faultConvergenceTimeout)
		suite.assertLeaderMetric(recoveredLeader)
		suite.assertOneLeader()
		suite.assertSteadyLeader(recoveredLeader)
		suite.assertTransitionCounterStable(before, "API server blackhole and recovery")
	})

	It("recovers after SIGKILL of the kube-vip leader", func() {
		if !suite.election.enabled {
			node := suite.nodes[0]
			oldPID, err := e2e.KubeVipPID(suite.cluster.Name, node)
			Expect(err).NotTo(HaveOccurred())
			By(withTimestamp(fmt.Sprintf("sending SIGKILL to kube-vip PID %s on non-electing %s node %q", oldPID, Mode, node)))
			killTime := time.Now()
			Expect(e2e.KillKubeVip(suite.cluster.Name, node, false)).To(Succeed())
			suite.waitForKubeVipRestart(node, oldPID)
			By(withTimestamp(fmt.Sprintf("kube-vip restarted on %q after %s", node, time.Since(killTime))))
			suite.assertNodeRunning(node)
			assertControlPlaneIsRoutable(suite.vip, 2*time.Second, faultConvergenceTimeout)
			return
		}

		before := suite.transitionSnapshot()
		oldLease := suite.waitForLease()
		oldLeader := *oldLease.Spec.HolderIdentity
		suite.waitForVIPOwner(oldLeader)

		By(withTimestamp(fmt.Sprintf("stopping kubelet and sending SIGKILL to kube-vip on elected leader %q", oldLeader)))
		oldPID, err := e2e.KillAndSuppressKubeVip(suite.cluster.Name, oldLeader)
		Expect(err).NotTo(HaveOccurred())
		suite.suppressedNode = oldLeader
		By(withTimestamp(fmt.Sprintf("sent SIGKILL to kube-vip PID %s on elected leader %q", oldPID, oldLeader)))
		suite.assertNodeRunning(oldLeader)

		newLeader := suite.waitForDifferentLeader(oldLeader)
		suite.waitForARPTransfer(newLeader)
		assertControlPlaneIsRoutable(suite.vip, 2*time.Second, faultConvergenceTimeout)
		suite.assertLeaderMetric(newLeader)
		owners, err := suite.vipOwners()
		Expect(err).NotTo(HaveOccurred())
		By(withTimestamp(fmt.Sprintf("lease and reachability transferred to %q while kubelet is stopped on %q; current VIP owners: %v", newLeader, oldLeader, owners)))

		By(withTimestamp(fmt.Sprintf("restarting kubelet to restore kube-vip on %q", oldLeader)))
		Expect(e2e.RestoreKubeVip(suite.cluster.Name, oldLeader)).To(Succeed())
		suite.suppressedNode = ""
		suite.waitForKubeVipRestart(oldLeader, oldPID)
		suite.waitForNonLeader(oldLeader, newLeader)
		suite.waitForARPFailover(oldLeader, newLeader)
		By(withTimestamp(fmt.Sprintf("waiting for kube-vip metrics to return on restarted node %q", oldLeader)))
		suite.waitForMetrics(oldLeader)
		recoveredLeader := suite.waitForLeader()
		suite.assertLeaderMetric(recoveredLeader)
		suite.assertOneLeader()
		suite.assertSteadyLeader(recoveredLeader)
		suite.assertTransitionCounterStable(before, "SIGKILL and kube-vip restart")
	})

	It("reacquires the lease after deletion and lease stealing", func() {
		if !suite.election.enabled {
			Skip(fmt.Sprintf("%s control-plane mode does not use a Kubernetes election lease", Mode))
		}

		beforeDelete := suite.transitionSnapshot()
		oldLease := suite.getLease()
		oldUID := string(oldLease.UID)

		By(withTimestamp(fmt.Sprintf("deleting lease %s/%s", faultLeaseNamespace, suite.election.leaseName)))
		Expect(e2e.DeleteLease(suite.ctx, suite.client, faultLeaseNamespace, suite.election.leaseName)).To(Succeed())
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
		By(withTimestamp(fmt.Sprintf("overwriting lease %s/%s with holder %q", faultLeaseNamespace, suite.election.leaseName, stolenHolder)))
		Eventually(func() error {
			return e2e.StealLease(suite.ctx, suite.client, faultLeaseNamespace, suite.election.leaseName, stolenHolder)
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
		if !suite.election.enabled {
			node := suite.nodes[0]
			By(withTimestamp(fmt.Sprintf("restarting non-electing %s node %q", Mode, node)))
			Expect(e2e.RestartNode(suite.cluster.Name, node)).To(Succeed())
			suite.waitForNodeReady(node)
			suite.waitForKubeVip(node)
			assertControlPlaneIsRoutable(suite.vip, 2*time.Second, faultConvergenceTimeout)
			return
		}

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
	Expect(s.election.enabled).To(BeTrue(), "%s mode has no control-plane election leader", Mode)
	return *s.waitForLease().Spec.HolderIdentity
}

func (s *controlPlaneFaultSuite) waitForDifferentLeader(oldLeader string) string {
	return *s.waitForLease(oldLeader).Spec.HolderIdentity
}

func (s *controlPlaneFaultSuite) waitForARPFailover(oldLeader, newLeader string) {
	Eventually(func() error {
		lease, err := s.client.CoordinationV1().Leases(faultLeaseNamespace).Get(s.ctx, s.election.leaseName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("observe election transfer: %w", err)
		}
		if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity == "" {
			return fmt.Errorf("lease %s/%s has no holder", faultLeaseNamespace, s.election.leaseName)
		}
		if *lease.Spec.HolderIdentity != newLeader {
			return fmt.Errorf("lease %s/%s is held by %q, want %q", faultLeaseNamespace, s.election.leaseName, *lease.Spec.HolderIdentity, newLeader)
		}
		owners, err := s.vipOwners()
		if err != nil {
			return err
		}
		return validateARPFailover(oldLeader, newLeader, owners)
	}, faultConvergenceTimeout, faultPollInterval).Should(Succeed())
	By(withTimestamp(fmt.Sprintf("election and sole VIP ownership transferred from %q to %q", oldLeader, newLeader)))
}

func (s *controlPlaneFaultSuite) waitForARPTransfer(newLeader string) {
	Eventually(func() error {
		lease, err := s.client.CoordinationV1().Leases(faultLeaseNamespace).Get(s.ctx, s.election.leaseName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("observe election transfer while kubelet is stopped: %w", err)
		}
		if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != newLeader {
			return fmt.Errorf("lease %s/%s is not held by replacement leader %q", faultLeaseNamespace, s.election.leaseName, newLeader)
		}
		owners, err := s.vipOwners()
		if err != nil {
			return err
		}
		return validateARPTransfer(newLeader, owners)
	}, faultConvergenceTimeout, faultPollInterval).Should(Succeed())
}

func (s *controlPlaneFaultSuite) waitForNonLeader(node, leader string) {
	Eventually(func() error {
		lease, err := s.client.CoordinationV1().Leases(faultLeaseNamespace).Get(s.ctx, s.election.leaseName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if lease.Spec.HolderIdentity == nil {
			return fmt.Errorf("replacement kube-vip on %q is not confirmed as a nonleader: lease %s/%s has no holder", node, faultLeaseNamespace, s.election.leaseName)
		}
		if *lease.Spec.HolderIdentity != leader {
			return fmt.Errorf("replacement kube-vip on %q is not a nonleader: lease %s/%s holder is %q, want %q", node, faultLeaseNamespace, s.election.leaseName, *lease.Spec.HolderIdentity, leader)
		}
		return nil
	}, faultConvergenceTimeout, faultPollInterval).Should(Succeed())
	By(withTimestamp(fmt.Sprintf("replacement kube-vip on %q started as nonleader behind %q", node, leader)))
}

func (s *controlPlaneFaultSuite) waitForVIPOwner(want string) {
	Eventually(func() error {
		owners, err := s.vipOwners()
		if err != nil {
			return err
		}
		if len(owners) != 1 || owners[0] != want {
			return fmt.Errorf("VIP %s owners are %v, want only %q", s.vip, owners, want)
		}
		return nil
	}, faultConvergenceTimeout, faultPollInterval).Should(Succeed())
}

func (s *controlPlaneFaultSuite) vipOwners() ([]string, error) {
	owners := make([]string, 0, len(s.nodes))
	for _, node := range s.nodes {
		var output bytes.Buffer
		cmd := exec.Command("docker", "exec", node, "ip", "-o", "addr", "show", "to", s.vip+"/32")
		cmd.Stdout = &output
		cmd.Stderr = &output
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("inspect VIP %s on node %q: %w: %s", s.vip, node, err, strings.TrimSpace(output.String()))
		}
		if strings.TrimSpace(output.String()) != "" {
			owners = append(owners, node)
		}
	}
	return owners, nil
}

func (s *controlPlaneFaultSuite) waitForKubeVipRestart(node, oldPID string) {
	Eventually(func() (string, error) {
		pid, err := e2e.KubeVipPID(s.cluster.Name, node)
		if err != nil {
			return "", err
		}
		if pid == oldPID {
			return "", fmt.Errorf("kube-vip PID %s is still running on %q", oldPID, node)
		}
		return pid, nil
	}, faultConvergenceTimeout, faultPollInterval).ShouldNot(BeEmpty())
}

func (s *controlPlaneFaultSuite) waitForKubeVip(node string) {
	Eventually(func() error {
		_, err := e2e.KubeVipPID(s.cluster.Name, node)
		return err
	}, faultConvergenceTimeout, faultPollInterval).Should(Succeed())
}

func (s *controlPlaneFaultSuite) assertNodeRunning(node string) {
	running, err := e2e.NodeRunning(s.cluster.Name, node)
	Expect(err).NotTo(HaveOccurred())
	Expect(running).To(BeTrue(), "SIGKILL must stop kube-vip, not its node container")
}

func (s *controlPlaneFaultSuite) assertNoElectionLease() {
	_, err := s.client.CoordinationV1().Leases(faultLeaseNamespace).Get(s.ctx, faultLeaseName, metav1.GetOptions{})
	Expect(apierrors.IsNotFound(err)).To(BeTrue(), "non-electing %s mode unexpectedly exposed control-plane lease %s/%s: %v", Mode, faultLeaseNamespace, faultLeaseName, err)
}

func (s *controlPlaneFaultSuite) getLease() *coordinationv1.Lease {
	lease, err := s.client.CoordinationV1().Leases(faultLeaseNamespace).Get(s.ctx, s.election.leaseName, metav1.GetOptions{})
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
		lease, err := s.client.CoordinationV1().Leases(faultLeaseNamespace).Get(s.ctx, s.election.leaseName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity == "" {
			return fmt.Errorf("lease %s/%s has no holder", faultLeaseNamespace, s.election.leaseName)
		}
		if _, skip := excluded[*lease.Spec.HolderIdentity]; skip {
			return fmt.Errorf("lease %s/%s is still held by excluded node %q", faultLeaseNamespace, s.election.leaseName, *lease.Spec.HolderIdentity)
		}
		current = lease
		return nil
	}, faultConvergenceTimeout, faultPollInterval).Should(Succeed())
	return current
}

func (s *controlPlaneFaultSuite) waitForLeaseHolder(holder string) *coordinationv1.Lease {
	var current *coordinationv1.Lease
	Eventually(func() error {
		lease, err := s.client.CoordinationV1().Leases(faultLeaseNamespace).Get(s.ctx, s.election.leaseName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != holder {
			return fmt.Errorf("lease %s/%s is not held by %q", faultLeaseNamespace, s.election.leaseName, holder)
		}
		current = lease
		return nil
	}, faultLeaseObservationLimit, faultPollInterval).Should(Succeed())
	return current
}

func (s *controlPlaneFaultSuite) waitForMetrics(node string) {
	if !s.metrics {
		return
	}
	Eventually(func() error {
		_, err := e2e.ScrapeMetrics(s.ctx, s.cluster.Name, node)
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
	if !s.metrics {
		return
	}
	e2e.EventuallyMetric(
		s.ctx,
		s.cluster.Name,
		leader,
		"kube_vip_is_leader",
		map[string]string{"node": leader, "lease_name": s.election.leaseName},
		Equal(float64(1)),
		faultConvergenceTimeout,
		faultPollInterval,
	)
}

func (s *controlPlaneFaultSuite) assertSteadyLeader(leader string) {
	if !s.metrics {
		return
	}
	e2e.ConsistentlyMetric(
		s.ctx,
		s.cluster.Name,
		leader,
		"kube_vip_is_leader",
		map[string]string{"node": leader, "lease_name": s.election.leaseName},
		Equal(float64(1)),
		faultSteadyStateWindow,
		faultPollInterval,
	)
}

func (s *controlPlaneFaultSuite) assertOneLeader(skipNodes ...string) {
	if !s.metrics {
		return
	}
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
			metrics, err := e2e.ScrapeMetrics(s.ctx, s.cluster.Name, node)
			if err != nil {
				return 0, err
			}
			value, ok := e2e.MaxMetric(metrics, "kube_vip_is_leader", map[string]string{"lease_name": s.election.leaseName})
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
	if !s.metrics {
		return nil
	}
	snapshot := make(faultMetricSnapshot, len(s.nodes))
	for _, node := range s.nodes {
		var metrics map[string]float64
		Eventually(func() error {
			var err error
			metrics, err = e2e.ScrapeMetrics(s.ctx, s.cluster.Name, node)
			return err
		}, faultConvergenceTimeout, faultPollInterval).Should(Succeed())
		snapshot[node] = metrics
	}
	return snapshot
}

func (s *controlPlaneFaultSuite) assertTransitionCounterStable(before faultMetricSnapshot, fault string) {
	if !s.metrics {
		return
	}
	labels := map[string]string{"lease_name": s.election.leaseName}
	stableValues := make(map[string]float64)
	totalDelta := 0.0
	observed := 0

	for _, node := range s.nodes {
		metrics, err := e2e.ScrapeMetrics(s.ctx, s.cluster.Name, node)
		Expect(err).NotTo(HaveOccurred())
		if _, matches := e2e.MetricValue(metrics, faultTransitionMetric, labels); matches == 0 {
			continue
		}

		var stable float64
		Eventually(func() error {
			var stableErr error
			stable, stableErr = e2e.MetricStable(s.ctx, s.cluster.Name, node, faultTransitionMetric, labels, 2, faultMetricGap)
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

func (s *controlPlaneFaultSuite) metricsAvailable() bool {
	for _, node := range s.nodes {
		if _, err := e2e.ScrapeMetrics(s.ctx, s.cluster.Name, node); err != nil {
			By(withTimestamp(fmt.Sprintf("metrics unavailable on %q; continuing with functional assertions: %v", node, err)))
			return false
		}
	}
	return true
}
