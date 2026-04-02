//go:build e2e
// +build e2e

package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	kindconfigv1alpha4 "sigs.k8s.io/kind/pkg/apis/config/v1alpha4"
	"sigs.k8s.io/kind/pkg/log"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/kube-vip/kube-vip/pkg/utils"
	"github.com/kube-vip/kube-vip/pkg/vip"
	"github.com/kube-vip/kube-vip/testing/e2e"
	"github.com/kube-vip/kube-vip/testing/e2e/bgp"

	api "github.com/osrg/gobgp/v3/api"
)

const (
	defaultFixedNexthopv6 = "fc00:1000:1000:1000::100"
	defaultFixedNexthopv4 = "172.18.0.100"
)

var _ = Describe("kube-vip BGP mode", Ordered, func() {
	if Mode == ModeBGP {
		var (
			logger log.Logger
			server *bgp.Server
		)

		ctx, cancel := context.WithCancel(context.TODO())

		BeforeAll(func() {
			klog.SetOutput(GinkgoWriter)
			logger = e2e.TestLogger{}
			server = sharedBGPServer
			Expect(server).ToNot(BeNil(), "SharedBGPServer not initialized")
		})

		AfterAll(func() {
			cancel()
		})

		Describe("kube-vip IPv4 control-plane BGP mode functionality", Ordered, func() {
			var (
				cpVIP       string
				kindCluster *e2e.Cluster
				gobgpClient api.GobgpApiClient
				gobgpPeers  []*e2e.BGPPeerValues
				tempDirPath string

				nodesNumber = 1
			)

			BeforeAll(func() {
				var err error
				tempDirPath, err = os.MkdirTemp(server.TempDir, "kube-vip-test")
				Expect(err).NotTo(HaveOccurred())
				kindCluster, _, _ = setupEnv(ctx, &cpVIP,
					server, utils.IPv4Family, utils.IPv4Family,
					[]string{utils.IPv4Family}, &gobgpPeers,
					&gobgpClient, logger, nodesNumber, "", "bgp-cp-ipv4", false, true, false)
			})

			AfterAll(func() {
				By(fmt.Sprintf("saving logs to %q", tempDirPath))
				err := e2e.GetLogs(ctx, kindCluster.Client, tempDirPath, kindCluster.Name)
				Expect(err).ToNot(HaveOccurred())
				server.RemovePeers(ctx, gobgpPeers)
				kindCluster.Delete()
			})

			It("should add paths", func() {
				addresses := strings.Split(cpVIP, ",")
				testControlPlaneBGP(ctx, gobgpClient, addresses)
			})
		})

		Describe("kube-vip IPv6 control-plane BGP mode functionality", Ordered, func() {
			var (
				cpVIP       string
				kindCluster *e2e.Cluster
				gobgpClient api.GobgpApiClient
				gobgpPeers  []*e2e.BGPPeerValues
				tempDirPath string

				nodesNumber = 1
			)

			BeforeAll(func() {
				var err error
				tempDirPath, err = os.MkdirTemp(server.TempDir, "kube-vip-test")
				Expect(err).NotTo(HaveOccurred())
				kindCluster, _, _ = setupEnv(ctx, &cpVIP,
					server, utils.IPv6Family, utils.IPv6Family,
					[]string{utils.IPv6Family}, &gobgpPeers,
					&gobgpClient, logger, nodesNumber, "", "bgp-cp-ipv4", false, true, false)
			})

			AfterAll(func() {
				By(fmt.Sprintf("saving logs to %q", tempDirPath))
				err := e2e.GetLogs(ctx, kindCluster.Client, tempDirPath, kindCluster.Name)
				Expect(err).ToNot(HaveOccurred())
				server.RemovePeers(ctx, gobgpPeers)
				kindCluster.Delete()
			})

			It("should add paths", func() {
				addresses := strings.Split(cpVIP, ",")
				testControlPlaneBGP(ctx, gobgpClient, addresses)
			})
		})

		Describe("kube-vip DualStack control-plane BGP mode functionality, IPv6 over IPv4", Ordered, func() {
			var (
				cpVIP       string
				kindCluster *e2e.Cluster
				gobgpClient api.GobgpApiClient
				gobgpPeers  []*e2e.BGPPeerValues
				tempDirPath string

				nodesNumber = 1
			)

			BeforeAll(func() {
				var err error
				tempDirPath, err = os.MkdirTemp(server.TempDir, "kube-vip-test")
				Expect(err).NotTo(HaveOccurred())
				kindCluster, _, _ = setupEnv(ctx, &cpVIP,
					server, e2e.DualstackFamily, utils.IPv4Family,
					[]string{utils.IPv4Family}, &gobgpPeers,
					&gobgpClient, logger, nodesNumber, "fixed", "cp-mpbgp-ipv4", false, true, false)
			})

			AfterAll(func() {
				By(fmt.Sprintf("saving logs to %q", tempDirPath))
				err := e2e.GetLogs(ctx, kindCluster.Client, tempDirPath, kindCluster.Name)
				Expect(err).ToNot(HaveOccurred())
				server.RemovePeers(ctx, gobgpPeers)
				kindCluster.Delete()
			})

			It("should add paths", func() {
				addresses := strings.Split(cpVIP, ",")
				testControlPlaneBGP(ctx, gobgpClient, addresses)
			})
		})

		Describe("kube-vip DualStack control-plane BGP mode functionality, IPv4 over IPv6", Ordered, func() {
			var (
				cpVIP       string
				kindCluster *e2e.Cluster
				gobgpClient api.GobgpApiClient
				gobgpPeers  []*e2e.BGPPeerValues
				tempDirPath string

				nodesNumber = 1
			)

			BeforeAll(func() {
				var err error
				tempDirPath, err = os.MkdirTemp(server.TempDir, "kube-vip-test")
				Expect(err).NotTo(HaveOccurred())
				kindCluster, _, _ = setupEnv(ctx, &cpVIP,
					server, e2e.DualstackFamilyIPv6, utils.IPv6Family,
					[]string{utils.IPv6Family}, &gobgpPeers,
					&gobgpClient, logger, nodesNumber, "fixed", "cp-mpbgp-ipv6", false, true, false)
			})

			AfterAll(func() {
				By(fmt.Sprintf("saving logs to %q", tempDirPath))
				err := e2e.GetLogs(ctx, kindCluster.Client, tempDirPath, kindCluster.Name)
				Expect(err).ToNot(HaveOccurred())
				server.RemovePeers(ctx, gobgpPeers)
				kindCluster.Delete()
			})

			It("should add paths", func() {
				addresses := strings.Split(cpVIP, ",")
				testControlPlaneBGP(ctx, gobgpClient, addresses)
			})
		})

		Describe("kube-vip IPv4 services BGP mode functionality", Ordered, func() {
			var (
				cpVIP       string
				kindCluster *e2e.Cluster
				gobgpClient api.GobgpApiClient
				gobgpPeers  []*e2e.BGPPeerValues
				tempDirPath string
				nodesNumber = 1
			)

			BeforeAll(func() {
				var err error
				tempDirPath, err = os.MkdirTemp(server.TempDir, "kube-vip-test")
				Expect(err).NotTo(HaveOccurred())
				kindCluster, _, _ = setupEnv(ctx, &cpVIP,
					server, utils.IPv4Family, utils.IPv4Family,
					[]string{utils.IPv4Family}, &gobgpPeers,
					&gobgpClient, logger, nodesNumber, "", "bgp-ipv4", false, false, true)
			})

			AfterAll(func() {
				By(fmt.Sprintf("saving logs to %q", tempDirPath))
				err := e2e.GetLogs(ctx, kindCluster.Client, tempDirPath, kindCluster.Name)
				Expect(err).ToNot(HaveOccurred())
				server.RemovePeers(ctx, gobgpPeers)
				kindCluster.Delete()
			})

			DescribeTable("advertise IPv4 routes for services",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv4Family, api.Family_AFI_IP, []corev1.IPFamily{corev1.IPv4Protocol}, svcName,
						trafficPolicy, kindCluster.Client, 1, gobgpClient, "", "")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only stops advertising route if it was referenced by multiple services and all of them were deleted",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv4Family, api.Family_AFI_IP, []corev1.IPFamily{corev1.IPv4Protocol}, svcName,
						trafficPolicy, kindCluster.Client, 2, gobgpClient, "", "")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)
		})

		Describe("kube-vip IPv6 services BGP mode functionality", Ordered, func() {
			var (
				cpVIP       string
				kindCluster *e2e.Cluster
				gobgpClient api.GobgpApiClient
				gobgpPeers  []*e2e.BGPPeerValues
				tempDirPath string
				nodesNumber = 1
			)

			BeforeAll(func() {
				var err error
				tempDirPath, err = os.MkdirTemp(server.TempDir, "kube-vip-test")
				Expect(err).NotTo(HaveOccurred())
				kindCluster, _, _ = setupEnv(ctx, &cpVIP,
					server, utils.IPv6Family, utils.IPv6Family,
					[]string{utils.IPv6Family}, &gobgpPeers,
					&gobgpClient, logger, nodesNumber, "", "bgp-ipv4", false, false, true)
			})

			AfterAll(func() {
				By(fmt.Sprintf("saving logs to %q", tempDirPath))
				err := e2e.GetLogs(ctx, kindCluster.Client, tempDirPath, kindCluster.Name)
				Expect(err).ToNot(HaveOccurred())
				server.RemovePeers(ctx, gobgpPeers)
				kindCluster.Delete()
			})

			DescribeTable("advertise IPv6 routes for services",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv6Family, api.Family_AFI_IP6, []corev1.IPFamily{corev1.IPv6Protocol}, svcName,
						trafficPolicy, kindCluster.Client, 1, gobgpClient, "", "")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only stops advertising route if it was referenced by multiple services and all of them were deleted",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv6Family, api.Family_AFI_IP6, []corev1.IPFamily{corev1.IPv6Protocol}, svcName,
						trafficPolicy, kindCluster.Client, 2, gobgpClient, "", "")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)
		})

		Describe("kube-vip DualStack services BGP mode functionality with MP-BGP IPv6 over IPv4 - fixed nexthop", Ordered, func() {
			var (
				cpVIP       string
				kindCluster *e2e.Cluster
				gobgpClient api.GobgpApiClient
				gobgpPeers  []*e2e.BGPPeerValues
				tempDirPath string
				nodesNumber = 1
			)

			BeforeAll(func() {
				var err error
				tempDirPath, err = os.MkdirTemp(server.TempDir, "kube-vip-test")
				Expect(err).NotTo(HaveOccurred())
				kindCluster, _, _ = setupEnv(ctx, &cpVIP,
					server, e2e.DualstackFamily, utils.IPv4Family,
					[]string{utils.IPv4Family}, &gobgpPeers,
					&gobgpClient, logger, nodesNumber, "fixed", "mpbgp-ipv4", false, false, true)
			})

			AfterAll(func() {
				By(fmt.Sprintf("saving logs to %q", tempDirPath))
				err := e2e.GetLogs(ctx, kindCluster.Client, tempDirPath, kindCluster.Name)
				Expect(err).ToNot(HaveOccurred())
				server.RemovePeers(ctx, gobgpPeers)
				kindCluster.Delete()
			})

			DescribeTable("advertise IPv6 routes over IPv4 session",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv6Family, api.Family_AFI_IP6, []corev1.IPFamily{corev1.IPv6Protocol}, svcName,
						trafficPolicy, kindCluster.Client, 1, gobgpClient, defaultFixedNexthopv6, "")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only stops advertising route if it was referenced by multiple services and all of them were deleted",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv6Family, api.Family_AFI_IP6, []corev1.IPFamily{corev1.IPv6Protocol}, svcName,
						trafficPolicy, kindCluster.Client, 2, gobgpClient, defaultFixedNexthopv6, "")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)
		})

		Describe("kube-vip DualStack services BGP mode functionality with MP-BGP IPv4 over IPv6 - fixed nexthop", Ordered, func() {
			var (
				cpVIP       string
				kindCluster *e2e.Cluster
				gobgpClient api.GobgpApiClient
				gobgpPeers  []*e2e.BGPPeerValues
				tempDirPath string
				nodesNumber = 1
			)

			BeforeAll(func() {
				var err error
				tempDirPath, err = os.MkdirTemp(server.TempDir, "kube-vip-test")
				Expect(err).NotTo(HaveOccurred())
				kindCluster, _, _ = setupEnv(ctx, &cpVIP,
					server, e2e.DualstackFamilyIPv6, utils.IPv6Family,
					[]string{utils.IPv6Family}, &gobgpPeers,
					&gobgpClient, logger, nodesNumber, "fixed", "mpbgp-ipv6", false, false, true)
			})

			AfterAll(func() {
				By(fmt.Sprintf("saving logs to %q", tempDirPath))
				err := e2e.GetLogs(ctx, kindCluster.Client, tempDirPath, kindCluster.Name)
				Expect(err).ToNot(HaveOccurred())
				server.RemovePeers(ctx, gobgpPeers)
				kindCluster.Delete()
			})

			DescribeTable("advertise IPv4 routes over IPv6 session",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv4Family, api.Family_AFI_IP, []corev1.IPFamily{corev1.IPv4Protocol}, svcName,
						trafficPolicy, kindCluster.Client, 1, gobgpClient, defaultFixedNexthopv4, "")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only stops advertising route if it was referenced by multiple services and all of them were deleted",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv4Family, api.Family_AFI_IP, []corev1.IPFamily{corev1.IPv4Protocol}, svcName,
						trafficPolicy, kindCluster.Client, 2, gobgpClient, defaultFixedNexthopv4, "")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)
		})

		Describe("kube-vip DualStack services BGP mode functionality with MP-BGP IPv6 over IPv4 - auto_sourceif nexthop", Ordered, func() {
			var (
				cpVIP       string
				kindCluster *e2e.Cluster
				gobgpClient api.GobgpApiClient
				gobgpPeers  []*e2e.BGPPeerValues
				containerIP string
				tempDirPath string
				nodesNumber = 1
			)

			BeforeAll(func() {
				var err error
				tempDirPath, err = os.MkdirTemp(server.TempDir, "kube-vip-test")
				Expect(err).NotTo(HaveOccurred())
				kindCluster, _, containerIP = setupEnv(ctx, &cpVIP,
					server, e2e.DualstackFamily, utils.IPv4Family,
					[]string{utils.IPv4Family}, &gobgpPeers,
					&gobgpClient, logger, nodesNumber, "auto_sourceif", "mpbgp-if-ipv4", false, false, true)
			})

			AfterAll(func() {
				By(fmt.Sprintf("saving logs to %q", tempDirPath))
				err := e2e.GetLogs(ctx, kindCluster.Client, tempDirPath, kindCluster.Name)
				Expect(err).ToNot(HaveOccurred())
				server.RemovePeers(ctx, gobgpPeers)
				kindCluster.Delete()
			})

			DescribeTable("advertise IPv6 routes over IPv4 session",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv6Family, api.Family_AFI_IP6, []corev1.IPFamily{corev1.IPv6Protocol}, svcName,
						trafficPolicy, kindCluster.Client, 1, gobgpClient, containerIP, "")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only stops advertising route if it was referenced by multiple services and all of them were deleted",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv6Family, api.Family_AFI_IP6, []corev1.IPFamily{corev1.IPv6Protocol}, svcName,
						trafficPolicy, kindCluster.Client, 2, gobgpClient, containerIP, "")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)
		})

		Describe("kube-vip DualStack services BGP mode functionality with MP-BGP IPv4 over IPv6 - auto_sourceif nexthop, auto source interface", Ordered, func() {
			var (
				cpVIP       string
				kindCluster *e2e.Cluster
				gobgpClient api.GobgpApiClient
				gobgpPeers  []*e2e.BGPPeerValues
				containerIP string
				tempDirPath string
				nodesNumber = 1
			)

			BeforeAll(func() {
				var err error
				tempDirPath, err = os.MkdirTemp(server.TempDir, "kube-vip-test")
				Expect(err).NotTo(HaveOccurred())
				kindCluster, containerIP, _ = setupEnv(ctx, &cpVIP,
					server, e2e.DualstackFamilyIPv6, utils.IPv6Family,
					[]string{utils.IPv6Family}, &gobgpPeers,
					&gobgpClient, logger, nodesNumber, "auto_sourceif", "mpbgp-if-ipv6", false, false, true)
			})

			AfterAll(func() {
				By(fmt.Sprintf("saving logs to %q", tempDirPath))
				err := e2e.GetLogs(ctx, kindCluster.Client, tempDirPath, kindCluster.Name)
				Expect(err).ToNot(HaveOccurred())
				server.RemovePeers(ctx, gobgpPeers)
				kindCluster.Delete()
			})

			DescribeTable("advertise IPv4 routes over IPv6 session",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv4Family, api.Family_AFI_IP, []corev1.IPFamily{corev1.IPv4Protocol}, svcName,
						trafficPolicy, kindCluster.Client, 1, gobgpClient, containerIP, "")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only stops advertising route if it was referenced by multiple services and all of them were deleted",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv4Family, api.Family_AFI_IP, []corev1.IPFamily{corev1.IPv4Protocol}, svcName,
						trafficPolicy, kindCluster.Client, 2, gobgpClient, containerIP, "")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)
		})

		Describe("kube-vip IPv4 services BGP mode functionality, with service election", Ordered, func() {
			var (
				cpVIP       string
				kindCluster *e2e.Cluster
				gobgpClient api.GobgpApiClient
				gobgpPeers  []*e2e.BGPPeerValues
				tempDirPath string
				nodesNumber = 1
			)

			BeforeAll(func() {
				var err error
				tempDirPath, err = os.MkdirTemp(server.TempDir, "kube-vip-test")
				Expect(err).NotTo(HaveOccurred())
				kindCluster, _, _ = setupEnv(ctx, &cpVIP,
					server, utils.IPv4Family, utils.IPv4Family,
					[]string{utils.IPv4Family}, &gobgpPeers,
					&gobgpClient, logger, nodesNumber, "", "bgp-ipv4", true, false, true)
			})

			AfterAll(func() {
				By(fmt.Sprintf("saving logs to %q", tempDirPath))
				err := e2e.GetLogs(ctx, kindCluster.Client, tempDirPath, kindCluster.Name)
				Expect(err).ToNot(HaveOccurred())
				server.RemovePeers(ctx, gobgpPeers)
				kindCluster.Delete()
			})

			DescribeTable("advertise IPv4 routes for services",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv4Family, api.Family_AFI_IP, []corev1.IPFamily{corev1.IPv4Protocol}, svcName,
						trafficPolicy, kindCluster.Client, 1, gobgpClient, "", "")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only stops advertising route if it was referenced by multiple services and all of them were deleted",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv4Family, api.Family_AFI_IP, []corev1.IPFamily{corev1.IPv4Protocol}, svcName,
						trafficPolicy, kindCluster.Client, 2, gobgpClient, "", "")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only stops advertising route if it was referenced by multiple services and all of them were deleted while using common lease",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv4Family, api.Family_AFI_IP, []corev1.IPFamily{corev1.IPv4Protocol}, svcName,
						trafficPolicy, kindCluster.Client, 2, gobgpClient, "", "common-lease")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
			)
		})

		Describe("kube-vip IPv6 services BGP mode functionality, with service election", Ordered, func() {
			var (
				cpVIP       string
				kindCluster *e2e.Cluster
				gobgpClient api.GobgpApiClient
				gobgpPeers  []*e2e.BGPPeerValues
				tempDirPath string
				nodesNumber = 1
			)

			BeforeAll(func() {
				var err error
				tempDirPath, err = os.MkdirTemp(server.TempDir, "kube-vip-test")
				Expect(err).NotTo(HaveOccurred())
				kindCluster, _, _ = setupEnv(ctx, &cpVIP,
					server, utils.IPv6Family, utils.IPv6Family,
					[]string{utils.IPv6Family}, &gobgpPeers,
					&gobgpClient, logger, nodesNumber, "", "bgp-ipv4", true, false, true)
			})

			AfterAll(func() {
				By(fmt.Sprintf("saving logs to %q", tempDirPath))
				err := e2e.GetLogs(ctx, kindCluster.Client, tempDirPath, kindCluster.Name)
				Expect(err).ToNot(HaveOccurred())
				server.RemovePeers(ctx, gobgpPeers)
				kindCluster.Delete()
			})

			DescribeTable("advertise IPv6 routes for services",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv6Family, api.Family_AFI_IP6, []corev1.IPFamily{corev1.IPv6Protocol}, svcName,
						trafficPolicy, kindCluster.Client, 1, gobgpClient, "", "")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only stops advertising route if it was referenced by multiple services and all of them were deleted",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv6Family, api.Family_AFI_IP6, []corev1.IPFamily{corev1.IPv6Protocol}, svcName,
						trafficPolicy, kindCluster.Client, 2, gobgpClient, "", "")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only stops advertising route if it was referenced by multiple services and all of them were deleted while using common lease",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv6Family, api.Family_AFI_IP6, []corev1.IPFamily{corev1.IPv6Protocol}, svcName,
						trafficPolicy, kindCluster.Client, 2, gobgpClient, "", "common-lease")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
			)
		})

		Describe("kube-vip DualStack services BGP mode functionality with MP-BGP IPv6 over IPv4 - fixed nexthop, with service election", Ordered, func() {
			var (
				cpVIP       string
				kindCluster *e2e.Cluster
				gobgpClient api.GobgpApiClient
				gobgpPeers  []*e2e.BGPPeerValues
				tempDirPath string
				nodesNumber = 1
			)

			BeforeAll(func() {
				var err error
				tempDirPath, err = os.MkdirTemp(server.TempDir, "kube-vip-test")
				Expect(err).NotTo(HaveOccurred())
				kindCluster, _, _ = setupEnv(ctx, &cpVIP,
					server, e2e.DualstackFamily, utils.IPv4Family,
					[]string{utils.IPv4Family}, &gobgpPeers,
					&gobgpClient, logger, nodesNumber, "fixed", "mpbgp-ipv4", true, false, true)
			})

			AfterAll(func() {
				By(fmt.Sprintf("saving logs to %q", tempDirPath))
				err := e2e.GetLogs(ctx, kindCluster.Client, tempDirPath, kindCluster.Name)
				Expect(err).ToNot(HaveOccurred())
				server.RemovePeers(ctx, gobgpPeers)
				kindCluster.Delete()
			})

			DescribeTable("advertise IPv6 routes over IPv4 session",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv6Family, api.Family_AFI_IP6, []corev1.IPFamily{corev1.IPv6Protocol}, svcName,
						trafficPolicy, kindCluster.Client, 1, gobgpClient, defaultFixedNexthopv6, "")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only stops advertising route if it was referenced by multiple services and all of them were deleted",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv6Family, api.Family_AFI_IP6, []corev1.IPFamily{corev1.IPv6Protocol}, svcName,
						trafficPolicy, kindCluster.Client, 2, gobgpClient, defaultFixedNexthopv6, "")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only stops advertising route if it was referenced by multiple services and all of them were deleted while using common lease",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv6Family, api.Family_AFI_IP6, []corev1.IPFamily{corev1.IPv6Protocol}, svcName,
						trafficPolicy, kindCluster.Client, 2, gobgpClient, defaultFixedNexthopv6, "common-lease")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
			)
		})

		Describe("kube-vip DualStack services BGP mode functionality with MP-BGP IPv4 over IPv6 - fixed nexthop, with service election", Ordered, func() {
			var (
				cpVIP       string
				kindCluster *e2e.Cluster
				gobgpClient api.GobgpApiClient
				gobgpPeers  []*e2e.BGPPeerValues
				tempDirPath string
				nodesNumber = 1
			)

			BeforeAll(func() {
				var err error
				tempDirPath, err = os.MkdirTemp(server.TempDir, "kube-vip-test")
				Expect(err).NotTo(HaveOccurred())
				kindCluster, _, _ = setupEnv(ctx, &cpVIP,
					server, e2e.DualstackFamilyIPv6, utils.IPv6Family,
					[]string{utils.IPv6Family}, &gobgpPeers,
					&gobgpClient, logger, nodesNumber, "fixed", "mpbgp-ipv6", true, false, true)
			})

			AfterAll(func() {
				By(fmt.Sprintf("saving logs to %q", tempDirPath))
				err := e2e.GetLogs(ctx, kindCluster.Client, tempDirPath, kindCluster.Name)
				Expect(err).ToNot(HaveOccurred())
				server.RemovePeers(ctx, gobgpPeers)
				kindCluster.Delete()
			})

			DescribeTable("advertise IPv4 routes over IPv6 session",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv4Family, api.Family_AFI_IP, []corev1.IPFamily{corev1.IPv4Protocol}, svcName,
						trafficPolicy, kindCluster.Client, 1, gobgpClient, defaultFixedNexthopv4, "")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only stops advertising route if it was referenced by multiple services and all of them were deleted",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv4Family, api.Family_AFI_IP, []corev1.IPFamily{corev1.IPv4Protocol}, svcName,
						trafficPolicy, kindCluster.Client, 2, gobgpClient, defaultFixedNexthopv4, "")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only stops advertising route if it was referenced by multiple services and all of them were deleted while using common lease",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv4Family, api.Family_AFI_IP, []corev1.IPFamily{corev1.IPv4Protocol}, svcName,
						trafficPolicy, kindCluster.Client, 2, gobgpClient, defaultFixedNexthopv4, "common-lease")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
			)
		})

		Describe("kube-vip DualStack services BGP mode functionality with MP-BGP IPv6 over IPv4 - auto_sourceif nexthop, with service election", Ordered, func() {
			var (
				cpVIP       string
				kindCluster *e2e.Cluster
				gobgpClient api.GobgpApiClient
				gobgpPeers  []*e2e.BGPPeerValues
				containerIP string
				tempDirPath string
				nodesNumber = 1
			)

			BeforeAll(func() {
				var err error
				tempDirPath, err = os.MkdirTemp(server.TempDir, "kube-vip-test")
				Expect(err).NotTo(HaveOccurred())
				kindCluster, _, containerIP = setupEnv(ctx, &cpVIP,
					server, e2e.DualstackFamily, utils.IPv4Family,
					[]string{utils.IPv4Family}, &gobgpPeers,
					&gobgpClient, logger, nodesNumber, "auto_sourceif", "mpbgp-if-ipv4", true, false, true)
			})

			AfterAll(func() {
				By(fmt.Sprintf("saving logs to %q", tempDirPath))
				err := e2e.GetLogs(ctx, kindCluster.Client, tempDirPath, kindCluster.Name)
				Expect(err).ToNot(HaveOccurred())
				server.RemovePeers(ctx, gobgpPeers)
				kindCluster.Delete()
			})

			DescribeTable("advertise IPv6 routes over IPv4 session",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv6Family, api.Family_AFI_IP6, []corev1.IPFamily{corev1.IPv6Protocol}, svcName,
						trafficPolicy, kindCluster.Client, 1, gobgpClient, containerIP, "")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only stops advertising route if it was referenced by multiple services and all of them were deleted",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv6Family, api.Family_AFI_IP6, []corev1.IPFamily{corev1.IPv6Protocol}, svcName,
						trafficPolicy, kindCluster.Client, 2, gobgpClient, containerIP, "")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only stops advertising route if it was referenced by multiple services and all of them were deleted while using common lease",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv6Family, api.Family_AFI_IP6, []corev1.IPFamily{corev1.IPv6Protocol}, svcName,
						trafficPolicy, kindCluster.Client, 2, gobgpClient, containerIP, "common-lease")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
			)
		})

		Describe("kube-vip DualStack services BGP mode functionality with MP-BGP IPv4 over IPv6 - auto_sourceif nexthop, with service election", Ordered, func() {
			var (
				cpVIP       string
				kindCluster *e2e.Cluster
				gobgpClient api.GobgpApiClient
				gobgpPeers  []*e2e.BGPPeerValues
				containerIP string
				tempDirPath string
				nodesNumber = 1
			)

			BeforeAll(func() {
				var err error
				tempDirPath, err = os.MkdirTemp(server.TempDir, "kube-vip-test")
				Expect(err).NotTo(HaveOccurred())
				kindCluster, containerIP, _ = setupEnv(ctx, &cpVIP,
					server, e2e.DualstackFamilyIPv6, utils.IPv6Family,
					[]string{utils.IPv6Family}, &gobgpPeers,
					&gobgpClient, logger, nodesNumber, "auto_sourceif", "mpbgp-if-ipv6", true, false, true)
			})

			AfterAll(func() {
				By(fmt.Sprintf("saving logs to %q", tempDirPath))
				err := e2e.GetLogs(ctx, kindCluster.Client, tempDirPath, kindCluster.Name)
				Expect(err).ToNot(HaveOccurred())
				server.RemovePeers(ctx, gobgpPeers)
				kindCluster.Delete()
			})

			DescribeTable("advertise IPv4 routes over IPv6 session",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv4Family, api.Family_AFI_IP, []corev1.IPFamily{corev1.IPv4Protocol}, svcName,
						trafficPolicy, kindCluster.Client, 1, gobgpClient, containerIP, "")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only stops advertising route if it was referenced by multiple services and all of them were deleted",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv4Family, api.Family_AFI_IP, []corev1.IPFamily{corev1.IPv4Protocol}, svcName,
						trafficPolicy, kindCluster.Client, 2, gobgpClient, containerIP, "")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only stops advertising route if it was referenced by multiple services and all of them were deleted while using common lease",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv4Family, api.Family_AFI_IP, []corev1.IPFamily{corev1.IPv4Protocol}, svcName,
						trafficPolicy, kindCluster.Client, 2, gobgpClient, containerIP, "common-lease")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
			)
		})

		Describe("kube-vip BGP configuration via node annotations", Ordered, func() {
			var (
				kindCluster *e2e.Cluster
				gobgpClient api.GobgpApiClient
				gobgpPeers  []*e2e.BGPPeerValues
				tempDirPath string
			)

			BeforeAll(func() {
				var err error
				tempDirPath, err = os.MkdirTemp(server.TempDir, "kube-vip-test")
				Expect(err).NotTo(HaveOccurred())

				kindCluster = e2e.CreateCluster(ctx, &e2e.ClusterSpec{
					Name:         "bgp-annotations",
					Nodes:        1,
					Networking:   kindconfigv1alpha4.Networking{IPFamily: kindconfigv1alpha4.IPv4Family},
					Logger:       logger.(e2e.TestLogger),
					ConfigMtx:    ConfigMtx,
					TemplateName: "kube-vip-bgp-annotations.yaml.tmpl",
				})
				kindCluster.LoadImage("ghcr.io/traefik/whoami:v1.11")

				By(withTimestamp("creating test daemonset"))
				createTestDS(ctx, kindCluster.Client, dsNamespace, dsName, 1)

				kvPeer := e2e.BGPPeerValues{
					IP:       server.LocalIPv4,
					AS:       bgp.GoBGPAS,
					IPFamily: utils.IPv4Family,
				}
				Expect(annotateNodes(ctx, "test", kindCluster.Client, kvPeer, bgp.KubevipAS)).To(Succeed())

				gobgpPeers = server.AddClusterPeers(ctx, kindCluster.Nodes, bgp.KubevipAS, []string{utils.IPv4Family})
				gobgpClient = server.Client
			})

			AfterAll(func() {
				By(fmt.Sprintf("saving logs to %q", tempDirPath))
				err := e2e.GetLogs(ctx, kindCluster.Client, tempDirPath, kindCluster.Name)
				Expect(err).ToNot(HaveOccurred())
				server.RemovePeers(ctx, gobgpPeers)
				kindCluster.Delete()
			})

			DescribeTable("advertise IPv4 routes for services",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv4Family, api.Family_AFI_IP, []corev1.IPFamily{corev1.IPv4Protocol}, svcName,
						trafficPolicy, kindCluster.Client, 1, gobgpClient, "", "")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)
		})
	}
})

