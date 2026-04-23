//go:build e2e
// +build e2e

package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/template"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
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
	"github.com/kube-vip/kube-vip/testing/services/pkg/deployment"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	api "github.com/osrg/gobgp/v3/api"
)

const (
	goBGPAS   uint32 = 65500
	kubevipAS uint32 = 65501
	goBGPPort uint32 = 50051

	defaultFixedNexthopv6 = "fc00:1000:1000:1000::100"
	defaultFixedNexthopv4 = "172.18.0.100"
)

var _ = Describe("kube-vip BGP mode", Ordered, func() {
	if Mode == ModeBGP {
		var (
			logger                     log.Logger
			imagePath                  string
			k8sImagePath               string
			configPath                 string
			kubeVIPBGPManifestTemplate *template.Template
			goBGPConfigTemplate        *template.Template
			v129                       bool
			localIPv4                  string
			localIPv6                  string
			curDir                     string
			networkInterface           string
			tempDirPathRoot            string

			bgpKill chan any
		)

		ctx, cancel := context.WithCancel(context.TODO())

		BeforeAll(func() {
			klog.SetOutput(GinkgoWriter)
			logger = e2e.TestLogger{}

			imagePath = os.Getenv("E2E_IMAGE_PATH")    // Path to kube-vip image
			configPath = os.Getenv("CONFIG_PATH")      // path to the api server config
			k8sImagePath = os.Getenv("K8S_IMAGE_PATH") // path to the kubernetes image (version for kind)
			if configPath == "" {
				configPath = "/etc/kubernetes/admin.conf"
			}
			if networkInterface = os.Getenv("NETWORK_INTERFACE"); networkInterface == "" {
				networkInterface = "br-"
			}

			_, v129 = os.LookupEnv("V129")
			var err error
			curDir, err = os.Getwd()
			Expect(err).NotTo(HaveOccurred())

			templateBGPPath := filepath.Join(curDir, "kube-vip-bgp.yaml.tmpl")
			kubeVIPBGPManifestTemplate, err = template.New("kube-vip-bgp.yaml.tmpl").ParseFiles(templateBGPPath)
			Expect(err).NotTo(HaveOccurred())

			tempDirPathRoot, err = os.MkdirTemp("", "kube-vip-test-bgp")
			Expect(err).NotTo(HaveOccurred())
			v4addr, _, err := deployment.GetLocalIPv4(networkInterface)
			Expect(err).ToNot(HaveOccurred())
			localIPv4 = v4addr.String()

			v6addr, _, err := deployment.GetLocalIPv6(networkInterface)
			Expect(err).ToNot(HaveOccurred())
			localIPv6 = v6addr.String()

			goBGPConfig := &e2e.BGPPeerValues{
				AS: goBGPAS,
			}

			bgpKill = make(chan any)

			goBGPConfigPath := filepath.Join(filepath.Join(curDir, "bgp"), "config.toml.tmpl")
			goBGPConfigTemplate, err = template.New("config.toml.tmpl").ParseFiles(goBGPConfigPath)
			Expect(err).ToNot(HaveOccurred())

			goBGPConfigPath = filepath.Join(tempDirPathRoot, "config.toml")

			f, err := os.OpenFile(goBGPConfigPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0644)
			Expect(err).ToNot(HaveOccurred())
			defer f.Close()

			err = goBGPConfigTemplate.Execute(f, goBGPConfig)
			Expect(err).ToNot(HaveOccurred())

			go startGoBGP(goBGPConfigPath, bgpKill)
		})

		AfterAll(func() {
			close(bgpKill)
			if os.Getenv("E2E_KEEP_LOGS") != "true" {
				Expect(os.RemoveAll(tempDirPathRoot)).To(Succeed())
			}
			cancel()
		})

		Describe("kube-vip IPv4 control-plane BGP mode functionality", Ordered, func() {
			var (
				cpVIP          string
				clusterName    string
				client         kubernetes.Interface
				manifestValues *e2e.KubevipManifestValues
				gobgpClient    api.GobgpApiClient
				gobgpPeers     []*e2e.BGPPeerValues
				tempDirPath    string

				nodesNumber = 1
			)

			BeforeAll(func() {
				var err error
				tempDirPath, err = os.MkdirTemp(tempDirPathRoot, "kube-vip-test")
				Expect(err).NotTo(HaveOccurred())
				setupEnv(ctx, &cpVIP, &clusterName, tempDirPath, manifestValues, localIPv4, localIPv6, imagePath, configPath,
					k8sImagePath, utils.IPv4Family, utils.IPv4Family, []string{utils.IPv4Family}, &client, &gobgpPeers, v129,
					kubeVIPBGPManifestTemplate, &gobgpClient, logger, nodesNumber, "", "bgp-cp-ipv4", false, true, false)
			})

			AfterAll(func() {
				By(fmt.Sprintf("saving logs to %q", tempDirPath))
				err := e2e.GetLogs(ctx, client, tempDirPath, clusterName)
				Expect(err).ToNot(HaveOccurred())
				for _, p := range gobgpPeers {
					Eventually(func() error {
						_, err := gobgpClient.DeletePeer(ctx, &api.DeletePeerRequest{
							Address: p.IP,
						})
						return err
					}, "30s", "200ms").Should(Succeed())
				}
				cleanupCluster(clusterName, defaultNetwork, ConfigMtx, logger)
			})

			It("should add paths", func() {
				addresses := strings.Split(cpVIP, ",")
				testControlPlaneBGP(ctx, gobgpClient, addresses)
			})
		})

		Describe("kube-vip IPv6 control-plane BGP mode functionality", Ordered, func() {
			var (
				cpVIP          string
				clusterName    string
				client         kubernetes.Interface
				manifestValues *e2e.KubevipManifestValues
				gobgpClient    api.GobgpApiClient
				gobgpPeers     []*e2e.BGPPeerValues
				tempDirPath    string

				nodesNumber = 1
			)

			BeforeAll(func() {
				var err error
				tempDirPath, err = os.MkdirTemp(tempDirPathRoot, "kube-vip-test")
				Expect(err).NotTo(HaveOccurred())
				setupEnv(ctx, &cpVIP, &clusterName, tempDirPath, manifestValues, localIPv4, localIPv6, imagePath, configPath,
					k8sImagePath, utils.IPv6Family, utils.IPv6Family, []string{utils.IPv6Family}, &client, &gobgpPeers, v129,
					kubeVIPBGPManifestTemplate, &gobgpClient, logger, nodesNumber, "", "bgp-cp-ipv4", false, true, false)
			})

			AfterAll(func() {
				By(fmt.Sprintf("saving logs to %q", tempDirPath))
				err := e2e.GetLogs(ctx, client, tempDirPath, clusterName)
				Expect(err).ToNot(HaveOccurred())
				for _, p := range gobgpPeers {
					Eventually(func() error {
						_, err := gobgpClient.DeletePeer(ctx, &api.DeletePeerRequest{
							Address: p.IP,
						})
						return err
					}, "30s", "200ms").Should(Succeed())
				}
				cleanupCluster(clusterName, defaultNetwork, ConfigMtx, logger)
			})

			It("should add paths", func() {
				addresses := strings.Split(cpVIP, ",")
				testControlPlaneBGP(ctx, gobgpClient, addresses)
			})
		})

		Describe("kube-vip DualStack control-plane BGP mode functionality, IPv6 over IPv4", Ordered, func() {
			var (
				cpVIP          string
				clusterName    string
				client         kubernetes.Interface
				manifestValues *e2e.KubevipManifestValues
				gobgpClient    api.GobgpApiClient
				gobgpPeers     []*e2e.BGPPeerValues
				tempDirPath    string

				nodesNumber = 1
			)

			BeforeAll(func() {
				var err error
				tempDirPath, err = os.MkdirTemp(tempDirPathRoot, "kube-vip-test")
				Expect(err).NotTo(HaveOccurred())
				setupEnv(ctx, &cpVIP, &clusterName, tempDirPath, manifestValues, localIPv4, localIPv6, imagePath, configPath, k8sImagePath,
					e2e.DualstackFamily, utils.IPv4Family, []string{utils.IPv4Family}, &client, &gobgpPeers, v129, kubeVIPBGPManifestTemplate, &gobgpClient,
					logger, nodesNumber, "fixed", "cp-mpbgp-ipv4", false, true, false)
			})

			AfterAll(func() {
				By(fmt.Sprintf("saving logs to %q", tempDirPath))
				err := e2e.GetLogs(ctx, client, tempDirPath, clusterName)
				Expect(err).ToNot(HaveOccurred())
				for _, p := range gobgpPeers {
					Eventually(func() error {
						_, err := gobgpClient.DeletePeer(ctx, &api.DeletePeerRequest{
							Address: p.IP,
						})
						return err
					}, "30s", "200ms").Should(Succeed())
				}
				cleanupCluster(clusterName, defaultNetwork, ConfigMtx, logger)
			})

			It("should add paths", func() {
				addresses := strings.Split(cpVIP, ",")
				testControlPlaneBGP(ctx, gobgpClient, addresses)
			})
		})

		Describe("kube-vip DualStack control-plane BGP mode functionality, IPv4 over IPv6", Ordered, func() {
			var (
				cpVIP          string
				clusterName    string
				client         kubernetes.Interface
				manifestValues *e2e.KubevipManifestValues
				gobgpClient    api.GobgpApiClient
				gobgpPeers     []*e2e.BGPPeerValues
				tempDirPath    string

				nodesNumber = 1
			)

			BeforeAll(func() {
				var err error
				tempDirPath, err = os.MkdirTemp(tempDirPathRoot, "kube-vip-test")
				Expect(err).NotTo(HaveOccurred())
				setupEnv(ctx, &cpVIP, &clusterName, tempDirPath, manifestValues, localIPv4, localIPv6, imagePath, configPath, k8sImagePath,
					e2e.DualstackFamilyIPv6, utils.IPv6Family, []string{utils.IPv6Family}, &client, &gobgpPeers, v129, kubeVIPBGPManifestTemplate, &gobgpClient,
					logger, nodesNumber, "fixed", "cp-mpbgp-ipv6", false, true, false)
			})

			AfterAll(func() {
				By(fmt.Sprintf("saving logs to %q", tempDirPath))
				err := e2e.GetLogs(ctx, client, tempDirPath, clusterName)
				Expect(err).ToNot(HaveOccurred())
				for _, p := range gobgpPeers {
					Eventually(func() error {
						_, err := gobgpClient.DeletePeer(ctx, &api.DeletePeerRequest{
							Address: p.IP,
						})
						return err
					}, "30s", "200ms").Should(Succeed())
				}
				cleanupCluster(clusterName, defaultNetwork, ConfigMtx, logger)
			})

			It("should add paths", func() {
				addresses := strings.Split(cpVIP, ",")
				testControlPlaneBGP(ctx, gobgpClient, addresses)
			})
		})

		Describe("kube-vip IPv4 services BGP mode functionality", Ordered, func() {
			var (
				cpVIP          string
				clusterName    string
				client         kubernetes.Interface
				manifestValues *e2e.KubevipManifestValues
				gobgpClient    api.GobgpApiClient
				gobgpPeers     []*e2e.BGPPeerValues
				tempDirPath    string

				nodesNumber = 1
			)

			BeforeAll(func() {
				var err error
				tempDirPath, err = os.MkdirTemp(tempDirPathRoot, "kube-vip-test")
				Expect(err).NotTo(HaveOccurred())
				setupEnv(ctx, &cpVIP, &clusterName, tempDirPath, manifestValues, localIPv4, localIPv6, imagePath, configPath,
					k8sImagePath, utils.IPv4Family, utils.IPv4Family, []string{utils.IPv4Family}, &client, &gobgpPeers, v129,
					kubeVIPBGPManifestTemplate, &gobgpClient, logger, nodesNumber, "", "bgp-ipv4", false, false, true)
			})

			AfterAll(func() {
				By(fmt.Sprintf("saving logs to %q", tempDirPath))
				err := e2e.GetLogs(ctx, client, tempDirPath, clusterName)
				Expect(err).ToNot(HaveOccurred())
				for _, p := range gobgpPeers {
					Eventually(func() error {
						_, err := gobgpClient.DeletePeer(ctx, &api.DeletePeerRequest{
							Address: p.IP,
						})
						return err
					}, "30s", "200ms").Should(Succeed())
				}
				cleanupCluster(clusterName, defaultNetwork, ConfigMtx, logger)
			})

			DescribeTable("advertise IPv4 routes for services",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv4Family, api.Family_AFI_IP, []corev1.IPFamily{corev1.IPv4Protocol}, svcName,
						trafficPolicy, client, 1, gobgpClient, "", "")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only stops advertising route if it was referenced by multiple services and all of them were deleted",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv4Family, api.Family_AFI_IP, []corev1.IPFamily{corev1.IPv4Protocol}, svcName,
						trafficPolicy, client, 2, gobgpClient, "", "")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)
		})

		Describe("kube-vip IPv6 services BGP mode functionality", Ordered, func() {
			var (
				cpVIP          string
				clusterName    string
				client         kubernetes.Interface
				manifestValues *e2e.KubevipManifestValues
				gobgpClient    api.GobgpApiClient
				gobgpPeers     []*e2e.BGPPeerValues
				tempDirPath    string

				nodesNumber = 1
			)

			BeforeAll(func() {
				var err error
				tempDirPath, err = os.MkdirTemp(tempDirPathRoot, "kube-vip-test")
				Expect(err).NotTo(HaveOccurred())
				setupEnv(ctx, &cpVIP, &clusterName, tempDirPath, manifestValues, localIPv4, localIPv6, imagePath, configPath,
					k8sImagePath, utils.IPv6Family, utils.IPv6Family, []string{utils.IPv6Family}, &client, &gobgpPeers, v129,
					kubeVIPBGPManifestTemplate, &gobgpClient, logger, nodesNumber, "", "bgp-ipv4", false, false, true)
			})

			AfterAll(func() {
				By(fmt.Sprintf("saving logs to %q", tempDirPath))
				err := e2e.GetLogs(ctx, client, tempDirPath, clusterName)
				Expect(err).ToNot(HaveOccurred())
				for _, p := range gobgpPeers {
					_, err := gobgpClient.DeletePeer(ctx, &api.DeletePeerRequest{
						Address: p.IP,
					})
					Expect(err).ToNot(HaveOccurred())
				}
				cleanupCluster(clusterName, defaultNetwork, ConfigMtx, logger)
			})

			DescribeTable("advertise IPv6 routes for services",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv6Family, api.Family_AFI_IP6, []corev1.IPFamily{corev1.IPv6Protocol}, svcName,
						trafficPolicy, client, 1, gobgpClient, "", "")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only stops advertising route if it was referenced by multiple services and all of them were deleted",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv6Family, api.Family_AFI_IP6, []corev1.IPFamily{corev1.IPv6Protocol}, svcName,
						trafficPolicy, client, 2, gobgpClient, "", "")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)
		})

		Describe("kube-vip DualStack services BGP mode functionality with MP-BGP IPv6 over IPv4 - fixed nexthop", Ordered, func() {
			var (
				cpVIP          string
				clusterName    string
				client         kubernetes.Interface
				manifestValues *e2e.KubevipManifestValues
				gobgpClient    api.GobgpApiClient
				gobgpPeers     []*e2e.BGPPeerValues
				tempDirPath    string

				nodesNumber = 1
			)

			BeforeAll(func() {
				var err error
				tempDirPath, err = os.MkdirTemp(tempDirPathRoot, "kube-vip-test")
				Expect(err).NotTo(HaveOccurred())
				setupEnv(ctx, &cpVIP, &clusterName, tempDirPath, manifestValues, localIPv4, localIPv6, imagePath, configPath, k8sImagePath,
					e2e.DualstackFamily, utils.IPv4Family, []string{utils.IPv4Family}, &client, &gobgpPeers, v129, kubeVIPBGPManifestTemplate, &gobgpClient,
					logger, nodesNumber, "fixed", "mpbgp-ipv4", false, false, true)
			})

			AfterAll(func() {
				By(fmt.Sprintf("saving logs to %q", tempDirPath))
				err := e2e.GetLogs(ctx, client, tempDirPath, clusterName)
				Expect(err).ToNot(HaveOccurred())
				for _, p := range gobgpPeers {
					_, err := gobgpClient.DeletePeer(ctx, &api.DeletePeerRequest{
						Address: p.IP,
					})
					Expect(err).ToNot(HaveOccurred())
				}
				cleanupCluster(clusterName, defaultNetwork, ConfigMtx, logger)
			})

			DescribeTable("advertise IPv6 routes over IPv4 session",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv6Family, api.Family_AFI_IP6, []corev1.IPFamily{corev1.IPv6Protocol}, svcName,
						trafficPolicy, client, 1, gobgpClient, defaultFixedNexthopv6, "")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only stops advertising route if it was referenced by multiple services and all of them were deleted",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv6Family, api.Family_AFI_IP6, []corev1.IPFamily{corev1.IPv6Protocol}, svcName,
						trafficPolicy, client, 2, gobgpClient, defaultFixedNexthopv6, "")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)
		})

		Describe("kube-vip DualStack services BGP mode functionality with MP-BGP IPv4 over IPv6 - fixed nexthop", Ordered, func() {
			var (
				cpVIP          string
				clusterName    string
				client         kubernetes.Interface
				manifestValues *e2e.KubevipManifestValues
				gobgpClient    api.GobgpApiClient
				gobgpPeers     []*e2e.BGPPeerValues
				tempDirPath    string

				nodesNumber = 1
			)

			BeforeAll(func() {
				var err error
				tempDirPath, err = os.MkdirTemp(tempDirPathRoot, "kube-vip-test")
				Expect(err).NotTo(HaveOccurred())
				setupEnv(ctx, &cpVIP, &clusterName, tempDirPath, manifestValues, localIPv4, localIPv6, imagePath, configPath, k8sImagePath,
					e2e.DualstackFamilyIPv6, utils.IPv6Family, []string{utils.IPv6Family}, &client, &gobgpPeers, v129, kubeVIPBGPManifestTemplate, &gobgpClient,
					logger, nodesNumber, "fixed", "mpbgp-ipv6", false, false, true)
			})

			AfterAll(func() {
				By(fmt.Sprintf("saving logs to %q", tempDirPath))
				err := e2e.GetLogs(ctx, client, tempDirPath, clusterName)
				Expect(err).ToNot(HaveOccurred())
				for _, n := range gobgpPeers {
					_, err := gobgpClient.DeletePeer(ctx, &api.DeletePeerRequest{
						Address: n.IP,
					})
					Expect(err).ToNot(HaveOccurred())
				}
				cleanupCluster(clusterName, defaultNetwork, ConfigMtx, logger)
			})

			DescribeTable("advertise IPv4 routes over IPv6 session",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv4Family, api.Family_AFI_IP, []corev1.IPFamily{corev1.IPv4Protocol}, svcName,
						trafficPolicy, client, 1, gobgpClient, defaultFixedNexthopv4, "")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only stops advertising route if it was referenced by multiple services and all of them were deleted",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv4Family, api.Family_AFI_IP, []corev1.IPFamily{corev1.IPv4Protocol}, svcName,
						trafficPolicy, client, 2, gobgpClient, defaultFixedNexthopv4, "")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)
		})

		Describe("kube-vip DualStack services BGP mode functionality with MP-BGP IPv6 over IPv4 - auto_sourceif nexthop", Ordered, func() {
			var (
				cpVIP          string
				clusterName    string
				client         kubernetes.Interface
				manifestValues *e2e.KubevipManifestValues
				gobgpClient    api.GobgpApiClient
				gobgpPeers     []*e2e.BGPPeerValues
				containerIP    string
				tempDirPath    string

				nodesNumber = 1
			)

			BeforeAll(func() {
				var err error
				tempDirPath, err = os.MkdirTemp(tempDirPathRoot, "kube-vip-test")
				Expect(err).NotTo(HaveOccurred())
				_, containerIP = setupEnv(ctx, &cpVIP, &clusterName, tempDirPath, manifestValues, localIPv4, localIPv6, imagePath, configPath, k8sImagePath,
					e2e.DualstackFamily, utils.IPv4Family, []string{utils.IPv4Family}, &client, &gobgpPeers, v129, kubeVIPBGPManifestTemplate, &gobgpClient,
					logger, nodesNumber, "auto_sourceif", "mpbgp-if-ipv4", false, false, true)
			})

			AfterAll(func() {
				By(fmt.Sprintf("saving logs to %q", tempDirPath))
				err := e2e.GetLogs(ctx, client, tempDirPath, clusterName)
				Expect(err).ToNot(HaveOccurred())
				for _, p := range gobgpPeers {
					_, err := gobgpClient.DeletePeer(ctx, &api.DeletePeerRequest{
						Address: p.IP,
					})
					Expect(err).ToNot(HaveOccurred())
				}
				cleanupCluster(clusterName, defaultNetwork, ConfigMtx, logger)
			})

			DescribeTable("advertise IPv6 routes over IPv4 session",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv6Family, api.Family_AFI_IP6, []corev1.IPFamily{corev1.IPv6Protocol}, svcName,
						trafficPolicy, client, 1, gobgpClient, containerIP, "")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only stops advertising route if it was referenced by multiple services and all of them were deleted",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv6Family, api.Family_AFI_IP6, []corev1.IPFamily{corev1.IPv6Protocol}, svcName,
						trafficPolicy, client, 2, gobgpClient, containerIP, "")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)
		})

		Describe("kube-vip DualStack services BGP mode functionality with MP-BGP IPv4 over IPv6 - auto_sourceif nexthop, auto source interface", Ordered, func() {
			var (
				cpVIP          string
				clusterName    string
				client         kubernetes.Interface
				manifestValues *e2e.KubevipManifestValues
				gobgpClient    api.GobgpApiClient
				gobgpPeers     []*e2e.BGPPeerValues
				containerIP    string
				tempDirPath    string

				nodesNumber = 1
			)

			BeforeAll(func() {
				var err error
				tempDirPath, err = os.MkdirTemp(tempDirPathRoot, "kube-vip-test")
				Expect(err).NotTo(HaveOccurred())
				containerIP, _ = setupEnv(ctx, &cpVIP, &clusterName, tempDirPath, manifestValues, localIPv4, localIPv6, imagePath, configPath, k8sImagePath,
					e2e.DualstackFamilyIPv6, utils.IPv6Family, []string{utils.IPv6Family}, &client, &gobgpPeers, v129, kubeVIPBGPManifestTemplate, &gobgpClient,
					logger, nodesNumber, "auto_sourceif", "mpbgp-if-ipv6", false, false, true)
			})

			AfterAll(func() {
				By(fmt.Sprintf("saving logs to %q", tempDirPath))
				err := e2e.GetLogs(ctx, client, tempDirPath, clusterName)
				Expect(err).ToNot(HaveOccurred())
				for _, n := range gobgpPeers {
					_, err := gobgpClient.DeletePeer(ctx, &api.DeletePeerRequest{
						Address: n.IP,
					})
					Expect(err).ToNot(HaveOccurred())
				}
				cleanupCluster(clusterName, defaultNetwork, ConfigMtx, logger)
			})

			DescribeTable("advertise IPv4 routes over IPv6 session",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv4Family, api.Family_AFI_IP, []corev1.IPFamily{corev1.IPv4Protocol}, svcName,
						trafficPolicy, client, 1, gobgpClient, containerIP, "")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only stops advertising route if it was referenced by multiple services and all of them were deleted",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv4Family, api.Family_AFI_IP, []corev1.IPFamily{corev1.IPv4Protocol}, svcName,
						trafficPolicy, client, 2, gobgpClient, containerIP, "")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)
		})

		Describe("kube-vip IPv4 services BGP mode functionality, with service election", Ordered, func() {
			var (
				cpVIP          string
				clusterName    string
				client         kubernetes.Interface
				manifestValues *e2e.KubevipManifestValues
				gobgpClient    api.GobgpApiClient
				gobgpPeers     []*e2e.BGPPeerValues
				tempDirPath    string

				nodesNumber = 1
			)

			BeforeAll(func() {
				var err error
				tempDirPath, err = os.MkdirTemp(tempDirPathRoot, "kube-vip-test")
				Expect(err).NotTo(HaveOccurred())
				setupEnv(ctx, &cpVIP, &clusterName, tempDirPath, manifestValues, localIPv4, localIPv6, imagePath, configPath,
					k8sImagePath, utils.IPv4Family, utils.IPv4Family, []string{utils.IPv4Family}, &client, &gobgpPeers, v129,
					kubeVIPBGPManifestTemplate, &gobgpClient, logger, nodesNumber, "", "bgp-ipv4", true, false, true)
			})

			AfterAll(func() {
				By(fmt.Sprintf("saving logs to %q", tempDirPath))
				err := e2e.GetLogs(ctx, client, tempDirPath, clusterName)
				Expect(err).ToNot(HaveOccurred())
				for _, p := range gobgpPeers {
					Eventually(func() error {
						_, err := gobgpClient.DeletePeer(ctx, &api.DeletePeerRequest{
							Address: p.IP,
						})
						return err
					}, "30s", "200ms").Should(Succeed())
				}
				cleanupCluster(clusterName, defaultNetwork, ConfigMtx, logger)
			})

			DescribeTable("advertise IPv4 routes for services",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv4Family, api.Family_AFI_IP, []corev1.IPFamily{corev1.IPv4Protocol}, svcName,
						trafficPolicy, client, 1, gobgpClient, "", "")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only stops advertising route if it was referenced by multiple services and all of them were deleted",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv4Family, api.Family_AFI_IP, []corev1.IPFamily{corev1.IPv4Protocol}, svcName,
						trafficPolicy, client, 2, gobgpClient, "", "")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only stops advertising route if it was referenced by multiple services and all of them were deleted while using common lease",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv4Family, api.Family_AFI_IP, []corev1.IPFamily{corev1.IPv4Protocol}, svcName,
						trafficPolicy, client, 2, gobgpClient, "", "common-lease")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
			)
		})

		Describe("kube-vip IPv6 services BGP mode functionality, with service election", Ordered, func() {
			var (
				cpVIP          string
				clusterName    string
				client         kubernetes.Interface
				manifestValues *e2e.KubevipManifestValues
				gobgpClient    api.GobgpApiClient
				gobgpPeers     []*e2e.BGPPeerValues
				tempDirPath    string

				nodesNumber = 1
			)

			BeforeAll(func() {
				var err error
				tempDirPath, err = os.MkdirTemp(tempDirPathRoot, "kube-vip-test")
				Expect(err).NotTo(HaveOccurred())
				setupEnv(ctx, &cpVIP, &clusterName, tempDirPath, manifestValues, localIPv4, localIPv6, imagePath, configPath,
					k8sImagePath, utils.IPv6Family, utils.IPv6Family, []string{utils.IPv6Family}, &client, &gobgpPeers, v129,
					kubeVIPBGPManifestTemplate, &gobgpClient, logger, nodesNumber, "", "bgp-ipv4", true, false, true)
			})

			AfterAll(func() {
				By(fmt.Sprintf("saving logs to %q", tempDirPath))
				err := e2e.GetLogs(ctx, client, tempDirPath, clusterName)
				Expect(err).ToNot(HaveOccurred())
				for _, p := range gobgpPeers {
					_, err := gobgpClient.DeletePeer(ctx, &api.DeletePeerRequest{
						Address: p.IP,
					})
					Expect(err).ToNot(HaveOccurred())
				}
				cleanupCluster(clusterName, defaultNetwork, ConfigMtx, logger)
			})

			DescribeTable("advertise IPv6 routes for services",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv6Family, api.Family_AFI_IP6, []corev1.IPFamily{corev1.IPv6Protocol}, svcName,
						trafficPolicy, client, 1, gobgpClient, "", "")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only stops advertising route if it was referenced by multiple services and all of them were deleted",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv6Family, api.Family_AFI_IP6, []corev1.IPFamily{corev1.IPv6Protocol}, svcName,
						trafficPolicy, client, 2, gobgpClient, "", "")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only stops advertising route if it was referenced by multiple services and all of them were deleted while using common lease",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv6Family, api.Family_AFI_IP6, []corev1.IPFamily{corev1.IPv6Protocol}, svcName,
						trafficPolicy, client, 2, gobgpClient, "", "common-lease")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
			)
		})

		Describe("kube-vip DualStack services BGP mode functionality with MP-BGP IPv6 over IPv4 - fixed nexthop, with service election", Ordered, func() {
			var (
				cpVIP          string
				clusterName    string
				client         kubernetes.Interface
				manifestValues *e2e.KubevipManifestValues
				gobgpClient    api.GobgpApiClient
				gobgpPeers     []*e2e.BGPPeerValues
				tempDirPath    string

				nodesNumber = 1
			)

			BeforeAll(func() {
				var err error
				tempDirPath, err = os.MkdirTemp(tempDirPathRoot, "kube-vip-test")
				Expect(err).NotTo(HaveOccurred())
				setupEnv(ctx, &cpVIP, &clusterName, tempDirPath, manifestValues, localIPv4, localIPv6, imagePath, configPath, k8sImagePath,
					e2e.DualstackFamily, utils.IPv4Family, []string{utils.IPv4Family}, &client, &gobgpPeers, v129, kubeVIPBGPManifestTemplate, &gobgpClient,
					logger, nodesNumber, "fixed", "mpbgp-ipv4", true, false, true)
			})

			AfterAll(func() {
				By(fmt.Sprintf("saving logs to %q", tempDirPath))
				err := e2e.GetLogs(ctx, client, tempDirPath, clusterName)
				Expect(err).ToNot(HaveOccurred())
				for _, p := range gobgpPeers {
					_, err := gobgpClient.DeletePeer(ctx, &api.DeletePeerRequest{
						Address: p.IP,
					})
					Expect(err).ToNot(HaveOccurred())
				}
				cleanupCluster(clusterName, defaultNetwork, ConfigMtx, logger)
			})

			DescribeTable("advertise IPv6 routes over IPv4 session",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv6Family, api.Family_AFI_IP6, []corev1.IPFamily{corev1.IPv6Protocol}, svcName,
						trafficPolicy, client, 1, gobgpClient, defaultFixedNexthopv6, "")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only stops advertising route if it was referenced by multiple services and all of them were deleted",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv6Family, api.Family_AFI_IP6, []corev1.IPFamily{corev1.IPv6Protocol}, svcName,
						trafficPolicy, client, 2, gobgpClient, defaultFixedNexthopv6, "")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only stops advertising route if it was referenced by multiple services and all of them were deleted while using common lease",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv6Family, api.Family_AFI_IP6, []corev1.IPFamily{corev1.IPv6Protocol}, svcName,
						trafficPolicy, client, 2, gobgpClient, defaultFixedNexthopv6, "common-lease")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
			)
		})

		Describe("kube-vip DualStack services BGP mode functionality with MP-BGP IPv4 over IPv6 - fixed nexthop, with service election", Ordered, func() {
			var (
				cpVIP          string
				clusterName    string
				client         kubernetes.Interface
				manifestValues *e2e.KubevipManifestValues
				gobgpClient    api.GobgpApiClient
				gobgpPeers     []*e2e.BGPPeerValues
				tempDirPath    string

				nodesNumber = 1
			)

			BeforeAll(func() {
				var err error
				tempDirPath, err = os.MkdirTemp(tempDirPathRoot, "kube-vip-test")
				Expect(err).NotTo(HaveOccurred())
				setupEnv(ctx, &cpVIP, &clusterName, tempDirPath, manifestValues, localIPv4, localIPv6, imagePath, configPath, k8sImagePath,
					e2e.DualstackFamilyIPv6, utils.IPv6Family, []string{utils.IPv6Family}, &client, &gobgpPeers, v129, kubeVIPBGPManifestTemplate, &gobgpClient,
					logger, nodesNumber, "fixed", "mpbgp-ipv6", true, false, true)
			})

			AfterAll(func() {
				By(fmt.Sprintf("saving logs to %q", tempDirPath))
				err := e2e.GetLogs(ctx, client, tempDirPath, clusterName)
				Expect(err).ToNot(HaveOccurred())
				for _, n := range gobgpPeers {
					_, err := gobgpClient.DeletePeer(ctx, &api.DeletePeerRequest{
						Address: n.IP,
					})
					Expect(err).ToNot(HaveOccurred())
				}
				cleanupCluster(clusterName, defaultNetwork, ConfigMtx, logger)
			})

			DescribeTable("advertise IPv4 routes over IPv6 session",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv4Family, api.Family_AFI_IP, []corev1.IPFamily{corev1.IPv4Protocol}, svcName,
						trafficPolicy, client, 1, gobgpClient, defaultFixedNexthopv4, "")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only stops advertising route if it was referenced by multiple services and all of them were deleted",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv4Family, api.Family_AFI_IP, []corev1.IPFamily{corev1.IPv4Protocol}, svcName,
						trafficPolicy, client, 2, gobgpClient, defaultFixedNexthopv4, "")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only stops advertising route if it was referenced by multiple services and all of them were deleted while using common lease",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv4Family, api.Family_AFI_IP, []corev1.IPFamily{corev1.IPv4Protocol}, svcName,
						trafficPolicy, client, 2, gobgpClient, defaultFixedNexthopv4, "common-lease")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
			)
		})

		Describe("kube-vip DualStack services BGP mode functionality with MP-BGP IPv6 over IPv4 - auto_sourceif nexthop, with service election", Ordered, func() {
			var (
				cpVIP          string
				clusterName    string
				client         kubernetes.Interface
				manifestValues *e2e.KubevipManifestValues
				gobgpClient    api.GobgpApiClient
				gobgpPeers     []*e2e.BGPPeerValues
				containerIP    string
				tempDirPath    string

				nodesNumber = 1
			)

			BeforeAll(func() {
				var err error
				tempDirPath, err = os.MkdirTemp(tempDirPathRoot, "kube-vip-test")
				Expect(err).NotTo(HaveOccurred())
				_, containerIP = setupEnv(ctx, &cpVIP, &clusterName, tempDirPath, manifestValues, localIPv4, localIPv6, imagePath, configPath, k8sImagePath,
					e2e.DualstackFamily, utils.IPv4Family, []string{utils.IPv4Family}, &client, &gobgpPeers, v129, kubeVIPBGPManifestTemplate, &gobgpClient,
					logger, nodesNumber, "auto_sourceif", "mpbgp-if-ipv4", true, false, true)
			})

			AfterAll(func() {
				By(fmt.Sprintf("saving logs to %q", tempDirPath))
				err := e2e.GetLogs(ctx, client, tempDirPath, clusterName)
				Expect(err).ToNot(HaveOccurred())
				for _, p := range gobgpPeers {
					_, err := gobgpClient.DeletePeer(ctx, &api.DeletePeerRequest{
						Address: p.IP,
					})
					Expect(err).ToNot(HaveOccurred())
				}
				cleanupCluster(clusterName, defaultNetwork, ConfigMtx, logger)
			})

			DescribeTable("advertise IPv6 routes over IPv4 session",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv6Family, api.Family_AFI_IP6, []corev1.IPFamily{corev1.IPv6Protocol}, svcName,
						trafficPolicy, client, 1, gobgpClient, containerIP, "")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only stops advertising route if it was referenced by multiple services and all of them were deleted",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv6Family, api.Family_AFI_IP6, []corev1.IPFamily{corev1.IPv6Protocol}, svcName,
						trafficPolicy, client, 2, gobgpClient, containerIP, "")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only stops advertising route if it was referenced by multiple services and all of them were deleted while using common lease",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv6Family, api.Family_AFI_IP6, []corev1.IPFamily{corev1.IPv6Protocol}, svcName,
						trafficPolicy, client, 2, gobgpClient, containerIP, "common-lease")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
			)
		})

		Describe("kube-vip DualStack services BGP mode functionality with MP-BGP IPv4 over IPv6 - auto_sourceif nexthop, with service election", Ordered, func() {
			var (
				cpVIP          string
				clusterName    string
				client         kubernetes.Interface
				manifestValues *e2e.KubevipManifestValues
				gobgpClient    api.GobgpApiClient
				gobgpPeers     []*e2e.BGPPeerValues
				containerIP    string
				tempDirPath    string

				nodesNumber = 1
			)

			BeforeAll(func() {
				var err error
				tempDirPath, err = os.MkdirTemp(tempDirPathRoot, "kube-vip-test")
				Expect(err).NotTo(HaveOccurred())
				containerIP, _ = setupEnv(ctx, &cpVIP, &clusterName, tempDirPath, manifestValues, localIPv4, localIPv6, imagePath, configPath, k8sImagePath,
					e2e.DualstackFamilyIPv6, utils.IPv6Family, []string{utils.IPv6Family}, &client, &gobgpPeers, v129, kubeVIPBGPManifestTemplate, &gobgpClient,
					logger, nodesNumber, "auto_sourceif", "mpbgp-if-ipv6", true, false, true)
			})

			AfterAll(func() {
				By(fmt.Sprintf("saving logs to %q", tempDirPath))
				err := e2e.GetLogs(ctx, client, tempDirPath, clusterName)
				Expect(err).ToNot(HaveOccurred())
				for _, n := range gobgpPeers {
					_, err := gobgpClient.DeletePeer(ctx, &api.DeletePeerRequest{
						Address: n.IP,
					})
					Expect(err).ToNot(HaveOccurred())
				}
				cleanupCluster(clusterName, defaultNetwork, ConfigMtx, logger)
			})

			DescribeTable("advertise IPv4 routes over IPv6 session",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv4Family, api.Family_AFI_IP, []corev1.IPFamily{corev1.IPv4Protocol}, svcName,
						trafficPolicy, client, 1, gobgpClient, containerIP, "")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only stops advertising route if it was referenced by multiple services and all of them were deleted",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv4Family, api.Family_AFI_IP, []corev1.IPFamily{corev1.IPv4Protocol}, svcName,
						trafficPolicy, client, 2, gobgpClient, containerIP, "")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only stops advertising route if it was referenced by multiple services and all of them were deleted while using common lease",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv4Family, api.Family_AFI_IP, []corev1.IPFamily{corev1.IPv4Protocol}, svcName,
						trafficPolicy, client, 2, gobgpClient, containerIP, "common-lease")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
			)
		})

		Describe("kube-vip BGP configuration via node annotations", Ordered, func() {
			var (
				clusterName string
				client      kubernetes.Interface
				gobgpClient api.GobgpApiClient
				gobgpPeers  []*e2e.BGPPeerValues
				tempDirPath string
			)

			BeforeAll(func() {
				const (
					nodesNumber  = 1
					templateName = "kube-vip-bgp-annotations.yaml.tmpl"
				)

				var err error
				tempDirPath, err = os.MkdirTemp(tempDirPathRoot, "kube-vip-test")
				Expect(err).NotTo(HaveOccurred())

				networking := &kindconfigv1alpha4.Networking{
					IPFamily: kindconfigv1alpha4.IPv4Family,
				}

				manifestValues := &e2e.KubevipManifestValues{
					ImagePath:  imagePath,
					ConfigPath: configPath,
				}

				templatePath := filepath.Join(curDir, templateName)
				kubeVIPBGPManifestTemplate, err = template.New(templateName).ParseFiles(templatePath)
				Expect(err).NotTo(HaveOccurred())

				clusterName, client, _ = prepareCluster(ctx, tempDirPath, "bgp-annotations", k8sImagePath, v129,
					kubeVIPBGPManifestTemplate, logger, manifestValues, networking, nodesNumber, nil, 1)

				kvPeer := e2e.BGPPeerValues{
					IP:       localIPv4,
					AS:       goBGPAS,
					IPFamily: utils.IPv4Family,
				}

				err = annotateNodes(ctx, "test", client, kvPeer, kubevipAS)
				Expect(err).ToNot(HaveOccurred())

				container := fmt.Sprintf("%s-control-plane", clusterName)
				containerIPv4, _, err := GetContainerIPs(ctx, container)
				Expect(err).ToNot(HaveOccurred())

				gobgpPeers = append(gobgpPeers, &e2e.BGPPeerValues{
					IP:       containerIPv4,
					AS:       kubevipAS,
					IPFamily: utils.IPv4Family,
				})

				gobgpClient, err = newGoBGPClient(localIPv4, goBGPPort)
				Expect(err).ToNot(HaveOccurred())

				for _, p := range gobgpPeers {
					if slices.Contains([]string{utils.IPv4Family}, p.IPFamily) {
						Eventually(func() error {
							_, err = (gobgpClient).AddPeer(ctx, &api.AddPeerRequest{
								Peer: &api.Peer{
									Conf: &api.PeerConf{
										NeighborAddress: p.IP,
										PeerAsn:         uint32(p.AS),
									},
									AfiSafis: []*api.AfiSafi{
										{
											Config: &api.AfiSafiConfig{
												Enabled: true,
												Family: &api.Family{
													Afi:  api.Family_AFI_IP6,
													Safi: api.Family_SAFI_UNICAST,
												},
											},
										},
										{
											Config: &api.AfiSafiConfig{
												Enabled: true,
												Family: &api.Family{
													Afi:  api.Family_AFI_IP,
													Safi: api.Family_SAFI_UNICAST,
												},
											},
										},
									},
								},
							})
							return err
						}, "120s", "100ms").Should(Succeed())
					}
				}
			})

			AfterAll(func() {
				By(fmt.Sprintf("saving logs to %q", tempDirPath))
				err := e2e.GetLogs(ctx, client, tempDirPath, clusterName)
				Expect(err).ToNot(HaveOccurred())
				for _, p := range gobgpPeers {
					Eventually(func() error {
						_, err := gobgpClient.DeletePeer(ctx, &api.DeletePeerRequest{
							Address: p.IP,
						})
						return err
					}, "30s", "200ms").Should(Succeed())
				}
				cleanupCluster(clusterName, defaultNetwork, ConfigMtx, logger)
			})

			DescribeTable("advertise IPv4 routes for services",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(ctx, offset, utils.IPv4Family, api.Family_AFI_IP, []corev1.IPFamily{corev1.IPv4Protocol}, svcName,
						trafficPolicy, client, 1, gobgpClient, "", "")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)
		})
	}
})

