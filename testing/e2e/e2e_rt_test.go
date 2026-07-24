//go:build e2e
// +build e2e

package e2e_test

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
)

var _ = Describe("kube-vip routing table mode", Ordered, func() {
	if Mode == ModeRT {
		var (
			logger                              log.Logger
			imagePath                           string
			k8sImagePath                        string
			configPath                          string
			kubeVIPRoutingTableManifestTemplate *template.Template
			tempDirPathRoot                     string
			v129                                bool
		)

		dsNumber := 1

		ctx, cancel := context.WithCancel(context.TODO())

		BeforeAll(func() {
			tempDirPathRoot = MustMkdirTemp("", fmt.Sprintf("%s-rt", testDirPrefix))
		})

		BeforeEach(func() {
			klog.SetOutput(GinkgoWriter)
			logger = e2e.TestLogger{}

			imagePath = os.Getenv("E2E_IMAGE_PATH")    // Path to kube-vip image
			configPath = os.Getenv("CONFIG_PATH")      // path to the api server config
			k8sImagePath = os.Getenv("K8S_IMAGE_PATH") // path to the kubernetes image (version for kind)
			if configPath == "" {
				configPath = "/etc/kubernetes/admin.conf"
			}
			_, v129 = os.LookupEnv("V129")
			curDir, err := os.Getwd()
			Expect(err).NotTo(HaveOccurred())

			templateRoutingTablePath := filepath.Join(curDir, "kube-vip-routing-table.yaml.tmpl")
			kubeVIPRoutingTableManifestTemplate, err = template.New("kube-vip-routing-table.yaml.tmpl").ParseFiles(templateRoutingTablePath)
			Expect(err).NotTo(HaveOccurred())
		})

		AfterAll(func() {
			if os.Getenv("E2E_KEEP_LOGS") != "true" {
				Expect(os.RemoveAll(tempDirPathRoot)).To(Succeed())
			}
			cancel()
		})

		Describe("kube-vip IPv4 control-plane routing table mode functionality", Ordered, func() {
			var (
				cpVIP       string
				clusterName string
				client      kubernetes.Interface
				tempDirPath string

				nodesNumber = 3
			)

			BeforeAll(func() {
				cpVIP = e2e.GenerateVIP(utils.IPv4Family, SOffset.Get(), defaultNetwork)

				networking := &kindconfigv1alpha4.Networking{
					IPFamily: kindconfigv1alpha4.IPv4Family,
				}

				manifestValues := &e2e.KubevipManifestValues{
					ControlPlaneVIP:       cpVIP,
					ImagePath:             imagePath,
					ConfigPath:            configPath,
					ControlPlaneEnable:    "true",
					SvcEnable:             "false",
					SvcElectionEnable:     "false",
					EnableNodeLabeling:    "false",
					EnableServiceSecurity: "true",
				}

				tempDirPath = MustMkdirTemp(tempDirPathRoot, testDirPrefix)

				clusterName, client, _ = prepareCluster(ctx, tempDirPath, "rt-ipv4", k8sImagePath, v129,
					kubeVIPRoutingTableManifestTemplate, logger, manifestValues, networking, nodesNumber, nil, dsNumber)
			})

			AfterAll(func() {
				SaveLogsAndCleanup(ctx, client, tempDirPath, clusterName, func() {
					cleanupCluster(clusterName, defaultNetwork, ConfigMtx, logger)
				})
			})

			It("setups IPv4 address and route on control-plane node", func() {
				By(withTimestamp("sitting for a few seconds to hopefully allow kube-vip to start"))
				time.Sleep(30 * time.Second)

				for i := 1; i <= nodesNumber; i++ {
					var container string
					if i > 1 {
						container = fmt.Sprintf("%s-control-plane%d", clusterName, i)
					} else {
						container = fmt.Sprintf("%s-control-plane", clusterName)
					}

					Expect(checkIPAddress(cpVIP, container, true)).To(BeTrue())
					e2e.CheckRoutePresence(cpVIP, container, true)
				}
			})
		})

		Describe("kube-vip IPv4 control-plane routing table mode with control-plane health check", Ordered, func() {
			var (
				cpVIP       string
				clusterName string
				client      kubernetes.Interface
				tempDirPath string

				nodesNumber = 3
			)

			BeforeAll(func() {
				cpVIP = e2e.GenerateVIP(utils.IPv4Family, SOffset.Get(), defaultNetwork)

				networking := &kindconfigv1alpha4.Networking{
					IPFamily: kindconfigv1alpha4.IPv4Family,
				}

				manifestValues := &e2e.KubevipManifestValues{
					ControlPlaneVIP:                         cpVIP,
					ImagePath:                               imagePath,
					ConfigPath:                              configPath,
					ControlPlaneEnable:                      "true",
					SvcEnable:                               "false",
					SvcElectionEnable:                       "false",
					EnableNodeLabeling:                      "false",
					ControlPlaneHealthCheckAddress:          "https://localhost:6443/livez",
					ControlPlaneHealthCheckPeriodSeconds:    1,
					ControlPlaneHealthCheckTimeoutSeconds:   1,
					ControlPlaneHealthCheckFailureThreshold: 3,
					ControlPlaneHealthCheckCAPath:           "/etc/kubernetes/pki/ca.crt",
				}

				var err error
				tempDirPath, err = os.MkdirTemp(tempDirPathRoot, testDirPrefix)
				Expect(err).NotTo(HaveOccurred())

				clusterName, client, _ = prepareCluster(ctx, tempDirPath, "rt-ipv4-hc", k8sImagePath, v129,
					kubeVIPRoutingTableManifestTemplate, logger, manifestValues, networking, nodesNumber, nil, dsNumber)
			})

			AfterAll(func() {
				By(fmt.Sprintf("saving logs to %q", tempDirPath))
				err := e2e.GetLogs(ctx, client, tempDirPath, clusterName)
				Expect(err).ToNot(HaveOccurred())
				cleanupCluster(clusterName, defaultNetwork, ConfigMtx, logger)
			})

			It("withdraws and re-adds the VIP and route when the apiserver health check fails", func() {
				By(withTimestamp("sitting for a few seconds to hopefully allow kube-vip to start"))
				time.Sleep(30 * time.Second)

				By("verifying every control-plane node sets up the VIP and route while healthy")
				for i := 1; i <= nodesNumber; i++ {
					container := controlPlaneContainerName(clusterName, i)
					Expect(checkIPAddress(cpVIP, container, true)).To(BeTrue())
					e2e.CheckRoutePresence(cpVIP, container, true)
				}

				// Pick a non-first node so the others keep serving the control plane.
				targetContainer := controlPlaneContainerName(clusterName, 2)
				otherContainer := controlPlaneContainerName(clusterName, 1)

				By(fmt.Sprintf("stopping the apiserver on %q", targetContainer))
				setAPIServerState(targetContainer, false)

				By("verifying the unhealthy node withdraws its VIP and route")
				Expect(checkIPAddress(cpVIP, targetContainer, false)).To(BeTrue())
				e2e.CheckRoutePresence(cpVIP, targetContainer, false)

				By("verifying a healthy node keeps its VIP and route")
				Expect(checkIPAddress(cpVIP, otherContainer, true)).To(BeTrue())
				e2e.CheckRoutePresence(cpVIP, otherContainer, true)

				By(fmt.Sprintf("restoring the apiserver on %q", targetContainer))
				setAPIServerState(targetContainer, true)

				By("verifying the recovered node re-adds its VIP and route")
				// Allow extra time here: the apiserver static pod must be restarted
				// by the kubelet and become healthy again before kube-vip re-adds the VIP.
				Eventually(func() bool {
					return e2e.CheckIPAddressPresence(cpVIP, targetContainer, true)
				}, "180s", "2s").Should(BeTrue(), "VIP should be re-added after apiserver recovery")
				e2e.CheckRoutePresence(cpVIP, targetContainer, true)
			})
		})

		Describe("kube-vip IPv6 control-plane routing table mode functionality", Ordered, func() {
			var (
				cpVIP       string
				clusterName string
				client      kubernetes.Interface
				tempDirPath string

				nodesNumber = 3
			)

			BeforeAll(func() {

				cpVIP = e2e.GenerateVIP(utils.IPv6Family, SOffset.Get(), defaultNetwork)

				networking := &kindconfigv1alpha4.Networking{
					IPFamily: kindconfigv1alpha4.IPv6Family,
				}

				manifestValues := &e2e.KubevipManifestValues{
					ControlPlaneVIP:       cpVIP,
					ImagePath:             imagePath,
					ConfigPath:            configPath,
					ControlPlaneEnable:    "true",
					SvcEnable:             "false",
					SvcElectionEnable:     "false",
					EnableNodeLabeling:    "false",
					EnableServiceSecurity: "true",
				}

				tempDirPath = MustMkdirTemp(tempDirPathRoot, testDirPrefix)

				clusterName, client, _ = prepareCluster(ctx, tempDirPath, "rt-ipv6", k8sImagePath, v129,
					kubeVIPRoutingTableManifestTemplate, logger, manifestValues, networking, nodesNumber, nil, dsNumber)
			})

			AfterAll(func() {
				SaveLogsAndCleanup(ctx, client, tempDirPath, clusterName, func() {
					cleanupCluster(clusterName, defaultNetwork, ConfigMtx, logger)
				})
			})

			It("setups IPv6 address and route on control-plane node", func() {
				By(withTimestamp("sitting for a few seconds to hopefully allow kube-vip to start"))
				time.Sleep(30 * time.Second)

				for i := 1; i <= nodesNumber; i++ {
					var container string
					if i > 1 {
						container = fmt.Sprintf("%s-control-plane%d", clusterName, i)
					} else {
						container = fmt.Sprintf("%s-control-plane", clusterName)
					}

					Expect(checkIPAddress(cpVIP, container, true)).To(BeTrue())
					e2e.CheckRoutePresence(cpVIP, container, true)
				}
			})
		})

		Describe("kube-vip IPv6 control-plane routing table mode with control-plane health check", Ordered, func() {
			var (
				cpVIP       string
				clusterName string
				client      kubernetes.Interface
				tempDirPath string

				nodesNumber = 3
			)

			BeforeAll(func() {
				cpVIP = e2e.GenerateVIP(utils.IPv6Family, SOffset.Get(), defaultNetwork)

				networking := &kindconfigv1alpha4.Networking{
					IPFamily: kindconfigv1alpha4.IPv6Family,
				}

				manifestValues := &e2e.KubevipManifestValues{
					ControlPlaneVIP:                         cpVIP,
					ImagePath:                               imagePath,
					ConfigPath:                              configPath,
					ControlPlaneEnable:                      "true",
					SvcEnable:                               "false",
					SvcElectionEnable:                       "false",
					EnableNodeLabeling:                      "false",
					ControlPlaneHealthCheckAddress:          "https://localhost:6443/livez",
					ControlPlaneHealthCheckPeriodSeconds:    1,
					ControlPlaneHealthCheckTimeoutSeconds:   1,
					ControlPlaneHealthCheckFailureThreshold: 3,
					ControlPlaneHealthCheckCAPath:           "/etc/kubernetes/pki/ca.crt",
				}

				var err error
				tempDirPath, err = os.MkdirTemp(tempDirPathRoot, testDirPrefix)
				Expect(err).NotTo(HaveOccurred())

				clusterName, client, _ = prepareCluster(ctx, tempDirPath, "rt-ipv6-hc", k8sImagePath, v129,
					kubeVIPRoutingTableManifestTemplate, logger, manifestValues, networking, nodesNumber, nil, dsNumber)
			})

			AfterAll(func() {
				By(fmt.Sprintf("saving logs to %q", tempDirPath))
				err := e2e.GetLogs(ctx, client, tempDirPath, clusterName)
				Expect(err).ToNot(HaveOccurred())
				cleanupCluster(clusterName, defaultNetwork, ConfigMtx, logger)
			})

			It("withdraws and re-adds the VIP and route when the apiserver health check fails", func() {
				By(withTimestamp("sitting for a few seconds to hopefully allow kube-vip to start"))
				time.Sleep(30 * time.Second)

				By("verifying every control-plane node sets up the VIP and route while healthy")
				for i := 1; i <= nodesNumber; i++ {
					container := controlPlaneContainerName(clusterName, i)
					Expect(checkIPAddress(cpVIP, container, true)).To(BeTrue())
					e2e.CheckRoutePresence(cpVIP, container, true)
				}

				// Pick a non-first node so the others keep serving the control plane.
				targetContainer := controlPlaneContainerName(clusterName, 2)
				otherContainer := controlPlaneContainerName(clusterName, 1)

				By(fmt.Sprintf("stopping the apiserver on %q", targetContainer))
				setAPIServerState(targetContainer, false)

				By("verifying the unhealthy node withdraws its VIP and route")
				Expect(checkIPAddress(cpVIP, targetContainer, false)).To(BeTrue())
				e2e.CheckRoutePresence(cpVIP, targetContainer, false)

				By("verifying a healthy node keeps its VIP and route")
				Expect(checkIPAddress(cpVIP, otherContainer, true)).To(BeTrue())
				e2e.CheckRoutePresence(cpVIP, otherContainer, true)

				By(fmt.Sprintf("restoring the apiserver on %q", targetContainer))
				setAPIServerState(targetContainer, true)

				By("verifying the recovered node re-adds its VIP and route")
				// Allow extra time here: the apiserver static pod must be restarted
				// by the kubelet and become healthy again before kube-vip re-adds the VIP.
				Eventually(func() bool {
					return e2e.CheckIPAddressPresence(cpVIP, targetContainer, true)
				}, "180s", "2s").Should(BeTrue(), "VIP should be re-added after apiserver recovery")
				e2e.CheckRoutePresence(cpVIP, targetContainer, true)
			})
		})

		Describe("kube-vip DualStack control-plane routing table mode functionality - IPv4 primary", Ordered, func() {
			var (
				cpVIP       string
				clusterName string
				client      kubernetes.Interface
				tempDirPath string

				nodesNumber = 3
			)

			BeforeAll(func() {
				cpVIP = e2e.GenerateDualStackVIP(SOffset.Get(), defaultNetwork)

				networking := &kindconfigv1alpha4.Networking{
					IPFamily: kindconfigv1alpha4.DualStackFamily,
				}

				manifestValues := &e2e.KubevipManifestValues{
					ControlPlaneVIP:       cpVIP,
					ImagePath:             imagePath,
					ConfigPath:            configPath,
					ControlPlaneEnable:    "true",
					SvcEnable:             "false",
					SvcElectionEnable:     "false",
					EnableServiceSecurity: "true",
				}

				networkInterface := ""
				if networkInterface = os.Getenv("NETWORK_INTERFACE"); networkInterface == "" {
					networkInterface = "br-"
				}

				localIPv6, localIPv6Net, err := deployment.GetLocalIPv6(networkInterface)
				Expect(err).ToNot(HaveOccurred())

				addSAN := &san{
					ip:    localIPv6,
					ipnet: localIPv6Net,
				}

				tempDirPath = MustMkdirTemp(tempDirPathRoot, testDirPrefix)

				clusterName, client, _ = prepareCluster(ctx, tempDirPath, "rt-ds-ipv4", k8sImagePath, v129,
					kubeVIPRoutingTableManifestTemplate, logger, manifestValues, networking, nodesNumber, addSAN, dsNumber)
			})

			AfterAll(func() {
				SaveLogsAndCleanup(ctx, client, tempDirPath, clusterName, func() {
					cleanupCluster(clusterName, defaultNetwork, ConfigMtx, logger)
				})
			})

			It("setups DualStack addresses and routes on control-plane nodes", func() {
				By(withTimestamp("sitting for a few seconds to hopefully allow kube-vip to start"))
				time.Sleep(30 * time.Second)

				for i := 1; i <= nodesNumber; i++ {
					var container string
					if i > 1 {
						container = fmt.Sprintf("%s-control-plane%d", clusterName, i)
					} else {
						container = fmt.Sprintf("%s-control-plane", clusterName)
					}

					addresses := vip.Split(cpVIP)

					for _, addr := range addresses {
						Expect(checkIPAddress(addr, container, true)).To(BeTrue())
						e2e.CheckRoutePresence(addr, container, true)
					}
				}
			})
		})

		Describe("kube-vip DualStack control-plane routing table mode functionality - IPv6 primary", Ordered, func() {
			var (
				cpVIP       string
				clusterName string
				client      kubernetes.Interface
				tempDirPath string

				nodesNumber = 3
			)

			BeforeAll(func() {
				cpVIP = e2e.GenerateDualStackVIP(SOffset.Get(), defaultNetwork)

				networking := &kindconfigv1alpha4.Networking{
					IPFamily:      kindconfigv1alpha4.DualStackFamily,
					PodSubnet:     "fd00:10:244::/56,10.244.0.0/16",
					ServiceSubnet: "fd00:10:96::/112,10.96.0.0/16",
				}

				manifestValues := &e2e.KubevipManifestValues{
					ControlPlaneVIP:       cpVIP,
					ImagePath:             imagePath,
					ConfigPath:            configPath,
					ControlPlaneEnable:    "true",
					SvcEnable:             "false",
					SvcElectionEnable:     "false",
					EnableServiceSecurity: "true",
				}

				networkInterface := ""
				if networkInterface = os.Getenv("NETWORK_INTERFACE"); networkInterface == "" {
					networkInterface = "br-"
				}

				localIPv4, localIPv4Net, err := deployment.GetLocalIPv4(networkInterface)
				Expect(err).ToNot(HaveOccurred())

				addSAN := &san{
					ip:    localIPv4,
					ipnet: localIPv4Net,
				}

				tempDirPath = MustMkdirTemp(tempDirPathRoot, testDirPrefix)

				clusterName, client, _ = prepareCluster(ctx, tempDirPath, "rt-ds-ipv6", k8sImagePath, v129,
					kubeVIPRoutingTableManifestTemplate, logger, manifestValues, networking, nodesNumber, addSAN, dsNumber)
			})

			AfterAll(func() {
				SaveLogsAndCleanup(ctx, client, tempDirPath, clusterName, func() {
					cleanupCluster(clusterName, defaultNetwork, ConfigMtx, logger)
				})
			})

			It("setups DualStack addresses and routes on control-plane nodes", func() {
				By(withTimestamp("sitting for a few seconds to hopefully allow kube-vip to start"))
				time.Sleep(30 * time.Second)

				for i := 1; i <= nodesNumber; i++ {
					var container string
					if i > 1 {
						container = fmt.Sprintf("%s-control-plane%d", clusterName, i)
					} else {
						container = fmt.Sprintf("%s-control-plane", clusterName)
					}

					addresses := vip.Split(cpVIP)

					for _, addr := range addresses {
						Expect(checkIPAddress(addr, container, true)).To(BeTrue())
						e2e.CheckRoutePresence(addr, container, true)
					}
				}
			})
		})

		Describe("kube-vip IPv4 services routing table mode functionality", Ordered, func() {
			var (
				cpVIP       string
				clusterName string
				client      kubernetes.Interface
				svcElection bool
				ipFamily    []corev1.IPFamily
				tempDirPath string

				nodesNumber = 1
			)

			BeforeAll(func() {
				cpVIP = e2e.GenerateVIP(utils.IPv4Family, SOffset.Get(), defaultNetwork)

				networking := &kindconfigv1alpha4.Networking{
					IPFamily: kindconfigv1alpha4.IPv4Family,
				}

				manifestValues := &e2e.KubevipManifestValues{
					ControlPlaneVIP:       cpVIP,
					ImagePath:             imagePath,
					ConfigPath:            configPath,
					ControlPlaneEnable:    "false",
					SvcEnable:             "true",
					SvcElectionEnable:     "false",
					EnableServiceSecurity: "true",
				}

				var err error
				svcElection, err = strconv.ParseBool(manifestValues.SvcElectionEnable)
				Expect(err).ToNot(HaveOccurred())

				ipFamily = []corev1.IPFamily{corev1.IPv4Protocol}

				tempDirPath = MustMkdirTemp(tempDirPathRoot, testDirPrefix)

				clusterName, client, _ = prepareCluster(ctx, tempDirPath, "rt-svc-ipv4-no-election", k8sImagePath, v129,
					kubeVIPRoutingTableManifestTemplate, logger, manifestValues, networking, nodesNumber, nil, dsNumber)
			})

			AfterAll(func() {
				SaveLogsAndCleanup(ctx, client, tempDirPath, clusterName, func() {
					cleanupCluster(clusterName, defaultNetwork, ConfigMtx, logger)
				})
			})

			DescribeTable("configures an IPv4 routes for services",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					lbAddress := e2e.GenerateVIP(utils.IPv4Family, offset, defaultNetwork)
					testServiceRT(ctx, svcName, lbAddress, "plndr-svcs-lock", "kube-system", clusterName,
						trafficPolicy, client, svcElection, ipFamily, 1, false, dsNumber, false)
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only removes route if it was referenced by multiple services and all of them were deleted",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					lbAddress := e2e.GenerateVIP(utils.IPv4Family, offset, defaultNetwork)
					testServiceRT(ctx, svcName, lbAddress, "plndr-svcs-lock", "kube-system", clusterName,
						trafficPolicy, client, svcElection, ipFamily, 2, false, dsNumber, false)
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)
		})

		Describe("kube-vip IPv4 services routing table mode functionality with mixed election", Ordered, func() {
			var (
				cpVIP       string
				clusterName string
				client      kubernetes.Interface
				svcElection bool
				ipFamily    []corev1.IPFamily
				tempDirPath string

				nodesNumber = 1
			)

			BeforeAll(func() {
				cpVIP = e2e.GenerateVIP(utils.IPv4Family, SOffset.Get(), defaultNetwork)

				networking := &kindconfigv1alpha4.Networking{
					IPFamily: kindconfigv1alpha4.IPv4Family,
				}

				manifestValues := &e2e.KubevipManifestValues{
					ControlPlaneVIP:            cpVIP,
					ImagePath:                  imagePath,
					ConfigPath:                 configPath,
					ControlPlaneEnable:         "false",
					SvcEnable:                  "true",
					SvcElectionEnable:          "false",
					VipElectionEnable:          "false",
					EnableServiceSecurity:      "true",
					PerServiceElectionOnDemand: "true",
				}

				var err error
				svcElection, err = strconv.ParseBool(manifestValues.SvcElectionEnable)
				Expect(err).ToNot(HaveOccurred())

				ipFamily = []corev1.IPFamily{corev1.IPv4Protocol}

				tempDirPath = MustMkdirTemp(tempDirPathRoot, testDirPrefix)

				clusterName, client, _ = prepareCluster(ctx, tempDirPath, "rt-svc-ipv4-mixed", k8sImagePath, v129,
					kubeVIPRoutingTableManifestTemplate, logger, manifestValues, networking, nodesNumber, nil, dsNumber)
			})

			AfterAll(func() {
				SaveLogsAndCleanup(ctx, client, tempDirPath, clusterName, func() {
					cleanupCluster(clusterName, defaultNetwork, ConfigMtx, logger)
				})
			})

			DescribeTable("configures an IPv4 routes for services",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					lbAddresses := []string{}
					for range 2 {
						lbAddresses = append(lbAddresses, e2e.GenerateVIP(utils.IPv4Family, offset, defaultNetwork))
					}
					testServiceRTMixedElection(ctx, svcName, lbAddresses, "plndr-svcs-lock", "kube-system", fmt.Sprintf("kubevip-%s", svcName), dsNamespace,
						trafficPolicy, client, svcElection, ipFamily, 1, 1, dsNumber, false, clusterName)
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)
		})

		Describe("kube-vip IPv6 services routing table mode functionality", Ordered, func() {
			var (
				cpVIP       string
				clusterName string
				client      kubernetes.Interface
				svcElection bool
				ipFamily    []corev1.IPFamily
				tempDirPath string

				nodesNumber = 1
			)

			BeforeAll(func() {
				cpVIP = e2e.GenerateVIP(utils.IPv6Family, SOffset.Get(), defaultNetwork)

				networking := &kindconfigv1alpha4.Networking{
					IPFamily: kindconfigv1alpha4.IPv6Family,
				}

				manifestValues := &e2e.KubevipManifestValues{
					ControlPlaneVIP:       cpVIP,
					ImagePath:             imagePath,
					ConfigPath:            configPath,
					ControlPlaneEnable:    "false",
					SvcEnable:             "true",
					SvcElectionEnable:     "false",
					EnableServiceSecurity: "true",
				}

				var err error
				svcElection, err = strconv.ParseBool(manifestValues.SvcElectionEnable)
				Expect(err).ToNot(HaveOccurred())

				ipFamily = []corev1.IPFamily{corev1.IPv6Protocol}

				tempDirPath = MustMkdirTemp(tempDirPathRoot, testDirPrefix)

				clusterName, client, _ = prepareCluster(ctx, tempDirPath, "rt-svc-ipv6", k8sImagePath, v129,
					kubeVIPRoutingTableManifestTemplate, logger, manifestValues, networking, nodesNumber, nil, dsNumber)
			})

			AfterAll(func() {
				SaveLogsAndCleanup(ctx, client, tempDirPath, clusterName, func() {
					cleanupCluster(clusterName, defaultNetwork, ConfigMtx, logger)
				})
			})

			DescribeTable("configures an IPv6 routes for services",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					lbAddress := e2e.GenerateVIP(utils.IPv6Family, offset, defaultNetwork)
					testServiceRT(ctx, svcName, lbAddress, "plndr-svcs-lock", "kube-system", clusterName,
						trafficPolicy, client, svcElection, ipFamily, 1, false, dsNumber, false)
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only removes route if it was referenced by multiple services and all of them were deleted",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					lbAddress := e2e.GenerateVIP(utils.IPv6Family, offset, defaultNetwork)
					testServiceRT(ctx, svcName, lbAddress, "plndr-svcs-lock", "kube-system", clusterName,
						trafficPolicy, client, svcElection, ipFamily, 2, false, dsNumber, false)
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)
		})

		Describe("kube-vip IPv6 services routing table mode functionality with mixed election", Ordered, func() {
			var (
				cpVIP       string
				clusterName string
				client      kubernetes.Interface
				svcElection bool
				ipFamily    []corev1.IPFamily
				tempDirPath string

				nodesNumber = 1
			)

			BeforeAll(func() {
				cpVIP = e2e.GenerateVIP(utils.IPv6Family, SOffset.Get(), defaultNetwork)

				networking := &kindconfigv1alpha4.Networking{
					IPFamily: kindconfigv1alpha4.IPv6Family,
				}

				manifestValues := &e2e.KubevipManifestValues{
					ControlPlaneVIP:            cpVIP,
					ImagePath:                  imagePath,
					ConfigPath:                 configPath,
					ControlPlaneEnable:         "false",
					SvcEnable:                  "true",
					SvcElectionEnable:          "false",
					VipElectionEnable:          "false",
					EnableServiceSecurity:      "true",
					PerServiceElectionOnDemand: "true",
				}

				var err error
				svcElection, err = strconv.ParseBool(manifestValues.SvcElectionEnable)
				Expect(err).ToNot(HaveOccurred())

				ipFamily = []corev1.IPFamily{corev1.IPv6Protocol}

				tempDirPath = MustMkdirTemp(tempDirPathRoot, testDirPrefix)

				clusterName, client, _ = prepareCluster(ctx, tempDirPath, "rt-svc-ipv6-mixed", k8sImagePath, v129,
					kubeVIPRoutingTableManifestTemplate, logger, manifestValues, networking, nodesNumber, nil, dsNumber)
			})

			AfterAll(func() {
				SaveLogsAndCleanup(ctx, client, tempDirPath, clusterName, func() {
					cleanupCluster(clusterName, defaultNetwork, ConfigMtx, logger)
				})
			})

			DescribeTable("configures an IPv6 routes for services",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					lbAddresses := []string{}
					for range 2 {
						lbAddresses = append(lbAddresses, e2e.GenerateVIP(utils.IPv6Family, offset, defaultNetwork))
					}
					testServiceRTMixedElection(ctx, svcName, lbAddresses, "plndr-svcs-lock", "kube-system", fmt.Sprintf("kubevip-%s", svcName), dsNamespace,
						trafficPolicy, client, svcElection, ipFamily, 1, 1, dsNumber, false, clusterName)
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)
		})

		Describe("kube-vip DualStack services routing table mode functionality - IPv4 primary", Ordered, func() {
			var (
				cpVIP       string
				clusterName string
				client      kubernetes.Interface
				svcElection bool
				ipFamily    []corev1.IPFamily
				tempDirPath string

				nodesNumber = 1
			)

			BeforeAll(func() {
				cpVIP = e2e.GenerateDualStackVIP(SOffset.Get(), defaultNetwork)

				networking := &kindconfigv1alpha4.Networking{
					IPFamily: kindconfigv1alpha4.DualStackFamily,
				}

				manifestValues := &e2e.KubevipManifestValues{
					ControlPlaneVIP:       cpVIP,
					ImagePath:             imagePath,
					ConfigPath:            configPath,
					ControlPlaneEnable:    "false",
					SvcEnable:             "true",
					SvcElectionEnable:     "false",
					EnableEndpoints:       "false",
					EnableServiceSecurity: "true",
				}

				var err error
				svcElection, err = strconv.ParseBool(manifestValues.SvcElectionEnable)
				Expect(err).ToNot(HaveOccurred())

				ipFamily = []corev1.IPFamily{corev1.IPv4Protocol, corev1.IPv6Protocol}

				tempDirPath = MustMkdirTemp(tempDirPathRoot, testDirPrefix)

				clusterName, client, _ = prepareCluster(ctx, tempDirPath, "rt-ds-svc-ipv4", k8sImagePath, v129,
					kubeVIPRoutingTableManifestTemplate, logger, manifestValues, networking, nodesNumber, nil, dsNumber)
			})

			AfterAll(func() {
				SaveLogsAndCleanup(ctx, client, tempDirPath, clusterName, func() {
					cleanupCluster(clusterName, defaultNetwork, ConfigMtx, logger)
				})
			})

			DescribeTable("configures an IPv4 and IPv6 routes for services",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					lbAddress := e2e.GenerateDualStackVIP(offset, defaultNetwork)
					testServiceRT(ctx, svcName, lbAddress, "plndr-svcs-lock", "kube-system", clusterName,
						trafficPolicy, client, svcElection, ipFamily, 1, false, dsNumber, false)
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only removes route if it was referenced by multiple services and all of them were deleted",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					lbAddress := e2e.GenerateDualStackVIP(offset, defaultNetwork)
					testServiceRT(ctx, svcName, lbAddress, "plndr-svcs-lock", "kube-system", clusterName,
						trafficPolicy, client, svcElection, ipFamily, 2, false, dsNumber, false)
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)
		})

		Describe("kube-vip DualStack services routing table mode functionality with mixed election - IPv4 primary", Ordered, func() {
			var (
				cpVIP       string
				clusterName string
				client      kubernetes.Interface
				svcElection bool
				ipFamily    []corev1.IPFamily
				tempDirPath string

				nodesNumber = 1
			)

			BeforeAll(func() {
				cpVIP = e2e.GenerateDualStackVIP(SOffset.Get(), defaultNetwork)

				networking := &kindconfigv1alpha4.Networking{
					IPFamily: kindconfigv1alpha4.DualStackFamily,
				}

				manifestValues := &e2e.KubevipManifestValues{
					ControlPlaneVIP:            cpVIP,
					ImagePath:                  imagePath,
					ConfigPath:                 configPath,
					ControlPlaneEnable:         "false",
					SvcEnable:                  "true",
					SvcElectionEnable:          "false",
					VipElectionEnable:          "false",
					EnableServiceSecurity:      "true",
					PerServiceElectionOnDemand: "true",
				}

				var err error
				svcElection, err = strconv.ParseBool(manifestValues.SvcElectionEnable)
				Expect(err).ToNot(HaveOccurred())

				ipFamily = []corev1.IPFamily{corev1.IPv4Protocol, corev1.IPv6Protocol}

				tempDirPath = MustMkdirTemp(tempDirPathRoot, testDirPrefix)

				clusterName, client, _ = prepareCluster(ctx, tempDirPath, "rt-ds-svc-ipv4-mixed", k8sImagePath, v129,
					kubeVIPRoutingTableManifestTemplate, logger, manifestValues, networking, nodesNumber, nil, dsNumber)
			})

			AfterAll(func() {
				SaveLogsAndCleanup(ctx, client, tempDirPath, clusterName, func() {
					cleanupCluster(clusterName, defaultNetwork, ConfigMtx, logger)
				})
			})

			DescribeTable("configures an IPv4 and IPv6 routes for services",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					lbAddresses := []string{}
					for range 2 {
						lbAddresses = append(lbAddresses, e2e.GenerateDualStackVIP(offset, defaultNetwork))
					}
					testServiceRTMixedElection(ctx, svcName, lbAddresses, "plndr-svcs-lock", "kube-system", fmt.Sprintf("kubevip-%s", svcName), dsNamespace,
						trafficPolicy, client, svcElection, ipFamily, 1, 1, dsNumber, false, clusterName)
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)
		})

		Describe("kube-vip DualStack services routing table mode functionality - IPv6 primary", Ordered, func() {
			var (
				cpVIP       string
				clusterName string
				client      kubernetes.Interface
				svcElection bool
				ipFamily    []corev1.IPFamily
				tempDirPath string

				nodesNumber = 1
			)

			BeforeAll(func() {
				cpVIP = e2e.GenerateDualStackVIP(SOffset.Get(), defaultNetwork)

				networking := &kindconfigv1alpha4.Networking{
					IPFamily:      kindconfigv1alpha4.DualStackFamily,
					PodSubnet:     "fd00:10:244::/56,10.244.0.0/16",
					ServiceSubnet: "fd00:10:96::/112,10.96.0.0/16",
				}

				manifestValues := &e2e.KubevipManifestValues{
					ControlPlaneVIP:       cpVIP,
					ImagePath:             imagePath,
					ConfigPath:            configPath,
					ControlPlaneEnable:    "false",
					SvcEnable:             "true",
					SvcElectionEnable:     "false",
					EnableEndpoints:       "false",
					EnableServiceSecurity: "true",
				}

				var err error
				svcElection, err = strconv.ParseBool(manifestValues.SvcElectionEnable)
				Expect(err).ToNot(HaveOccurred())

				ipFamily = []corev1.IPFamily{corev1.IPv4Protocol, corev1.IPv6Protocol}

				tempDirPath = MustMkdirTemp(tempDirPathRoot, testDirPrefix)

				clusterName, client, _ = prepareCluster(ctx, tempDirPath, "rt-ds-svc-ipv6", k8sImagePath, v129,
					kubeVIPRoutingTableManifestTemplate, logger, manifestValues, networking, nodesNumber, nil, dsNumber)
			})

			AfterAll(func() {
				SaveLogsAndCleanup(ctx, client, tempDirPath, clusterName, func() {
					cleanupCluster(clusterName, defaultNetwork, ConfigMtx, logger)
				})
			})

			DescribeTable("configures an IPv4 and IPv6 routes for services",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					lbAddress := e2e.GenerateDualStackVIP(offset, defaultNetwork)
					testServiceRT(ctx, svcName, lbAddress, "plndr-svcs-lock", "kube-system", clusterName,
						trafficPolicy, client, svcElection, ipFamily, 1, false, dsNumber, false)
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only removes route if it was referenced by multiple services and all of them were deleted",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					lbAddress := e2e.GenerateDualStackVIP(offset, defaultNetwork)
					testServiceRT(ctx, svcName, lbAddress, "plndr-svcs-lock", "kube-system", clusterName,
						trafficPolicy, client, svcElection, ipFamily, 2, false, dsNumber, false)
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)
		})

		Describe("kube-vip DualStack services routing table mode functionality with mixed election - IPv6 primary", Ordered, func() {
			var (
				cpVIP       string
				clusterName string
				client      kubernetes.Interface
				svcElection bool
				ipFamily    []corev1.IPFamily
				tempDirPath string

				nodesNumber = 1
			)

			BeforeAll(func() {
				cpVIP = e2e.GenerateDualStackVIP(SOffset.Get(), defaultNetwork)

				networking := &kindconfigv1alpha4.Networking{
					IPFamily:      kindconfigv1alpha4.DualStackFamily,
					PodSubnet:     "fd00:10:244::/56,10.244.0.0/16",
					ServiceSubnet: "fd00:10:96::/112,10.96.0.0/16",
				}

				manifestValues := &e2e.KubevipManifestValues{
					ControlPlaneVIP:            cpVIP,
					ImagePath:                  imagePath,
					ConfigPath:                 configPath,
					ControlPlaneEnable:         "false",
					SvcEnable:                  "true",
					SvcElectionEnable:          "false",
					VipElectionEnable:          "false",
					EnableServiceSecurity:      "true",
					PerServiceElectionOnDemand: "true",
				}

				var err error
				svcElection, err = strconv.ParseBool(manifestValues.SvcElectionEnable)
				Expect(err).ToNot(HaveOccurred())

				ipFamily = []corev1.IPFamily{corev1.IPv6Protocol, corev1.IPv4Protocol}

				tempDirPath = MustMkdirTemp(tempDirPathRoot, testDirPrefix)

				clusterName, client, _ = prepareCluster(ctx, tempDirPath, "rt-ds-svc-ipv6-mixed", k8sImagePath, v129,
					kubeVIPRoutingTableManifestTemplate, logger, manifestValues, networking, nodesNumber, nil, dsNumber)
			})

			AfterAll(func() {
				SaveLogsAndCleanup(ctx, client, tempDirPath, clusterName, func() {
					cleanupCluster(clusterName, defaultNetwork, ConfigMtx, logger)
				})
			})

			DescribeTable("configures an IPv6 and IPv4 routes for services",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					lbAddresses := []string{}
					for range 2 {
						lbAddresses = append(lbAddresses, e2e.GenerateDualStackVIP(offset, defaultNetwork))
					}
					testServiceRTMixedElection(ctx, svcName, lbAddresses, "plndr-svcs-lock", "kube-system", fmt.Sprintf("kubevip-%s", svcName), dsNamespace,
						trafficPolicy, client, svcElection, ipFamily, 1, 1, dsNumber, false, clusterName)
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)
		})

		Describe("kube-vip IPv4 services routing table mode functionality with services election", Ordered, func() {
			var (
				cpVIP       string
				clusterName string
				client      kubernetes.Interface
				svcElection bool
				ipFamily    []corev1.IPFamily
				tempDirPath string

				nodesNumber = 1
			)

			BeforeAll(func() {
				cpVIP = e2e.GenerateVIP(utils.IPv4Family, SOffset.Get(), defaultNetwork)

				networking := &kindconfigv1alpha4.Networking{
					IPFamily: kindconfigv1alpha4.IPv4Family,
				}

				manifestValues := &e2e.KubevipManifestValues{
					ControlPlaneVIP:       cpVIP,
					ImagePath:             imagePath,
					ConfigPath:            configPath,
					ControlPlaneEnable:    "false",
					SvcEnable:             "true",
					SvcElectionEnable:     "true",
					EnableServiceSecurity: "true",
				}

				var err error
				svcElection, err = strconv.ParseBool(manifestValues.SvcElectionEnable)
				Expect(err).ToNot(HaveOccurred())

				ipFamily = []corev1.IPFamily{corev1.IPv4Protocol}

				tempDirPath = MustMkdirTemp(tempDirPathRoot, testDirPrefix)

				clusterName, client, _ = prepareCluster(ctx, tempDirPath, "rt-svc-ipv4", k8sImagePath, v129,
					kubeVIPRoutingTableManifestTemplate, logger, manifestValues, networking, nodesNumber, nil, dsNumber)
			})

			AfterAll(func() {
				SaveLogsAndCleanup(ctx, client, tempDirPath, clusterName, func() {
					cleanupCluster(clusterName, defaultNetwork, ConfigMtx, logger)
				})
			})

			DescribeTable("configures an IPv4 routes for services",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					lbAddress := e2e.GenerateVIP(utils.IPv4Family, offset, defaultNetwork)
					testServiceRT(ctx, svcName, lbAddress, fmt.Sprintf("kubevip-%s", svcName), dsNamespace, clusterName,
						trafficPolicy, client, svcElection, ipFamily, 1, false, dsNumber, false)
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only removes route if it was referenced by multiple services and all of them were deleted",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					lbAddress := e2e.GenerateVIP(utils.IPv4Family, offset, defaultNetwork)
					testServiceRT(ctx, svcName, lbAddress, fmt.Sprintf("kubevip-%s", svcName), dsNamespace, clusterName,
						trafficPolicy, client, svcElection, ipFamily, 2, false, dsNumber, false)
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only removes route if it was referenced by multiple services and all of them were deleted if common lease is used",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					lbAddress := e2e.GenerateVIP(utils.IPv4Family, offset, defaultNetwork)
					testServiceRT(ctx, svcName, lbAddress, fmt.Sprintf("kubevip-%s", svcName), dsNamespace, clusterName,
						trafficPolicy, client, svcElection, ipFamily, 2, true, dsNumber, false)
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
			)
		})

		Describe("kube-vip IPv6 services routing table mode functionality with services election", Ordered, func() {
			var (
				cpVIP       string
				clusterName string
				client      kubernetes.Interface
				svcElection bool
				ipFamily    []corev1.IPFamily
				tempDirPath string

				nodesNumber = 1
			)

			BeforeAll(func() {
				cpVIP = e2e.GenerateVIP(utils.IPv6Family, SOffset.Get(), defaultNetwork)

				networking := &kindconfigv1alpha4.Networking{
					IPFamily: kindconfigv1alpha4.IPv6Family,
				}

				manifestValues := &e2e.KubevipManifestValues{
					ControlPlaneVIP:       cpVIP,
					ImagePath:             imagePath,
					ConfigPath:            configPath,
					ControlPlaneEnable:    "false",
					SvcEnable:             "true",
					SvcElectionEnable:     "true",
					EnableServiceSecurity: "true",
				}

				var err error
				svcElection, err = strconv.ParseBool(manifestValues.SvcElectionEnable)
				Expect(err).ToNot(HaveOccurred())

				ipFamily = []corev1.IPFamily{corev1.IPv6Protocol}

				tempDirPath = MustMkdirTemp(tempDirPathRoot, testDirPrefix)

				clusterName, client, _ = prepareCluster(ctx, tempDirPath, "rt-svc-ipv6", k8sImagePath, v129,
					kubeVIPRoutingTableManifestTemplate, logger, manifestValues, networking, nodesNumber, nil, dsNumber)
			})

			AfterAll(func() {
				SaveLogsAndCleanup(ctx, client, tempDirPath, clusterName, func() {
					cleanupCluster(clusterName, defaultNetwork, ConfigMtx, logger)
				})
			})

			DescribeTable("configures an IPv6 routes for services",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					lbAddress := e2e.GenerateVIP(utils.IPv6Family, offset, defaultNetwork)
					testServiceRT(ctx, svcName, lbAddress, fmt.Sprintf("kubevip-%s", svcName), dsNamespace, clusterName,
						trafficPolicy, client, svcElection, ipFamily, 1, false, dsNumber, false)
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only removes route if it was referenced by multiple services and all of them were deleted",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					lbAddress := e2e.GenerateVIP(utils.IPv6Family, offset, defaultNetwork)
					testServiceRT(ctx, svcName, lbAddress, fmt.Sprintf("kubevip-%s", svcName), dsNamespace, clusterName,
						trafficPolicy, client, svcElection, ipFamily, 2, false, dsNumber, false)
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only removes route if it was referenced by multiple services and all of them were deleted when common lease is used",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					lbAddress := e2e.GenerateVIP(utils.IPv6Family, offset, defaultNetwork)
					testServiceRT(ctx, svcName, lbAddress, fmt.Sprintf("kubevip-%s", svcName), dsNamespace, clusterName,
						trafficPolicy, client, svcElection, ipFamily, 2, true, dsNumber, false)
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
			)
		})

		Describe("kube-vip DualStack services routing table mode functionality - IPv4 primary with services election", Ordered, func() {
			var (
				cpVIP       string
				clusterName string
				client      kubernetes.Interface
				svcElection bool
				ipFamily    []corev1.IPFamily
				tempDirPath string

				nodesNumber = 1
			)

			BeforeAll(func() {
				cpVIP = e2e.GenerateDualStackVIP(SOffset.Get(), defaultNetwork)

				networking := &kindconfigv1alpha4.Networking{
					IPFamily: kindconfigv1alpha4.DualStackFamily,
				}

				manifestValues := &e2e.KubevipManifestValues{
					ControlPlaneVIP:       cpVIP,
					ImagePath:             imagePath,
					ConfigPath:            configPath,
					ControlPlaneEnable:    "false",
					SvcEnable:             "true",
					SvcElectionEnable:     "true",
					EnableEndpoints:       "false",
					EnableServiceSecurity: "true",
				}

				var err error
				svcElection, err = strconv.ParseBool(manifestValues.SvcElectionEnable)
				Expect(err).ToNot(HaveOccurred())

				ipFamily = []corev1.IPFamily{corev1.IPv4Protocol, corev1.IPv6Protocol}

				tempDirPath = MustMkdirTemp(tempDirPathRoot, testDirPrefix)

				clusterName, client, _ = prepareCluster(ctx, tempDirPath, "rt-ds-svc-ipv4", k8sImagePath, v129,
					kubeVIPRoutingTableManifestTemplate, logger, manifestValues, networking, nodesNumber, nil, dsNumber)
			})

			AfterAll(func() {
				SaveLogsAndCleanup(ctx, client, tempDirPath, clusterName, func() {
					cleanupCluster(clusterName, defaultNetwork, ConfigMtx, logger)
				})
			})

			DescribeTable("configures an IPv4 and IPv6 routes for services",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					lbAddress := e2e.GenerateDualStackVIP(offset, defaultNetwork)
					testServiceRT(ctx, svcName, lbAddress, fmt.Sprintf("kubevip-%s", svcName), dsNamespace, clusterName,
						trafficPolicy, client, svcElection, ipFamily, 1, false, dsNumber, false)
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only removes route if it was referenced by multiple services and all of them were deleted",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					lbAddress := e2e.GenerateDualStackVIP(offset, defaultNetwork)
					testServiceRT(ctx, svcName, lbAddress, fmt.Sprintf("kubevip-%s", svcName), dsNamespace, clusterName,
						trafficPolicy, client, svcElection, ipFamily, 2, false, dsNumber, false)
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only removes route if it was referenced by multiple services and all of them were deleted when common lease is used",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					lbAddress := e2e.GenerateDualStackVIP(offset, defaultNetwork)
					testServiceRT(ctx, svcName, lbAddress, fmt.Sprintf("kubevip-%s", svcName), dsNamespace, clusterName,
						trafficPolicy, client, svcElection, ipFamily, 2, true, dsNumber, false)
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
			)
		})

		Describe("kube-vip DualStack services routing table mode functionality - IPv6 primary with services election", Ordered, func() {
			var (
				cpVIP       string
				clusterName string
				client      kubernetes.Interface
				svcElection bool
				ipFamily    []corev1.IPFamily
				tempDirPath string

				nodesNumber = 1
			)

			BeforeAll(func() {
				cpVIP = e2e.GenerateDualStackVIP(SOffset.Get(), defaultNetwork)

				networking := &kindconfigv1alpha4.Networking{
					IPFamily:      kindconfigv1alpha4.DualStackFamily,
					PodSubnet:     "fd00:10:244::/56,10.244.0.0/16",
					ServiceSubnet: "fd00:10:96::/112,10.96.0.0/16",
				}

				manifestValues := &e2e.KubevipManifestValues{
					ControlPlaneVIP:       cpVIP,
					ImagePath:             imagePath,
					ConfigPath:            configPath,
					ControlPlaneEnable:    "false",
					SvcEnable:             "true",
					SvcElectionEnable:     "true",
					EnableEndpoints:       "false",
					EnableServiceSecurity: "true",
				}

				var err error
				svcElection, err = strconv.ParseBool(manifestValues.SvcElectionEnable)
				Expect(err).ToNot(HaveOccurred())

				ipFamily = []corev1.IPFamily{corev1.IPv4Protocol, corev1.IPv6Protocol}

				tempDirPath = MustMkdirTemp(tempDirPathRoot, testDirPrefix)

				clusterName, client, _ = prepareCluster(ctx, tempDirPath, "rt-ds-svc-ipv6", k8sImagePath, v129,
					kubeVIPRoutingTableManifestTemplate, logger, manifestValues, networking, nodesNumber, nil, dsNumber)
			})

			AfterAll(func() {
				SaveLogsAndCleanup(ctx, client, tempDirPath, clusterName, func() {
					cleanupCluster(clusterName, defaultNetwork, ConfigMtx, logger)
				})
			})

			DescribeTable("configures an IPv4 and IPv6 routes for services",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					lbAddress := e2e.GenerateDualStackVIP(offset, defaultNetwork)
					testServiceRT(ctx, svcName, lbAddress, fmt.Sprintf("kubevip-%s", svcName), dsNamespace, clusterName,
						trafficPolicy, client, svcElection, ipFamily, 1, false, dsNumber, false)
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only removes route if it was referenced by multiple services and all of them were deleted",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					lbAddress := e2e.GenerateDualStackVIP(offset, defaultNetwork)
					testServiceRT(ctx, svcName, lbAddress, fmt.Sprintf("kubevip-%s", svcName), dsNamespace, clusterName,
						trafficPolicy, client, svcElection, ipFamily, 2, false, dsNumber, false)
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only removes route if it was referenced by multiple services and all of them were deleted when common lease is used",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					lbAddress := e2e.GenerateDualStackVIP(offset, defaultNetwork)
					testServiceRT(ctx, svcName, lbAddress, fmt.Sprintf("kubevip-%s", svcName), dsNamespace, clusterName,
						trafficPolicy, client, svcElection, ipFamily, 2, true, dsNumber, false)
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
			)
		})

		Describe("kube-vip IPv4 services routing table mode functionality with global election", Ordered, func() {
			var (
				cpVIP       string
				clusterName string
				client      kubernetes.Interface
				svcElection bool
				ipFamily    []corev1.IPFamily
				tempDirPath string

				nodesNumber = 1
			)

			BeforeAll(func() {
				cpVIP = e2e.GenerateVIP(utils.IPv4Family, SOffset.Get(), defaultNetwork)

				networking := &kindconfigv1alpha4.Networking{
					IPFamily: kindconfigv1alpha4.IPv4Family,
				}

				manifestValues := &e2e.KubevipManifestValues{
					ControlPlaneVIP:       cpVIP,
					ImagePath:             imagePath,
					ConfigPath:            configPath,
					ControlPlaneEnable:    "false",
					SvcEnable:             "true",
					VipElectionEnable:     "true",
					SvcElectionEnable:     "false",
					EnableServiceSecurity: "true",
				}

				var err error
				svcElection, err = strconv.ParseBool(manifestValues.SvcElectionEnable)
				Expect(err).ToNot(HaveOccurred())

				ipFamily = []corev1.IPFamily{corev1.IPv4Protocol}

				tempDirPath = MustMkdirTemp(tempDirPathRoot, testDirPrefix)

				clusterName, client, _ = prepareCluster(ctx, tempDirPath, "rt-svc-ipv4-global", k8sImagePath, v129,
					kubeVIPRoutingTableManifestTemplate, logger, manifestValues, networking, nodesNumber, nil, dsNumber)
			})

			AfterAll(func() {
				SaveLogsAndCleanup(ctx, client, tempDirPath, clusterName, func() {
					cleanupCluster(clusterName, defaultNetwork, ConfigMtx, logger)
				})
			})

			DescribeTable("configures an IPv4 routes for services",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					lbAddress := e2e.GenerateVIP(utils.IPv4Family, offset, defaultNetwork)
					testServiceRT(ctx, svcName, lbAddress, "plndr-svcs-lock", dsNamespace, clusterName,
						trafficPolicy, client, svcElection, ipFamily, 1, false, dsNumber, false)
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only removes route if it was referenced by multiple services and all of them were deleted",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					lbAddress := e2e.GenerateVIP(utils.IPv4Family, offset, defaultNetwork)
					testServiceRT(ctx, svcName, lbAddress, "plndr-svcs-lock", dsNamespace, clusterName,
						trafficPolicy, client, svcElection, ipFamily, 2, false, dsNumber, false)
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("removes route if all endpoint were deleted, and re-adds when endpoints are created",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					lbAddress := e2e.GenerateVIP(utils.IPv4Family, offset, defaultNetwork)
					testServiceRT(ctx, svcName, lbAddress, "plndr-svcs-lock", dsNamespace, clusterName,
						trafficPolicy, client, svcElection, ipFamily, 1, false, dsNumber, true)
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)
		})

		Describe("kube-vip IPv6 services routing table mode functionality with global election", Ordered, func() {
			var (
				cpVIP       string
				clusterName string
				client      kubernetes.Interface
				svcElection bool
				ipFamily    []corev1.IPFamily
				tempDirPath string

				nodesNumber = 1
			)

			BeforeAll(func() {
				cpVIP = e2e.GenerateVIP(utils.IPv6Family, SOffset.Get(), defaultNetwork)

				networking := &kindconfigv1alpha4.Networking{
					IPFamily: kindconfigv1alpha4.IPv6Family,
				}

				manifestValues := &e2e.KubevipManifestValues{
					ControlPlaneVIP:       cpVIP,
					ImagePath:             imagePath,
					ConfigPath:            configPath,
					ControlPlaneEnable:    "false",
					SvcEnable:             "true",
					SvcElectionEnable:     "false",
					VipElectionEnable:     "true",
					EnableServiceSecurity: "true",
				}

				var err error
				svcElection, err = strconv.ParseBool(manifestValues.SvcElectionEnable)
				Expect(err).ToNot(HaveOccurred())

				ipFamily = []corev1.IPFamily{corev1.IPv6Protocol}

				tempDirPath = MustMkdirTemp(tempDirPathRoot, testDirPrefix)

				clusterName, client, _ = prepareCluster(ctx, tempDirPath, "rt-svc-ipv6-global", k8sImagePath, v129,
					kubeVIPRoutingTableManifestTemplate, logger, manifestValues, networking, nodesNumber, nil, dsNumber)
			})

			AfterAll(func() {
				SaveLogsAndCleanup(ctx, client, tempDirPath, clusterName, func() {
					cleanupCluster(clusterName, defaultNetwork, ConfigMtx, logger)
				})
			})

			DescribeTable("configures an IPv6 routes for services",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					lbAddress := e2e.GenerateVIP(utils.IPv6Family, offset, defaultNetwork)
					testServiceRT(ctx, svcName, lbAddress, "plndr-svcs-lock", dsNamespace, clusterName,
						trafficPolicy, client, svcElection, ipFamily, 1, false, dsNumber, false)
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only removes route if it was referenced by multiple services and all of them were deleted",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					lbAddress := e2e.GenerateVIP(utils.IPv6Family, offset, defaultNetwork)
					testServiceRT(ctx, svcName, lbAddress, "plndr-svcs-lock", dsNamespace, clusterName,
						trafficPolicy, client, svcElection, ipFamily, 2, false, dsNumber, false)
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("removes route if all endpoint were deleted, and re-adds when endpoints are created",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					lbAddress := e2e.GenerateVIP(utils.IPv6Family, offset, defaultNetwork)
					testServiceRT(ctx, svcName, lbAddress, "plndr-svcs-lock", dsNamespace, clusterName,
						trafficPolicy, client, svcElection, ipFamily, 1, false, dsNumber, true)
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)
		})

		Describe("kube-vip DualStack services routing table mode functionality - IPv4 primary with global election", Ordered, func() {
			var (
				cpVIP       string
				clusterName string
				client      kubernetes.Interface
				svcElection bool
				ipFamily    []corev1.IPFamily
				tempDirPath string

				nodesNumber = 1
			)

			BeforeAll(func() {
				cpVIP = e2e.GenerateDualStackVIP(SOffset.Get(), defaultNetwork)

				networking := &kindconfigv1alpha4.Networking{
					IPFamily: kindconfigv1alpha4.DualStackFamily,
				}

				manifestValues := &e2e.KubevipManifestValues{
					ControlPlaneVIP:       cpVIP,
					ImagePath:             imagePath,
					ConfigPath:            configPath,
					ControlPlaneEnable:    "false",
					SvcEnable:             "true",
					SvcElectionEnable:     "false",
					VipElectionEnable:     "true",
					EnableEndpoints:       "false",
					EnableServiceSecurity: "true",
				}

				var err error
				svcElection, err = strconv.ParseBool(manifestValues.SvcElectionEnable)
				Expect(err).ToNot(HaveOccurred())

				ipFamily = []corev1.IPFamily{corev1.IPv4Protocol, corev1.IPv6Protocol}

				tempDirPath = MustMkdirTemp(tempDirPathRoot, testDirPrefix)

				clusterName, client, _ = prepareCluster(ctx, tempDirPath, "rt-ds-svc-ipv4-global", k8sImagePath, v129,
					kubeVIPRoutingTableManifestTemplate, logger, manifestValues, networking, nodesNumber, nil, dsNumber)
			})

			AfterAll(func() {
				SaveLogsAndCleanup(ctx, client, tempDirPath, clusterName, func() {
					cleanupCluster(clusterName, defaultNetwork, ConfigMtx, logger)
				})
			})

			DescribeTable("configures an IPv4 and IPv6 routes for services",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					lbAddress := e2e.GenerateDualStackVIP(offset, defaultNetwork)
					testServiceRT(ctx, svcName, lbAddress, "plndr-svcs-lock", dsNamespace, clusterName,
						trafficPolicy, client, svcElection, ipFamily, 1, false, dsNumber, false)
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only removes route if it was referenced by multiple services and all of them were deleted",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					lbAddress := e2e.GenerateDualStackVIP(offset, defaultNetwork)
					testServiceRT(ctx, svcName, lbAddress, "plndr-svcs-lock", dsNamespace, clusterName,
						trafficPolicy, client, svcElection, ipFamily, 2, false, dsNumber, false)
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("removes route if all endpoint were deleted, and re-adds when endpoints are created",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					lbAddress := e2e.GenerateDualStackVIP(offset, defaultNetwork)
					testServiceRT(ctx, svcName, lbAddress, "plndr-svcs-lock", dsNamespace, clusterName,
						trafficPolicy, client, svcElection, ipFamily, 1, false, dsNumber, true)
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)
		})

		Describe("kube-vip DualStack services routing table mode functionality - IPv6 primary with global election", Ordered, func() {
			var (
				cpVIP       string
				clusterName string
				client      kubernetes.Interface
				svcElection bool
				ipFamily    []corev1.IPFamily
				tempDirPath string

				nodesNumber = 1
			)

			BeforeAll(func() {
				cpVIP = e2e.GenerateDualStackVIP(SOffset.Get(), defaultNetwork)

				networking := &kindconfigv1alpha4.Networking{
					IPFamily: kindconfigv1alpha4.DualStackFamily,
				}

				manifestValues := &e2e.KubevipManifestValues{
					ControlPlaneVIP:       cpVIP,
					ImagePath:             imagePath,
					ConfigPath:            configPath,
					ControlPlaneEnable:    "false",
					SvcEnable:             "true",
					SvcElectionEnable:     "false",
					VipElectionEnable:     "true",
					EnableEndpoints:       "false",
					EnableServiceSecurity: "true",
				}

				var err error
				svcElection, err = strconv.ParseBool(manifestValues.SvcElectionEnable)
				Expect(err).ToNot(HaveOccurred())

				ipFamily = []corev1.IPFamily{corev1.IPv6Protocol, corev1.IPv4Protocol}

				tempDirPath = MustMkdirTemp(tempDirPathRoot, testDirPrefix)

				clusterName, client, _ = prepareCluster(ctx, tempDirPath, "rt-ds-svc-ipv6-global", k8sImagePath, v129,
					kubeVIPRoutingTableManifestTemplate, logger, manifestValues, networking, nodesNumber, nil, dsNumber)
			})

			AfterAll(func() {
				SaveLogsAndCleanup(ctx, client, tempDirPath, clusterName, func() {
					cleanupCluster(clusterName, defaultNetwork, ConfigMtx, logger)
				})
			})

			DescribeTable("configures an IPv4 and IPv6 routes for services",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					lbAddress := e2e.GenerateDualStackVIP(offset, defaultNetwork)
					testServiceRT(ctx, svcName, lbAddress, "plndr-svcs-lock", dsNamespace, clusterName,
						trafficPolicy, client, svcElection, ipFamily, 1, false, dsNumber, false)
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only removes route if it was referenced by multiple services and all of them were deleted",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					lbAddress := e2e.GenerateDualStackVIP(offset, defaultNetwork)
					testServiceRT(ctx, svcName, lbAddress, "plndr-svcs-lock", dsNamespace, clusterName,
						trafficPolicy, client, svcElection, ipFamily, 2, false, dsNumber, false)
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("removes route if all endpoint were deleted, and re-adds when endpoints are created",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					lbAddress := e2e.GenerateDualStackVIP(offset, defaultNetwork)
					testServiceRT(ctx, svcName, lbAddress, "plndr-svcs-lock", dsNamespace, clusterName,
						trafficPolicy, client, svcElection, ipFamily, 1, false, dsNumber, true)
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)
		})
	}
})

func testServiceRT(ctx context.Context, svcName, lbAddress, leaseName, leaseNamespace, clusterName string, trafficPolicy corev1.ServiceExternalTrafficPolicy,
	client kubernetes.Interface, serviceElection bool, ipFamily []corev1.IPFamily, numberOfServices int, commonLease bool, dsNumber int, deleteDS bool) {
	lbAddresses := vip.Split(lbAddress)

	services := []string{}
	for i := range numberOfServices {
		services = append(services, fmt.Sprintf("%s-%d", svcName, i))
	}

	for _, svc := range services {
		svcLease := ""
		if commonLease {
			svcLease = leaseName
		}
		createTestService(ctx, svc, dsNamespace, dsName, lbAddress,
			client, corev1.IPFamilyPolicyPreferDualStack, ipFamily, trafficPolicy, svcLease, dsNumber, false, false)
		time.Sleep(time.Second)
	}

	var container string
	if serviceElection {
		for _, svc := range services {
			lease := fmt.Sprintf("kubevip-%s", svc)
			if commonLease {
				lease = leaseName
			}
			By(withTimestamp(fmt.Sprintf("getting lease holder for lease '%s/%s'", leaseNamespace, lease)))
			container = e2e.GetLeaseHolder(ctx, lease, leaseNamespace, client)
			for _, addr := range lbAddresses {
				By(withTimestamp(fmt.Sprintf("checking route presence for address %q on container %q", addr, container)))
				e2e.CheckRoutePresence(addr, container, true)
			}
		}
	} else {
		container = fmt.Sprintf("%s-control-plane", clusterName)
		for _, addr := range lbAddresses {
			By(withTimestamp(fmt.Sprintf("checking route presence for address %q on container %q", addr, container)))
			e2e.CheckRoutePresence(addr, container, true)
		}
	}

	if deleteDS {
		removeTestDS(ctx, client, dsNamespace, dsName, dsNumber)
		time.Sleep(time.Second)

		container = fmt.Sprintf("%s-control-plane", clusterName)
		for _, addr := range lbAddresses {
			By(withTimestamp(fmt.Sprintf("checking route presence for address %q on container %q", addr, container)))
			e2e.CheckRoutePresence(addr, container, false)
		}

		createTestDS(ctx, client, dsNamespace, dsName, dsNumber)
		time.Sleep(time.Second)

		for _, addr := range lbAddresses {
			By(withTimestamp(fmt.Sprintf("checking route presence for address %q on container %q", addr, container)))
			e2e.CheckRoutePresence(addr, container, true)
		}
	}

	for i := range numberOfServices {
		By(withTimestamp(fmt.Sprintf("deleting service %s/%s\n", dsNamespace, services[i])))
		expected := i < numberOfServices-1
		err := client.CoreV1().Services(dsNamespace).Delete(ctx, services[i], metav1.DeleteOptions{})
		Expect(err).ToNot(HaveOccurred())
		time.Sleep(time.Second)

		for _, addr := range lbAddresses {
			By(withTimestamp(fmt.Sprintf("checking route presence for address %q on container %q - expected: %t", addr, container, expected)))
			e2e.CheckRoutePresence(addr, container, expected)
			if commonLease {
				By(withTimestamp(fmt.Sprintf("getting lease holder for lease '%s/%s' - expected: %t", leaseNamespace, leaseName, expected)))
				e2e.CheckLeasePresence(ctx, leaseName, leaseNamespace, client, expected)
			}
		}
	}
}

func testServiceRTMixedElection(ctx context.Context, svcName string, lbAddress []string, globalLeaseName, globalLeaseNamespace, leaseName, leaseNamespace string, trafficPolicy corev1.ServiceExternalTrafficPolicy,
	client kubernetes.Interface, serviceElection bool, ipFamily []corev1.IPFamily, numberOfGlobalServices, numberOfElectedServices int, dsNumber int, deleteDS bool, clusterName string) {
	services := []serviceWithLease{}
	for i := range numberOfGlobalServices {
		services = append(services, serviceWithLease{
			name:           fmt.Sprintf("%s-global-%d", svcName, i),
			lease:          globalLeaseName,
			leaseNamespace: globalLeaseNamespace,
			election:       false,
			lbAddress:      lbAddress[i],
		})
	}

	for i := range numberOfElectedServices {
		services = append(services, serviceWithLease{
			name:           fmt.Sprintf("%s-%d", svcName, i),
			lease:          fmt.Sprintf("%s-%d", leaseName, i),
			leaseNamespace: leaseNamespace,
			election:       true,
			lbAddress:      lbAddress[i+numberOfGlobalServices],
		})
	}

	for _, svc := range services {
		createTestService(ctx, svc.name, dsNamespace, dsName, svc.lbAddress,
			client, corev1.IPFamilyPolicyPreferDualStack, ipFamily, trafficPolicy, "", dsNumber, false, svc.election)
		time.Sleep(time.Second)
	}

	for _, svc := range services {
		container := fmt.Sprintf("%s-control-plane", clusterName)
		if svc.election {
			By(withTimestamp(fmt.Sprintf("getting lease holder for lease '%s/%s'", svc.leaseNamespace, svc.lease)))
			container = e2e.GetLeaseHolder(ctx, svc.lease, leaseNamespace, client)
		}

		for addr := range strings.SplitSeq(svc.lbAddress, ",") {
			By(withTimestamp(fmt.Sprintf("checking route presence for address %q on container %q", addr, container)))
			e2e.CheckRoutePresence(addr, container, true)
		}
	}

	for i, svc := range services {
		By(withTimestamp(fmt.Sprintf("deleting service %s/%s\n", dsNamespace, svc.name)))

		container := fmt.Sprintf("%s-control-plane", clusterName)
		if svc.election {
			container = e2e.GetLeaseHolder(ctx, svc.lease, svc.leaseNamespace, client)
		}

		err := client.CoreV1().Services(dsNamespace).Delete(ctx, svc.name, metav1.DeleteOptions{})
		Expect(err).ToNot(HaveOccurred())
		time.Sleep(time.Second)

		expected := i < len(services)-1
		for addr := range strings.SplitSeq(svc.lbAddress, ",") {
			By(withTimestamp(fmt.Sprintf("checking route presence for address %q on container %q - expected: %t", addr, container, expected)))
			e2e.CheckRoutePresence(addr, container, expected)
		}
	}
}

type san struct {
	ip    *net.IP
	ipnet *net.IPNet
}

// controlPlaneContainerName returns the kind docker container name for the
// i-th (1-based) control-plane node of the cluster.
func controlPlaneContainerName(clusterName string, i int) string {
	if i > 1 {
		return fmt.Sprintf("%s-control-plane%d", clusterName, i)
	}
	return fmt.Sprintf("%s-control-plane", clusterName)
}

// setAPIServerEnabled stops or starts the static apiserver pod on a control-plane
// container by moving its manifest in or out of the kubelet manifests directory.
func setAPIServerState(container string, enabled bool) {
	const (
		manifestPath = "/etc/kubernetes/manifests/kube-apiserver.yaml"
		stashPath    = "/tmp/kube-apiserver.yaml"
	)

	src, dst := manifestPath, stashPath
	if enabled {
		src, dst = stashPath, manifestPath
	}

	out := new(bytes.Buffer)
	cmd := exec.Command("docker", "exec", container, "mv", src, dst)
	cmd.Stdout = out
	cmd.Stderr = out
	Expect(cmd.Run()).To(Succeed(), out.String())
}