func testBGP(ctx context.Context, offset uint, lbFamily string, afiFamily api.Family_Afi, svcFamily []corev1.IPFamily, svcName string,
	trafficPolicy corev1.ServiceExternalTrafficPolicy, client kubernetes.Interface, numberOfServices int,
	gobgpClient api.GobgpApiClient, expectedNexthop string, serviceLease string,
) {
	lbAddress := e2e.GenerateVIP(lbFamily, offset, defaultNetwork)
	routeCheckFamily := &api.Family{
		Afi:  afiFamily,
		Safi: api.Family_SAFI_UNICAST,
	}
	testServiceBGP(ctx, svcName, lbAddress, trafficPolicy, client, svcFamily, numberOfServices, gobgpClient, routeCheckFamily, expectedNexthop, serviceLease)
}

func setupEnv(ctx context.Context, cpVIP *string,
	server *bgp.Server, clusterAddrFamily, bgpClientAddrFamily string,
	peerAddrFamily []string, gobgpPeers *[]*e2e.BGPPeerValues,
	gobgpClient *api.GobgpApiClient, logger log.Logger,
	nodesNumber int, mpbgpnexthop, clusterNameSuffix string,
	serviceElection, cpEnable, svcEnable bool,
) (*e2e.Cluster, string, string) {
	*cpVIP = e2e.GenerateVIP(clusterAddrFamily, SOffset.Get(), defaultNetwork)

	var clusterIPFamily kindconfigv1alpha4.ClusterIPFamily
	var podSubnet, serviceSubnet string
	switch clusterAddrFamily {
	case utils.IPv6Family:
		clusterIPFamily = kindconfigv1alpha4.IPv6Family
	case e2e.DualstackFamily:
		clusterIPFamily = kindconfigv1alpha4.DualStackFamily
	case e2e.DualstackFamilyIPv6:
		clusterIPFamily = kindconfigv1alpha4.DualStackFamily
		podSubnet = "fd00:10:244::/56,10.244.0.0/16"
		serviceSubnet = "fd00:10:96::/112,10.96.0.0/16"
	default:
		clusterIPFamily = kindconfigv1alpha4.IPv4Family
	}

	networking := kindconfigv1alpha4.Networking{IPFamily: clusterIPFamily}
	if podSubnet != "" && serviceSubnet != "" {
		networking.PodSubnet = podSubnet
		networking.ServiceSubnet = serviceSubnet
	}

	kvPeers := []*e2e.BGPPeerValues{}
	if slices.Contains(peerAddrFamily, utils.IPv4Family) {
		kvPeers = append(kvPeers, &e2e.BGPPeerValues{IP: server.LocalIPv4, AS: bgp.GoBGPAS, IPFamily: utils.IPv4Family})
	}
	if slices.Contains(peerAddrFamily, utils.IPv6Family) {
		kvPeers = append(kvPeers, &e2e.BGPPeerValues{IP: server.LocalIPv6, AS: bgp.GoBGPAS, IPFamily: utils.IPv6Family})
	}

	manifestValues := e2e.KubevipManifestValues{
		ControlPlaneVIP:    *cpVIP,
		ControlPlaneEnable: fmt.Sprintf("%t", cpEnable),
		SvcEnable:          fmt.Sprintf("%t", svcEnable),
		SvcElectionEnable:  fmt.Sprintf("%t", serviceElection),
		EnableNodeLabeling: "false",
		BGPAS:              bgp.KubevipAS,
		BGPPeers:           bgp.PeerStrings(kvPeers),
		MPBGPNexthop:       mpbgpnexthop,
		MPBGPNexthopIPv4:   defaultFixedNexthopv4,
		MPBGPNexthopIPv6:   defaultFixedNexthopv6,
	}

	By(manifestValues.BGPPeers)

	cluster := e2e.CreateCluster(ctx, &e2e.ClusterSpec{
		Name:       clusterNameSuffix,
		Nodes:      nodesNumber,
		Networking: networking,
		KubeVip:    manifestValues,
		Logger:     logger.(e2e.TestLogger),
		ConfigMtx:  ConfigMtx,
	})

	cluster.LoadImage("ghcr.io/traefik/whoami:v1.11")

	By(withTimestamp("creating test daemonset"))
	createTestDS(ctx, cluster.Client, dsNamespace, dsName, 1)

	containerName := fmt.Sprintf("%s-control-plane", cluster.Name)
	containerIPv4, containerIPv6, err := bgp.ContainerIPs(ctx, containerName)
	Expect(err).ToNot(HaveOccurred())

	*gobgpPeers = server.AddClusterPeers(ctx, cluster.Nodes, bgp.KubevipAS, peerAddrFamily)

	if bgpClientAddrFamily == utils.IPv6Family {
		*gobgpClient = server.NewClientIPv6()
	} else {
		*gobgpClient = server.Client
	}

	return cluster, containerIPv4, containerIPv6
}