func testBGP(ctx context.Context, offset uint, lbFamily string, afiFamily api.Family_Afi, svcFamily []corev1.IPFamily, svcName string,
	trafficPolicy corev1.ServiceExternalTrafficPolicy, client kubernetes.Interface, numberOfServices int,
	gobgpClient api.GobgpApiClient, expectedNexthop string, serviceLease string) {
	lbAddress := e2e.GenerateVIP(lbFamily, offset, defaultNetwork)
	routeCheckFamily := &api.Family{
		Afi:  afiFamily,
		Safi: api.Family_SAFI_UNICAST,
	}
	testServiceBGP(ctx, svcName, lbAddress, trafficPolicy, client, svcFamily, numberOfServices, gobgpClient, routeCheckFamily, expectedNexthop, serviceLease)
}

func setupEnv(ctx context.Context, cpVIP, clusterName *string, tempDirPath string, manifestValues *e2e.KubevipManifestValues,
	localIPv4, localIPv6, imagePath, configPath, k8sImagePath, clusterAddrFamily, bgpClientAddrFamily string, peerAddrFamily []string, client *kubernetes.Interface,
	gobgpPeers *[]*e2e.BGPPeerValues, v129 bool, kubeVIPBGPManifestTemplate *template.Template, gobgpClient *api.GobgpApiClient,
	logger log.Logger, nodesNumber int, mpbgpnexthop, clusterNameSuffix string, serviceElection, cpEnable, svcEnable bool) (string, string) {
	var err error

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

	networking := &kindconfigv1alpha4.Networking{
		IPFamily: clusterIPFamily,
	}

	if podSubnet != "" && serviceSubnet != "" {
		networking.PodSubnet = podSubnet
		networking.ServiceSubnet = serviceSubnet
	}

	kvPeers := []*e2e.BGPPeerValues{}
	if slices.Contains(peerAddrFamily, utils.IPv4Family) {
		kvPeers = append(kvPeers, &e2e.BGPPeerValues{
			IP:       localIPv4,
			AS:       goBGPAS,
			IPFamily: utils.IPv4Family,
		})
	}

	if slices.Contains(peerAddrFamily, utils.IPv6Family) {
		kvPeers = append(kvPeers, &e2e.BGPPeerValues{
			IP:       localIPv6,
			AS:       goBGPAS,
			IPFamily: utils.IPv6Family,
		})
	}

	kvPeersStr := []string{}
	for _, p := range kvPeers {
		kvPeersStr = append(kvPeersStr, p.String())
	}

	manifestValues = &e2e.KubevipManifestValues{
		ControlPlaneVIP:    *cpVIP,
		ImagePath:          imagePath,
		ConfigPath:         configPath,
		ControlPlaneEnable: fmt.Sprintf("%t", cpEnable),
		SvcEnable:          fmt.Sprintf("%t", svcEnable),
		SvcElectionEnable:  fmt.Sprintf("%t", serviceElection),
		EnableNodeLabeling: "false",
		BGPAS:              kubevipAS,
		BGPPeers:           strings.Join(kvPeersStr, ","),
		MPBGPNexthop:       mpbgpnexthop,
		MPBGPNexthopIPv4:   defaultFixedNexthopv4,
		MPBGPNexthopIPv6:   defaultFixedNexthopv6,
	}

	By(manifestValues.BGPPeers)

	*clusterName, *client, _ = prepareCluster(ctx, tempDirPath, clusterNameSuffix, k8sImagePath, v129,
		kubeVIPBGPManifestTemplate, logger, manifestValues, networking, nodesNumber, nil, 1)

	container := fmt.Sprintf("%s-control-plane", *clusterName)

	containerIPv4, containerIPv6, err := GetContainerIPs(ctx, container)
	Expect(err).ToNot(HaveOccurred())

	if slices.Contains(peerAddrFamily, utils.IPv4Family) {
		*gobgpPeers = append(*gobgpPeers, &e2e.BGPPeerValues{
			IP:       containerIPv4,
			AS:       kubevipAS,
			IPFamily: utils.IPv4Family,
		})
	}

	if slices.Contains(peerAddrFamily, utils.IPv6Family) {
		*gobgpPeers = append(*gobgpPeers, &e2e.BGPPeerValues{
			IP:       containerIPv6,
			AS:       kubevipAS,
			IPFamily: utils.IPv6Family,
		})
	}

	if bgpClientAddrFamily == utils.IPv6Family {
		*gobgpClient, err = newGoBGPClient(localIPv6, goBGPPort)
	} else {
		*gobgpClient, err = newGoBGPClient(localIPv4, goBGPPort)
	}
	Expect(err).ToNot(HaveOccurred())

	for _, p := range *gobgpPeers {
		if slices.Contains(peerAddrFamily, p.IPFamily) {
			Eventually(func() error {
				_, err = (*gobgpClient).AddPeer(ctx, &api.AddPeerRequest{
					Peer: &api.Peer{
						Conf: &api.PeerConf{
							NeighborAddress: p.IP,
							PeerAsn:         uint32(p.AS),
						},
						AfiSafis: []*api.AfiSafi{
							{
								Config: &api.AfiSafiConfig{
									Enabled: true,
									Family: &api.Family{
										Afi:  api.Family_AFI_IP6,
										Safi: api.Family_SAFI_UNICAST,
									},
								},
							},
							{
								Config: &api.AfiSafiConfig{
									Enabled: true,
									Family: &api.Family{
										Afi:  api.Family_AFI_IP,
										Safi: api.Family_SAFI_UNICAST,
									},
								},
							},
						},
					},
				})
				return err
			}, "120s", "100ms").Should(Succeed())
		}
	}

	return containerIPv4, containerIPv6
}

