//go:build e2e
// +build e2e

package e2e_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	kindconfigv1alpha4 "sigs.k8s.io/kind/pkg/apis/config/v1alpha4"
	"sigs.k8s.io/kind/pkg/cluster/nodes"

	"github.com/kube-vip/kube-vip/pkg/utils"
	"github.com/kube-vip/kube-vip/testing/e2e"
)

var _ = Describe("kube-vip elected VIP failover", Ordered, func() {
	if Mode == ModeARP {
		var (
			kindCluster *e2e.Cluster
			tempDirPath string
			controlVIP  string
		)

		BeforeAll(func(ctx context.Context) {
			tempDirPath = MustMkdirTemp("", fmt.Sprintf("%s-vip-failover-", testDirPrefix))
			controlVIP = e2e.GenerateVIP(utils.IPv4Family, SOffset.Get(), defaultNetwork)
			kindCluster = e2e.CreateCluster(ctx, &e2e.ClusterSpec{
				Name:       filepath.Base(tempDirPath),
				Nodes:      2,
				Networking: kindconfigv1alpha4.Networking{IPFamily: kindconfigv1alpha4.IPv4Family},
				Logger:     e2e.TestLogger{},
				ConfigMtx:  ConfigMtx,
				KubeVip: e2e.KubevipManifestValues{
					ControlPlaneVIP:       controlVIP,
					ControlPlaneEnable:    "true",
					SvcEnable:             "true",
					SvcElectionEnable:     "true",
					VipElectionEnable:     "true",
					EnableEndpoints:       "true",
					EnableNodeLabeling:    "false",
					EnableServiceSecurity: "true",
				},
			})

			if err := e2e.LoadDockerImageToKind(e2e.TestLogger{}, "ghcr.io/traefik/whoami:v1.11", kindCluster.Name); err != nil {
				By(fmt.Sprintf("test image will be downloaded by the DaemonSet: %v", err))
			}
			createTestDS(ctx, kindCluster.Client, dsNamespace, dsName, 1)
		})

		AfterAll(func(ctx context.Context) {
			if kindCluster != nil {
				SaveLogsAndCleanup(ctx, kindCluster.Client, tempDirPath, kindCluster.Name, kindCluster.Delete)
			}
			if os.Getenv("E2E_KEEP_LOGS") != "true" {
				_ = os.RemoveAll(tempDirPath)
			}
		})

		It("moves the control-plane VIP to the other node and back", func(ctx context.Context) {
			firstNode := findVIPOwner(controlVIP, kindCluster.Nodes)
			exerciseBidirectionalVIPHandoff(ctx, kindCluster, controlVIP, firstNode, "", "")
		})

		It("moves a per-Service VIP to the other node and back", func(ctx context.Context) {
			const serviceName = "vip-failover-service"
			serviceVIP := e2e.GenerateVIP(utils.IPv4Family, SOffset.Get(), defaultNetwork)
			serviceLease := "kubevip-" + serviceName
			createTestService(ctx, serviceName, dsNamespace, dsName, serviceVIP,
				kindCluster.Client, corev1.IPFamilyPolicySingleStack,
				[]corev1.IPFamily{corev1.IPv4Protocol}, corev1.ServiceExternalTrafficPolicyCluster,
				"", 80, false, false)
			DeferCleanup(func(ctx context.Context) {
				_ = kindCluster.Client.CoreV1().Services(dsNamespace).Delete(ctx, serviceName, v1.DeleteOptions{})
			})

			Expect(checkIPAddressByLease(ctx, serviceLease, dsNamespace, serviceVIP, true, kindCluster.Client)).To(BeTrue())
			firstNode := findFailoverNode(kindCluster.Nodes, e2e.GetLeaseHolder(ctx, serviceLease, dsNamespace, kindCluster.Client))
			exerciseBidirectionalVIPHandoff(ctx, kindCluster, serviceVIP, firstNode, serviceLease, dsNamespace)
		})
	}
})