func testServiceBGP(ctx context.Context, svcName, lbAddress string, trafficPolicy corev1.ServiceExternalTrafficPolicy,
	client kubernetes.Interface, serviceAddrFamily []corev1.IPFamily, numberOfServices int,
	gobgpClient api.GobgpApiClient, gobgpFamily *api.Family, expectedNexthop string,
	serviceLease string,
) {
	lbAddresses := vip.Split(lbAddress)

	services := []string{}
	for i := range numberOfServices {
		services = append(services, fmt.Sprintf("%s-%d", svcName, i))
	}

	for _, svc := range services {
		createTestService(ctx, svc, dsNamespace, dsName, lbAddress,
			client, corev1.IPFamilyPolicyPreferDualStack, serviceAddrFamily, trafficPolicy, serviceLease, 80)
		time.Sleep(time.Second)
	}

	for _, addr := range lbAddresses {
		By(withTimestamp(fmt.Sprintf("checking bgp route for address %q", addr)))
		paths := bgp.CheckPaths(ctx, gobgpClient, gobgpFamily, []*api.TableLookupPrefix{{Prefix: addr}}, 1)
		Expect(paths).ToNot(BeNil())
		Expect(paths).ToNot(BeEmpty())
		Expect(strings.Contains(paths[0].Prefix, lbAddress)).To(BeTrue())
		if expectedNexthop != "" {
			Expect(strings.Contains(paths[0].String(), fmt.Sprintf("next_hop:\"%s\"", expectedNexthop)) || strings.Contains(paths[0].String(), fmt.Sprintf("next_hops:\"%s\"", expectedNexthop))).To(BeTrue())
		}
	}

	for i := range numberOfServices {
		By(withTimestamp(fmt.Sprintf("deleting service '%s/%s'", dsNamespace, services[i])))
		err := client.CoreV1().Services(dsNamespace).Delete(ctx, services[i], metav1.DeleteOptions{})
		Expect(err).ToNot(HaveOccurred())
		time.Sleep(time.Second)
		if i < numberOfServices-1 {
			for _, addr := range lbAddresses {
				By(withTimestamp(fmt.Sprintf("checking bgp route for address %q", addr)))
				paths := bgp.CheckPaths(ctx, gobgpClient, gobgpFamily, []*api.TableLookupPrefix{{Prefix: addr}}, 1)
				Expect(paths).ToNot(BeNil())
				Expect(paths).ToNot(BeEmpty())
				Expect(strings.Contains(paths[0].Prefix, lbAddress)).To(BeTrue())
				if expectedNexthop != "" {
					Expect(strings.Contains(paths[0].String(), fmt.Sprintf("next_hop:\"%s\"", expectedNexthop)) || strings.Contains(paths[0].String(), fmt.Sprintf("next_hops:\"%s\"", expectedNexthop))).To(BeTrue())
				}
			}
		}
	}

	for _, addr := range lbAddresses {
		By(withTimestamp(fmt.Sprintf("checking bgp route for address %q - should be deleted", addr)))
		bgp.CheckPaths(ctx, gobgpClient, gobgpFamily, []*api.TableLookupPrefix{{Prefix: addr}}, 0)
	}
}