func testServiceBGP(ctx context.Context, svcName, lbAddress string, trafficPolicy corev1.ServiceExternalTrafficPolicy,
	client kubernetes.Interface, serviceAddrFamily []corev1.IPFamily, numberOfServices int,
	gobgpClient api.GobgpApiClient, gobgpFamily *api.Family, expectedNexthop string,
	serviceLease string) {
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
		paths := checkGoBGPPaths(ctx, gobgpClient, gobgpFamily, []*api.TableLookupPrefix{{Prefix: addr}}, 1)
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
				paths := checkGoBGPPaths(ctx, gobgpClient, gobgpFamily, []*api.TableLookupPrefix{{Prefix: addr}}, 1)
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
		checkGoBGPPaths(ctx, gobgpClient, gobgpFamily, []*api.TableLookupPrefix{{Prefix: addr}}, 0)
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
		paths := checkGoBGPPaths(ctx, gobgpClient, gobgpFamily, []*api.TableLookupPrefix{{Prefix: addr}}, 1)
		Expect(paths).ToNot(BeNil())
		Expect(paths).ToNot(BeEmpty())
		Expect(strings.Contains(paths[0].Prefix, addr)).To(BeTrue())
	}
}

func GetContainerIPs(ctx context.Context, containerName string) (string, string, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		return "", "", fmt.Errorf("failed to create client: %w", err)
	}
	containers, err := cli.ContainerList(ctx, container.ListOptions{})
	if err != nil {
		return "", "", fmt.Errorf("failed to list containers: %w", err)
	}

	for _, c := range containers {
		for _, n := range c.Names {
			if n[1:] == containerName {
				for _, n := range c.NetworkSettings.Networks {
					return n.IPAddress, n.GlobalIPv6Address, nil
				}
			}
		}
	}

	return "", "", nil
}

