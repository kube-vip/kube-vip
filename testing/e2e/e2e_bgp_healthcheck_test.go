//go:build e2e
// +build e2e

package e2e_test

import (
	"context"
	"crypto/tls"
	"fmt"
	"math/rand/v2"
	"net/http"
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/klog/v2"
	kindconfigv1alpha4 "sigs.k8s.io/kind/pkg/apis/config/v1alpha4"

	"github.com/kube-vip/kube-vip/pkg/utils"
	"github.com/kube-vip/kube-vip/testing/e2e"
	"github.com/kube-vip/kube-vip/testing/e2e/bgp"
)

var _ = Describe("kube-vip BGP ControlPlane health-check", Ordered, func() {
	if Mode != ModeBGP {
		return
	}

	var (
		server      *bgp.Server
		kindCluster *e2e.Cluster
		cpVIP       string
		peers       []*e2e.BGPPeerValues
		tempDirRoot string
		httpClient  *http.Client
	)

	BeforeAll(func(ctx context.Context) {
		klog.SetOutput(GinkgoWriter)

		server = sharedBGPServer
		Expect(server).ToNot(BeNil(), "SharedBGPServer not initialized")

		var err error
		tempDirRoot, err = os.MkdirTemp("", "kube-vip-test-bgp-hc")
		Expect(err).NotTo(HaveOccurred())

		cpVIP = e2e.GenerateVIP(utils.IPv4Family, SOffset.Get())
		kvPeers := []*e2e.BGPPeerValues{
			{IP: server.LocalIPv4, AS: bgp.GoBGPAS, IPFamily: utils.IPv4Family},
		}

		kindCluster = e2e.CreateCluster(ctx, &e2e.ClusterSpec{
			Name:       "bgp-hc",
			Nodes:      2,
			Networking: kindconfigv1alpha4.Networking{IPFamily: kindconfigv1alpha4.IPv4Family},
			Logger:     e2e.TestLogger{},
			ConfigMtx:  ConfigMtx,
			KubeVip: e2e.KubevipManifestValues{
				ControlPlaneVIP:                         cpVIP,
				ControlPlaneEnable:                      "true",
				SvcEnable:                               "false",
				BGPAS:                                   bgp.KubevipAS,
				BGPPeers:                                bgp.PeerStrings(kvPeers),
				ControlPlaneHealthCheckAddress:          "https://localhost:6443/livez",
				ControlPlaneHealthCheckPeriodSeconds:    1,
				ControlPlaneHealthCheckTimeoutSeconds:   1,
				ControlPlaneHealthCheckFailureThreshold: 3,
				ControlPlaneHealthCheckCAPath:           "/etc/kubernetes/pki/ca.crt",
			},
		})

		peers = server.AddClusterPeers(ctx, kindCluster.Nodes, bgp.KubevipAS, []string{utils.IPv4Family})

		httpClient = &http.Client{
			Timeout:   3 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec
		}
	})

	AfterAll(func(ctx context.Context) {
		if server != nil {
			server.RemovePeers(ctx, peers)
		}
		if kindCluster != nil {
			kindCluster.Delete()
		}
		if os.Getenv("E2E_KEEP_LOGS") != "true" {
			_ = os.RemoveAll(tempDirRoot)
		}
	})

	When("all nodes are healthy", func() {
		It("announces BGP routes for all nodes", func(ctx context.Context) {
			bgp.CheckPathCount(ctx, server.Client, cpVIP, len(kindCluster.Nodes), 30*time.Second)
			assertVIPReachable(ctx, server, cpVIP, httpClient)
		})
	})

	When("the apiserver is stopped on a node", func() {
		It("withdraws and re-announces the BGP route", func(ctx context.Context) {
			node := kindCluster.Nodes[rand.IntN(len(kindCluster.Nodes))]

			By(fmt.Sprintf("stopping the apiserver on node %s", node.String()))
			e2e.RunInNode(node, "mv",
				"/etc/kubernetes/manifests/kube-apiserver.yaml",
				"/tmp/kube-apiserver.yaml")

			bgp.CheckPathCount(ctx, server.Client, cpVIP, len(kindCluster.Nodes)-1, 30*time.Second)
			assertVIPReachable(ctx, server, cpVIP, httpClient)

			By(fmt.Sprintf("restoring the apiserver on node %s", node.String()))
			e2e.RunInNode(node, "mv",
				"/tmp/kube-apiserver.yaml",
				"/etc/kubernetes/manifests/kube-apiserver.yaml")

			bgp.CheckPathCount(ctx, server.Client, cpVIP, len(kindCluster.Nodes), 120*time.Second)
			assertVIPReachable(ctx, server, cpVIP, httpClient)
		})
	})

	When("kube-vip is stopped on a node", func() {
		It("withdraws and re-announces the BGP route via graceful shutdown", func(ctx context.Context) {
			node := kindCluster.Nodes[rand.IntN(len(kindCluster.Nodes))]

			By(fmt.Sprintf("stopping kube-vip on node %s", node.String()))
			e2e.RunInNode(node, "bash", "-c",
				"cp /etc/kubernetes/manifests/kube-vip.yaml /tmp/kube-vip.yaml && truncate -s 0 /etc/kubernetes/manifests/kube-vip.yaml")

			bgp.CheckPathCount(ctx, server.Client, cpVIP, len(kindCluster.Nodes)-1, 30*time.Second)
			assertVIPReachable(ctx, server, cpVIP, httpClient)

			By(fmt.Sprintf("restoring kube-vip on node %s", node.String()))
			e2e.RunInNode(node, "bash", "-c",
				"cp /tmp/kube-vip.yaml /etc/kubernetes/manifests/kube-vip.yaml")

			bgp.CheckPathCount(ctx, server.Client, cpVIP, len(kindCluster.Nodes), 120*time.Second)
			assertVIPReachable(ctx, server, cpVIP, httpClient)
		})
	})
})

// assertVIPReachable resolves the VIP through GoBGP to get the current
// next-hops, then makes 10 rapid requests to each next-hop's apiserver.
// This simulates what a real BGP router would do — only forward to nodes
// that are currently announcing the route.
func assertVIPReachable(ctx context.Context, server *bgp.Server, vip string, httpClient *http.Client) {
	By("verifying apiserver reachable via BGP next-hops (10 requests each)")
	for i := range 10 {
		nexthops := bgp.ResolveVIP(ctx, server.Client, vip)
		Expect(nexthops).ToNot(BeEmpty(), "no BGP next-hops for VIP %s", vip)

		for _, nextHop := range nexthops {
			url := fmt.Sprintf("https://%s:6443/version", nextHop)
			resp, err := httpClient.Get(url)
			Expect(err).ToNot(HaveOccurred(), "request %d to next-hop %s failed", i+1, nextHop)
			resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusOK),
				"request %d to next-hop %s returned status %d", i+1, nextHop, resp.StatusCode)
		}
	}
}