func testControlPlaneBGP(ctx context.Context, gobgpClient api.GobgpApiClient, lbAddresses []string) {
	for _, addr := range lbAddresses {
		By(withTimestamp(fmt.Sprintf("checking bgp route for address %q", addr)))
		afiFamily := api.Family_AFI_IP
		if net.ParseIP(addr).To4() == nil {
			afiFamily = api.Family_AFI_IP6
		}
		gobgpFamily := &api.Family{
			Afi:  afiFamily,
			Safi: api.Family_SAFI_UNICAST,
		}
		paths := bgp.CheckPaths(ctx, gobgpClient, gobgpFamily, []*api.TableLookupPrefix{{Prefix: addr}}, 1)
		Expect(paths).ToNot(BeNil())
		Expect(paths).ToNot(BeEmpty())
		Expect(strings.Contains(paths[0].Prefix, addr)).To(BeTrue())
	}
}

// annotateNodes adds BGP peer-0 configuration annotations (with the given
// prefix) to all cluster nodes. Used by the annotation-based BGP test, where
// kube-vip reads peer config from node annotations instead of the bgp_peers
// env var.
func annotateNodes(ctx context.Context, pref string, client kubernetes.Interface, peer e2e.BGPPeerValues, asn uint32) error {
	pref += "/bgp-peers-0-"

	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}

	for _, n := range nodes.Items {
		annotations := n.Annotations
		if annotations == nil {
			annotations = make(map[string]string)
		}

		var addr string
		for _, a := range n.Status.Addresses {
			if a.Type == corev1.NodeInternalIP {
				addr = a.Address
			}
		}
		if addr == "" {
			return fmt.Errorf("node %q doesn't have an internal address", n.Name)
		}

		annotations[pref+"node-asn"] = strconv.Itoa(int(asn))
		annotations[pref+"src-ip"] = addr
		annotations[pref+"peer-asn"] = strconv.Itoa(int(peer.AS))
		annotations[pref+"peer-ip"] = peer.IP

		patch, err := json.Marshal(map[string]any{
			"metadata": map[string]any{
				"annotations": annotations,
			},
		})
		if err != nil {
			return fmt.Errorf("failed to marshall annotations patch: %w", err)
		}

		_, err = client.CoreV1().Nodes().Patch(ctx, n.Name, types.MergePatchType, patch, metav1.PatchOptions{})
		if err != nil {
			return err
		}
	}

	return nil
}