func newGoBGPClient(address string, port uint32) (api.GobgpApiClient, error) {
	grpcOpts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	target := net.JoinHostPort(address, strconv.Itoa(int(port)))
	conn, err := grpc.NewClient(target, grpcOpts...)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to GoBGP server %q: %w", target, err)
	}

	return api.NewGobgpApiClient(conn), nil
}

func checkGoBGPPaths(ctx context.Context, client api.GobgpApiClient, family *api.Family, prefixes []*api.TableLookupPrefix, expectedPaths int) []*api.Destination {
	var paths []*api.Destination
	Eventually(func() error {
		var err error
		paths, err = getGoBGPPaths(ctx, client, family, prefixes)
		if err != nil {
			return err
		}
		if len(paths) != expectedPaths {
			return fmt.Errorf("expected %d paths, but found %d", expectedPaths, len(paths))
		}
		return nil
	}, "30s", "1s").ShouldNot(HaveOccurred())
	return paths
}

func getGoBGPPaths(ctx context.Context, client api.GobgpApiClient, family *api.Family, prefixes []*api.TableLookupPrefix) ([]*api.Destination, error) {
	pathCtx, cancel := context.WithTimeout(ctx, time.Second*5)
	defer cancel()
	stream, err := client.ListPath(pathCtx, &api.ListPathRequest{
		TableType: api.TableType_GLOBAL,
		Family:    family,
		Name:      "",
		Prefixes:  prefixes,
		SortType:  api.ListPathRequest_PREFIX,
	})
	if err != nil {
		return nil, err
	}

	rib := make([]*api.Destination, 0)
	for {
		r, err := stream.Recv()
		if err != nil && err != io.EOF {
			return nil, err
		}
		if r != nil {
			rib = append(rib, r.Destination)
		}
		if err != nil && err == io.EOF {
			break
		}
	}

	return rib, nil
}

// annotateNodes adds all BGP configuration annotations to all cluster nodes.
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

func startGoBGP(config string, kill chan any) {
	By("starting GoBGP server")
	cmd := exec.Command("../../bin/gobgpd", "-f", config)
	go cmd.Run()
	<-kill
	By("stopping GoBGP server")
	err := cmd.Process.Kill()
	Expect(err).ToNot(HaveOccurred())
}