func exerciseBidirectionalVIPHandoff(ctx context.Context, kindCluster *e2e.Cluster, address string, firstNode nodes.Node, leaseName, leaseNamespace string) {
	secondNode := otherFailoverNode(kindCluster.Nodes, firstNode)
	expectVIPOnlyOnNode(address, kindCluster.Nodes, firstNode)

	stopKubeVipForFailover(firstNode)
	if leaseName != "" {
		expectFailoverLeaseHolder(ctx, kindCluster.Client, leaseName, leaseNamespace, secondNode.String())
	}
	expectVIPOnlyOnNode(address, kindCluster.Nodes, secondNode)

	restoreKubeVipAfterFailover(firstNode)
	waitForKubeVipOnNode(ctx, kindCluster.Client, firstNode.String())
	stopKubeVipForFailover(secondNode)
	if leaseName != "" {
		expectFailoverLeaseHolder(ctx, kindCluster.Client, leaseName, leaseNamespace, firstNode.String())
	}
	expectVIPOnlyOnNode(address, kindCluster.Nodes, firstNode)

	restoreKubeVipAfterFailover(secondNode)
	waitForKubeVipOnNode(ctx, kindCluster.Client, secondNode.String())
	expectVIPOnlyOnNode(address, kindCluster.Nodes, firstNode)
}

func findVIPOwner(address string, clusterNodes []nodes.Node) nodes.Node {
	var owner nodes.Node
	Eventually(func() bool {
		owners := 0
		for _, node := range clusterNodes {
			if e2e.CheckIPAddressPresence(address, node.String(), true) {
				owner = node
				owners++
			}
		}
		return owners == 1
	}, 90*time.Second, time.Second).Should(BeTrue(), "exactly one Kind node should own VIP %s", address)
	return owner
}

func findFailoverNode(clusterNodes []nodes.Node, name string) nodes.Node {
	for _, node := range clusterNodes {
		if node.String() == name {
			return node
		}
	}
	Fail(fmt.Sprintf("no Kind node matches lease holder %q", name))
	return nil
}

func otherFailoverNode(clusterNodes []nodes.Node, owner nodes.Node) nodes.Node {
	for _, node := range clusterNodes {
		if node.String() != owner.String() {
			return node
		}
	}
	Fail("VIP failover E2E requires at least two Kind nodes")
	return nil
}

func expectVIPOnlyOnNode(address string, clusterNodes []nodes.Node, owner nodes.Node) {
	Eventually(func() bool {
		for _, node := range clusterNodes {
			if !e2e.CheckIPAddressPresence(address, node.String(), node.String() == owner.String()) {
				return false
			}
		}
		return true
	}, 90*time.Second, time.Second).Should(BeTrue(), "VIP %s should exist only on node %s", address, owner.String())
}

func expectFailoverLeaseHolder(ctx context.Context, client kubernetes.Interface, name, namespace, expected string) {
	Eventually(func() string {
		lease, err := client.CoordinationV1().Leases(namespace).Get(ctx, name, v1.GetOptions{})
		if err != nil || lease.Spec.HolderIdentity == nil {
			return ""
		}
		return *lease.Spec.HolderIdentity
	}, 90*time.Second, time.Second).Should(Equal(expected))
}

func stopKubeVipForFailover(node nodes.Node) {
	DeferCleanup(func() { restoreKubeVipAfterFailover(node) })
	By(fmt.Sprintf("stopping kube-vip on node %s", node.String()))
	e2e.RunInNode(node, "bash", "-c",
		"cp /etc/kubernetes/manifests/kube-vip.yaml /tmp/kube-vip-failover.yaml && truncate -s 0 /etc/kubernetes/manifests/kube-vip.yaml")
}

func restoreKubeVipAfterFailover(node nodes.Node) {
	e2e.RunInNode(node, "cp", "/tmp/kube-vip-failover.yaml", "/etc/kubernetes/manifests/kube-vip.yaml")
}

func waitForKubeVipOnNode(ctx context.Context, client kubernetes.Interface, nodeName string) {
	Eventually(func() bool {
		pods, err := client.CoreV1().Pods("kube-system").List(ctx, v1.ListOptions{
			FieldSelector: "spec.nodeName=" + nodeName,
			LabelSelector: "app=kube-vip",
		})
		if err != nil {
			return false
		}
		for _, pod := range pods.Items {
			if pod.Status.Phase == corev1.PodRunning {
				return true
			}
		}
		return false
	}, 90*time.Second, time.Second).Should(BeTrue(), "kube-vip should be running on node %s", nodeName)
}
