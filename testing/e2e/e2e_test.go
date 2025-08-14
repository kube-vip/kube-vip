//go:build e2e
// +build e2e

package e2e_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"text/template"
	"time"

	v1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/klog/v2"
	"sigs.k8s.io/kind/pkg/apis/config/v1alpha4"
	kindconfigv1alpha4 "sigs.k8s.io/kind/pkg/apis/config/v1alpha4"
	"sigs.k8s.io/kind/pkg/cluster"
	"sigs.k8s.io/kind/pkg/log"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/format"
	"github.com/onsi/gomega/gexec"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/vip"
	"github.com/kube-vip/kube-vip/testing/e2e"
)

const (
	dsName      = "traefik-whoami"
	dsNamespace = "default"
)

const testJSON = `
- op: add
  path: "/apiServer/certSANs/-"
  value: `

var _ = Describe("kube-vip ARP/NDP broadcast neighbor", Ordered, func() {
	if Mode == ModeARP {
		var (
			logger                          log.Logger
			imagePath                       string
			k8sImagePath                    string
			configPath                      string
			kubeVIPManifestTemplate         *template.Template
			kubeVIPHostnameManifestTemplate *template.Template
			v129                            bool
			tempDirPathRoot                 string
		)

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

			templatePath := filepath.Join(curDir, "kube-vip.yaml.tmpl")
			kubeVIPManifestTemplate, err = template.New("kube-vip.yaml.tmpl").ParseFiles(templatePath)
			Expect(err).NotTo(HaveOccurred())

			hostnameTemplatePath := filepath.Join(curDir, "kube-vip-hostname.yaml.tmpl")
			kubeVIPHostnameManifestTemplate, err = template.New("kube-vip-hostname.yaml.tmpl").ParseFiles(hostnameTemplatePath)
			Expect(err).NotTo(HaveOccurred())
		})

		BeforeAll(func() {
			var err error
			tempDirPathRoot, err = os.MkdirTemp("", "kube-vip-test-arp")
			Expect(err).NotTo(HaveOccurred())

		})

		AfterAll(func() {
			if os.Getenv("E2E_KEEP_LOGS") != "true" {
				Expect(os.RemoveAll(tempDirPathRoot)).To(Succeed())
			}
		})

		Describe("kube-vip IPv4 functionality, vip_leaderelection=true", Ordered, func() {
			var (
				cpVIP       string
				client      kubernetes.Interface
				clusterName string
				tempDirPath string
			)

			BeforeAll(func() {
				cpVIP = e2e.GenerateVIP(e2e.IPv4Family, SOffset.Get())

				networking := &kindconfigv1alpha4.Networking{
					IPFamily: kindconfigv1alpha4.IPv4Family,
				}

				manifestValues := &e2e.KubevipManifestValues{
					ControlPlaneVIP:      cpVIP,
					ImagePath:            imagePath,
					ConfigPath:           configPath,
					SvcEnable:            "false",
					SvcElectionEnable:    "false",
					EnableEndpointslices: "false",
				}

				var err error
				tempDirPath, err = os.MkdirTemp(tempDirPathRoot, "kube-vip-test")
				Expect(err).NotTo(HaveOccurred())

				clusterName, client, _ = prepareCluster(tempDirPath, "ipv4", k8sImagePath, v129, kubeVIPManifestTemplate, logger, manifestValues, networking, 3, nil)
			})

			AfterAll(func() {
				Eventually(func() error {
					return e2e.GetLogs(context.Background(), client, tempDirPath)
				}, "60s", "5s").Should(Succeed())
				cleanupCluster(clusterName, ConfigMtx, logger)
			})

			It(clusterName+" provides an IPv4 VIP address for the Kubernetes control plane nodes", func() {
				testControlPlaneVIPs([]string{cpVIP}, clusterName, client)
			})
		})

		Describe("kube-vip IPv4 functionality, svc_enable=true, svc_election=false", Ordered, func() {
			var (
				cpVIP       string
				client      kubernetes.Interface
				clusterName string
				tempDirPath string
			)

			BeforeAll(func() {
				cpVIP = e2e.GenerateVIP(e2e.IPv4Family, SOffset.Get())

				networking := &kindconfigv1alpha4.Networking{
					IPFamily: kindconfigv1alpha4.IPv4Family,
				}

				manifestValues := &e2e.KubevipManifestValues{
					ControlPlaneVIP:      cpVIP,
					ImagePath:            imagePath,
					ConfigPath:           configPath,
					SvcEnable:            "true",
					SvcElectionEnable:    "false",
					EnableEndpointslices: "false",
				}

				var err error
				tempDirPath, err = os.MkdirTemp(tempDirPathRoot, "kube-vip-test")
				Expect(err).NotTo(HaveOccurred())

				clusterName, client, _ = prepareCluster(tempDirPath, "svc-ipv4", k8sImagePath, v129, kubeVIPManifestTemplate, logger, manifestValues, networking, 1, nil)
			})

			AfterAll(func() {
				Eventually(func() error {
					return e2e.GetLogs(context.Background(), client, tempDirPath)
				}, "60s", "5s").Should(Succeed())
				cleanupCluster(clusterName, ConfigMtx, logger)
			})

			DescribeTable("configures an IPv4 VIP address for service",
				func(svcName string, currentOffset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					lbAddress := e2e.GenerateVIP(e2e.IPv4Family, currentOffset)
					testService(svcName, lbAddress, "plndr-svcs-lock", "kube-system", trafficPolicy, client, false, []corev1.IPFamily{corev1.IPv4Protocol}, 1)
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only removes VIP address if it was referenced by multiple services and all of them were deleted",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					lbAddress := e2e.GenerateVIP(e2e.IPv4Family, offset)
					testService(svcName, lbAddress, "plndr-svcs-lock", "kube-system", trafficPolicy, client, false, []corev1.IPFamily{corev1.IPv4Protocol}, 2)
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)
		})

		Describe("kube-vip IPv4 functionality, svc_enable=true, svc_election=true", Ordered, func() {
			var (
				cpVIP       string
				client      kubernetes.Interface
				clusterName string
				tempDirPath string
			)

			BeforeAll(func() {
				cpVIP = e2e.GenerateVIP(e2e.IPv4Family, SOffset.Get())

				networking := &kindconfigv1alpha4.Networking{
					IPFamily: kindconfigv1alpha4.IPv4Family,
				}

				manifestValues := &e2e.KubevipManifestValues{
					ControlPlaneVIP:      cpVIP,
					ImagePath:            imagePath,
					ConfigPath:           configPath,
					SvcEnable:            "true",
					SvcElectionEnable:    "true",
					EnableEndpointslices: "false",
				}

				var err error
				tempDirPath, err = os.MkdirTemp(tempDirPathRoot, "kube-vip-test")
				Expect(err).NotTo(HaveOccurred())

				clusterName, client, _ = prepareCluster(tempDirPath, "svc-el-ipv4", k8sImagePath, v129, kubeVIPManifestTemplate, logger, manifestValues, networking, 1, nil)
			})

			AfterAll(func() {
				Eventually(func() error {
					return e2e.GetLogs(context.Background(), client, tempDirPath)
				}, "60s", "5s").Should(Succeed())
				cleanupCluster(clusterName, ConfigMtx, logger)
			})

			DescribeTable("configures an IPv4 VIP address for service",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					lbAddress := e2e.GenerateVIP(e2e.IPv4Family, offset)
					testService(svcName, lbAddress, fmt.Sprintf("kubevip-%s", svcName), dsNamespace, trafficPolicy, client, true, []corev1.IPFamily{corev1.IPv4Protocol}, 1)
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)
		})

		Describe("kube-vip IPv6 functionality, vip_leaderelection=true", Ordered, func() {
			var (
				cpVIP       string
				client      kubernetes.Interface
				clusterName string
				clusterCfg  *rest.Config
				tempDirPath string
			)

			BeforeAll(func() {
				cpVIP = e2e.GenerateVIP(e2e.IPv6Family, SOffset.Get())

				networking := &kindconfigv1alpha4.Networking{
					IPFamily: kindconfigv1alpha4.IPv6Family,
				}

				manifestValues := &e2e.KubevipManifestValues{
					ControlPlaneVIP:      cpVIP,
					ImagePath:            imagePath,
					ConfigPath:           configPath,
					SvcEnable:            "false",
					SvcElectionEnable:    "false",
					EnableEndpointslices: "false",
				}

				var err error
				tempDirPath, err = os.MkdirTemp(tempDirPathRoot, "kube-vip-test")
				Expect(err).NotTo(HaveOccurred())

				clusterName, client, clusterCfg = prepareCluster(tempDirPath, "ipv6", k8sImagePath, v129, kubeVIPManifestTemplate, logger, manifestValues, networking, 3, nil)
			})

			AfterAll(func() {
				c, err := kubernetes.NewForConfig(clusterCfg)
				Expect(err).ToNot(HaveOccurred())
				Eventually(func() error {
					return e2e.GetLogs(context.Background(), c, tempDirPath)
				}, "60s", "5s").Should(Succeed())
				cleanupCluster(clusterName, ConfigMtx, logger)
			})

			It(clusterName+"provides an IPv6 VIP address for the Kubernetes control plane nodes", func() {
				testControlPlaneVIPs([]string{cpVIP}, clusterName, client)
			})
		})

		Describe("kube-vip IPv6 functionality, svc_enable=true, svc_election=false", Ordered, func() {
			var (
				cpVIP       string
				client      kubernetes.Interface
				clusterName string
				tempDirPath string
			)

			BeforeAll(func() {
				cpVIP = e2e.GenerateVIP(e2e.IPv6Family, SOffset.Get())

				networking := &kindconfigv1alpha4.Networking{
					IPFamily: kindconfigv1alpha4.IPv6Family,
				}

				manifestValues := &e2e.KubevipManifestValues{
					ControlPlaneVIP:      cpVIP,
					ImagePath:            imagePath,
					ConfigPath:           configPath,
					SvcEnable:            "true",
					SvcElectionEnable:    "false",
					EnableEndpointslices: "false",
				}

				var err error
				tempDirPath, err = os.MkdirTemp(tempDirPathRoot, "kube-vip-test")
				Expect(err).NotTo(HaveOccurred())

				clusterName, client, _ = prepareCluster(tempDirPath, "svc-ipv6", k8sImagePath, v129, kubeVIPManifestTemplate, logger, manifestValues, networking, 1, nil)
			})

			AfterAll(func() {
				Eventually(func() error {
					return e2e.GetLogs(context.Background(), client, tempDirPath)
				}, "60s", "5s").Should(Succeed())
				cleanupCluster(clusterName, ConfigMtx, logger)
			})

			DescribeTable("configures an IPv6 VIP address for service",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					lbAddress := e2e.GenerateVIP(e2e.IPv6Family, offset)
					testService(svcName, lbAddress, "plndr-svcs-lock", "kube-system", trafficPolicy, client, false, []corev1.IPFamily{corev1.IPv6Protocol}, 1)
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only removes VIP address if it was referenced by multiple services and all of them were deleted",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					lbAddress := e2e.GenerateVIP(e2e.IPv6Family, offset)
					testService(svcName, lbAddress, "plndr-svcs-lock", "kube-system", trafficPolicy, client, false, []corev1.IPFamily{corev1.IPv6Protocol}, 2)
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)
		})

		Describe("kube-vip IPv6 functionality, svc_enable=true, svc_election=true", Ordered, func() {
			var (
				cpVIP       string
				client      kubernetes.Interface
				clusterName string
				tempDirPath string
			)

			BeforeAll(func() {
				cpVIP = e2e.GenerateVIP(e2e.IPv6Family, SOffset.Get())

				networking := &kindconfigv1alpha4.Networking{
					IPFamily: kindconfigv1alpha4.IPv6Family,
				}

				manifestValues := &e2e.KubevipManifestValues{
					ControlPlaneVIP:      cpVIP,
					ImagePath:            imagePath,
					ConfigPath:           configPath,
					SvcEnable:            "true",
					SvcElectionEnable:    "true",
					EnableEndpointslices: "false",
				}

				var err error
				tempDirPath, err = os.MkdirTemp(tempDirPathRoot, "kube-vip-test")
				Expect(err).NotTo(HaveOccurred())

				clusterName, client, _ = prepareCluster(tempDirPath, "svc-el-ipv6", k8sImagePath, v129, kubeVIPManifestTemplate, logger, manifestValues, networking, 1, nil)
			})

			AfterAll(func() {
				Eventually(func() error {
					return e2e.GetLogs(context.Background(), client, tempDirPath)
				}, "60s", "5s").Should(Succeed())
				cleanupCluster(clusterName, ConfigMtx, logger)
			})

			DescribeTable("configures an IPv6 VIP address for service",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					lbAddress := e2e.GenerateVIP(e2e.IPv6Family, offset)
					testService(svcName, lbAddress, fmt.Sprintf("kubevip-%s", svcName), dsNamespace, trafficPolicy, client, true, []corev1.IPFamily{corev1.IPv6Protocol}, 1)
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)
		})

		Describe("kube-vip DualStack functionality - IPv4 primary, vip_leaderelection=true", Ordered, func() {
			var (
				cpVIP       string
				client      kubernetes.Interface
				clusterName string
				tempDirPath string
			)

			BeforeAll(func() {
				cpVIP = e2e.GenerateDualStackVIP(23)

				networking := &kindconfigv1alpha4.Networking{
					IPFamily: kindconfigv1alpha4.DualStackFamily,
				}

				manifestValues := &e2e.KubevipManifestValues{
					ControlPlaneVIP:      cpVIP,
					ImagePath:            imagePath,
					ConfigPath:           configPath,
					SvcEnable:            "false",
					SvcElectionEnable:    "false",
					EnableEndpointslices: "true",
				}

				var err error
				tempDirPath, err = os.MkdirTemp(tempDirPathRoot, "kube-vip-test")
				Expect(err).NotTo(HaveOccurred())

				clusterName, client, _ = prepareCluster(tempDirPath, "ds-ipv4", k8sImagePath, v129, kubeVIPManifestTemplate, logger, manifestValues, networking, 3, nil)
			})

			AfterAll(func() {
				Eventually(func() error {
					return e2e.GetLogs(context.Background(), client, tempDirPath)
				}, "60s", "5s").Should(Succeed())
				cleanupCluster(clusterName, ConfigMtx, logger)
			})

			It(clusterName+" provides a DualStack VIP addresses for the Kubernetes control plane nodes", func() {
				testControlPlaneVIPs(vip.Split(cpVIP), clusterName, client)
			})
		})

		Describe("kube-vip DualStack functionality - IPv4 primary, svc_enable=true, svc_election=false", Ordered, func() {
			var (
				cpVIP       string
				client      kubernetes.Interface
				clusterName string
				tempDirPath string
			)

			BeforeAll(func() {
				cpVIP = e2e.GenerateDualStackVIP(24)

				networking := &kindconfigv1alpha4.Networking{
					IPFamily: kindconfigv1alpha4.DualStackFamily,
				}

				manifestValues := &e2e.KubevipManifestValues{
					ControlPlaneVIP:      cpVIP,
					ImagePath:            imagePath,
					ConfigPath:           configPath,
					SvcEnable:            "true",
					SvcElectionEnable:    "false",
					EnableEndpointslices: "true",
				}

				var err error
				tempDirPath, err = os.MkdirTemp(tempDirPathRoot, "kube-vip-test")
				Expect(err).NotTo(HaveOccurred())

				clusterName, client, _ = prepareCluster(tempDirPath, "ds-svc-ipv4", k8sImagePath, v129, kubeVIPManifestTemplate, logger, manifestValues, networking, 1, nil)
			})

			AfterAll(func() {
				By(fmt.Sprintf("saving logs to %q", tempDirPath))
				Eventually(func() error {
					return e2e.GetLogs(context.Background(), client, tempDirPath)
				}, "60s", "5s").Should(Succeed())
				cleanupCluster(clusterName, ConfigMtx, logger)
			})

			DescribeTable("configures an IPv4 and IPv6 VIP addresses for service",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					lbAddress := e2e.GenerateDualStackVIP(offset)
					testService(svcName, lbAddress, "plndr-svcs-lock", "kube-system", trafficPolicy, client, false, []corev1.IPFamily{corev1.IPv4Protocol, corev1.IPv6Protocol}, 1)
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only removes VIP address if it was referenced by multiple services and all of them were deleted",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					lbAddress := e2e.GenerateDualStackVIP(offset)
					testService(svcName, lbAddress, "plndr-svcs-lock", "kube-system", trafficPolicy, client, false, []corev1.IPFamily{corev1.IPv4Protocol, corev1.IPv6Protocol}, 2)
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)
		})

		Describe("kube-vip DualStack functionality - IPv4 primary, svc_enable=true, svc_election=true", Ordered, func() {
			var (
				cpVIP       string
				client      kubernetes.Interface
				clusterName string
				tempDirPath string
			)

			BeforeAll(func() {
				cpVIP = e2e.GenerateDualStackVIP(29)

				networking := &kindconfigv1alpha4.Networking{
					IPFamily: kindconfigv1alpha4.DualStackFamily,
				}

				manifestValues := &e2e.KubevipManifestValues{
					ControlPlaneVIP:      cpVIP,
					ImagePath:            imagePath,
					ConfigPath:           configPath,
					SvcEnable:            "true",
					SvcElectionEnable:    "true",
					EnableEndpointslices: "true",
				}

				var err error
				tempDirPath, err = os.MkdirTemp(tempDirPathRoot, "kube-vip-test")
				Expect(err).NotTo(HaveOccurred())

				clusterName, client, _ = prepareCluster(tempDirPath, "ds-svc-el-ipv4", k8sImagePath, v129, kubeVIPManifestTemplate, logger, manifestValues, networking, 1, nil)
			})

			AfterAll(func() {
				By(fmt.Sprintf("saving logs to %q", tempDirPath))
				Eventually(func() error {
					return e2e.GetLogs(context.Background(), client, tempDirPath)
				}, "60s", "5s").Should(Succeed())
				cleanupCluster(clusterName, ConfigMtx, logger)
			})

			DescribeTable("configures an IPv4 and IPv6 VIP addresses for service",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					lbAddress := e2e.GenerateDualStackVIP(offset)
					testService(svcName, lbAddress, fmt.Sprintf("kubevip-%s", svcName), dsNamespace, trafficPolicy, client, true, []corev1.IPFamily{corev1.IPv4Protocol, corev1.IPv6Protocol}, 1)
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)
		})

		Describe("kube-vip DualStack functionality - IPv6 primary, vip_leaderelection=true", Ordered, func() {
			var (
				cpVIP       string
				client      kubernetes.Interface
				clusterName string
				tempDirPath string
			)

			BeforeAll(func() {
				cpVIP = e2e.GenerateDualStackVIP(32)

				networking := &kindconfigv1alpha4.Networking{
					IPFamily:      kindconfigv1alpha4.DualStackFamily,
					PodSubnet:     "fd00:10:244::/56,10.244.0.0/16",
					ServiceSubnet: "fd00:10:96::/112,10.96.0.0/16",
				}

				manifestValues := &e2e.KubevipManifestValues{
					ControlPlaneVIP:      cpVIP,
					ImagePath:            imagePath,
					ConfigPath:           configPath,
					SvcEnable:            "true",
					SvcElectionEnable:    "false",
					EnableEndpointslices: "true",
				}

				var err error
				tempDirPath, err = os.MkdirTemp(tempDirPathRoot, "kube-vip-test")
				Expect(err).NotTo(HaveOccurred())

				clusterName, client, _ = prepareCluster(tempDirPath, "ds-ipv6", k8sImagePath, v129, kubeVIPManifestTemplate, logger, manifestValues, networking, 3, nil)
			})

			AfterAll(func() {
				By(fmt.Sprintf("saving logs to %q", tempDirPath))
				Eventually(func() error {
					return e2e.GetLogs(context.Background(), client, tempDirPath)
				}, "60s", "5s").Should(Succeed())
				cleanupCluster(clusterName, ConfigMtx, logger)
			})

			It(clusterName+" provides a DualStack VIP addresses for the Kubernetes control plane nodes", func() {
				testControlPlaneVIPs(vip.Split(cpVIP), clusterName, client)
			})
		})

		Describe("kube-vip DualStack functionality - IPv6 primary, svc_enable=true, svc_election=false", Ordered, func() {
			var (
				cpVIP       string
				client      kubernetes.Interface
				clusterName string
				tempDirPath string
			)

			BeforeAll(func() {
				cpVIP = e2e.GenerateDualStackVIP(33)

				networking := &kindconfigv1alpha4.Networking{
					IPFamily:      kindconfigv1alpha4.DualStackFamily,
					PodSubnet:     "fd00:10:244::/56,10.244.0.0/16",
					ServiceSubnet: "fd00:10:96::/112,10.96.0.0/16",
				}

				manifestValues := &e2e.KubevipManifestValues{
					ControlPlaneVIP:      cpVIP,
					ImagePath:            imagePath,
					ConfigPath:           configPath,
					SvcEnable:            "true",
					SvcElectionEnable:    "false",
					EnableEndpointslices: "true",
				}

				var err error
				tempDirPath, err = os.MkdirTemp(tempDirPathRoot, "kube-vip-test")
				Expect(err).NotTo(HaveOccurred())

				clusterName, client, _ = prepareCluster(tempDirPath, "ds-svc-ipv6", k8sImagePath, v129, kubeVIPManifestTemplate, logger, manifestValues, networking, 1, nil)
			})

			AfterAll(func() {
				By(fmt.Sprintf("saving logs to %q", tempDirPath))
				Eventually(func() error {
					return e2e.GetLogs(context.Background(), client, tempDirPath)
				}, "60s", "5s").Should(Succeed())
				cleanupCluster(clusterName, ConfigMtx, logger)
			})

			AfterEach(func() {
				Eventually(func() error {
					return e2e.GetLogs(context.Background(), client, tempDirPath)
				}, "60s", "5s").Should(Succeed())
			})

			DescribeTable("configures an IPv4 and IPv6 VIP addresses for service",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					lbAddress := e2e.GenerateDualStackVIP(offset)
					testService(svcName, lbAddress, "plndr-svcs-lock", "kube-system", trafficPolicy, client, false, []corev1.IPFamily{corev1.IPv6Protocol, corev1.IPv4Protocol}, 1)
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only removes VIP address if it was referenced by multiple services and all of them were deleted",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					lbAddress := e2e.GenerateDualStackVIP(offset)
					testService(svcName, lbAddress, "plndr-svcs-lock", "kube-system", trafficPolicy, client, false, []corev1.IPFamily{corev1.IPv6Protocol, corev1.IPv4Protocol}, 2)
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)
		})

		Describe("kube-vip DualStack functionality - IPv6 primary, svc_enable=true, svc_election=true", Ordered, func() {
			var (
				cpVIP       string
				client      kubernetes.Interface
				clusterName string
				tempDirPath string
			)

			BeforeAll(func() {
				cpVIP = e2e.GenerateDualStackVIP(38)

				networking := &kindconfigv1alpha4.Networking{
					IPFamily:      kindconfigv1alpha4.DualStackFamily,
					PodSubnet:     "fd00:10:244::/56,10.244.0.0/16",
					ServiceSubnet: "fd00:10:96::/112,10.96.0.0/16",
				}

				manifestValues := &e2e.KubevipManifestValues{
					ControlPlaneVIP:      cpVIP,
					ImagePath:            imagePath,
					ConfigPath:           configPath,
					SvcEnable:            "true",
					SvcElectionEnable:    "true",
					EnableEndpointslices: "true",
				}

				var err error
				tempDirPath, err = os.MkdirTemp(tempDirPathRoot, "kube-vip-test")
				Expect(err).NotTo(HaveOccurred())

				clusterName, client, _ = prepareCluster(tempDirPath, "ds-svc-el-ipv6", k8sImagePath, v129, kubeVIPManifestTemplate, logger, manifestValues, networking, 1, nil)
			})

			AfterAll(func() {
				By(fmt.Sprintf("saving logs to %q", tempDirPath))
				Eventually(func() error {
					return e2e.GetLogs(context.Background(), client, tempDirPath)
				}, "60s", "5s").Should(Succeed())
				cleanupCluster(clusterName, ConfigMtx, logger)
			})

			DescribeTable("configures an IPv6 VIP address for service",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					lbAddress := e2e.GenerateDualStackVIP(offset)
					testService(svcName, lbAddress, fmt.Sprintf("kubevip-%s", svcName), dsNamespace, trafficPolicy, client, true, []corev1.IPFamily{corev1.IPv6Protocol, corev1.IPv4Protocol}, 1)
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)
		})

		Describe("kube-vip IPv4 functionality with legacy hostname", Ordered, func() {
			var (
				cpVIP       string
				client      kubernetes.Interface
				clusterName string
				tempDirPath string
			)

			BeforeAll(func() {
				cpVIP = e2e.GenerateVIP(e2e.IPv4Family, SOffset.Get())

				networking := &kindconfigv1alpha4.Networking{
					IPFamily: kindconfigv1alpha4.IPv4Family,
				}

				manifestValues := &e2e.KubevipManifestValues{
					ControlPlaneVIP:      cpVIP,
					ImagePath:            imagePath,
					ConfigPath:           configPath,
					SvcEnable:            "false",
					SvcElectionEnable:    "false",
					EnableEndpointslices: "false",
				}

				var err error
				tempDirPath, err = os.MkdirTemp(tempDirPathRoot, "kube-vip-test")
				Expect(err).NotTo(HaveOccurred())

				clusterName, client, _ = prepareCluster(tempDirPath, "ipv4-hostname", k8sImagePath, v129, kubeVIPHostnameManifestTemplate, logger, manifestValues, networking, 3, nil)
			})

			AfterAll(func() {
				By(fmt.Sprintf("saving logs to %q", tempDirPath))
				Eventually(func() error {
					return e2e.GetLogs(context.Background(), client, tempDirPath)
				}, "60s", "5s").Should(Succeed())
				cleanupCluster(clusterName, ConfigMtx, logger)
			})

			It(clusterName+" uses hostname fallback while providing an IPv4 VIP address for the Kubernetes control plane nodes", func() {
				testControlPlaneVIPs([]string{cpVIP}, clusterName, client)
			})
		})
	}
})

func createKindCluster(logger log.Logger, config *v1alpha4.Cluster, clusterName string) (kubernetes.Interface, *rest.Config) {
	provider := cluster.NewProvider(
		cluster.ProviderWithLogger(logger),
		cluster.ProviderWithDocker(),
	)
	format.UseStringerRepresentation = true // Otherwise error stacks have binary format.
	Expect(provider.Create(
		clusterName,
		cluster.CreateWithV1Alpha4Config(config),
		cluster.CreateWithRetain(os.Getenv("E2E_PRESERVE_CLUSTER") == "true"), // If create fails, we'll need the cluster alive to debug
	)).To(Succeed())

	kc, err := provider.KubeConfig(clusterName, false)
	Expect(err).ToNot(HaveOccurred())

	kubeconfigGetter := func() (*api.Config, error) {
		return clientcmd.Load([]byte(kc))
	}

	cfg, err := clientcmd.BuildConfigFromKubeconfigGetter("", kubeconfigGetter)
	if err != nil {
		panic(err.Error())
	}

	client, err := kubernetes.NewForConfig(cfg)
	Expect(err).ToNot(HaveOccurred())

	return client, cfg
}

// Assume the VIP is routable if status code is 200 or 500. Since etcd might glitch.
func assertControlPlaneIsRoutable(controlPlaneVIP string, transportTimeout, eventuallyTimeout time.Duration) {
	assertConnection("https", controlPlaneVIP, "6443", "livez", transportTimeout, eventuallyTimeout)
}

// Assume connection to the provided address is possible
func assertConnection(protocol, ip, port, suffix string, transportTimeout, eventuallyTimeout time.Duration) {
	if strings.Contains(ip, ":") {
		ip = fmt.Sprintf("[%s]", ip)
	}

	By(withTimestamp(fmt.Sprintf("checking connection to %s://%s:%s/%s", protocol, ip, port, suffix)))

	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // nolint
	}
	client := &http.Client{Transport: transport, Timeout: transportTimeout}

	Eventually(func() int {
		resp, _ := client.Get(fmt.Sprintf("%s://%s:%s/%s", protocol, ip, port, suffix))
		if resp == nil {
			return -1
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}, eventuallyTimeout).Should(BeElementOf([]int{http.StatusOK, http.StatusInternalServerError}), fmt.Sprintf("Failed to connect to %s", ip))

	By(withTimestamp(fmt.Sprintf("estabilished connection to %s://%s:%s/%s", protocol, ip, port, suffix)))
}

// Assume connection to the provided address is possible
func assertConnectionError(protocol, ip, port, suffix string, transportTimeout time.Duration) {
	if strings.Contains(ip, ":") {
		ip = fmt.Sprintf("[%s]", ip)
	}

	By(withTimestamp(fmt.Sprintf("checking connection error to %s://%s:%s/%s", protocol, ip, port, suffix)))

	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // nolint
	}
	client := &http.Client{Transport: transport, Timeout: transportTimeout}

	Eventually(func() error {
		_, err := client.Get(fmt.Sprintf("%s://%s:%s/%s", protocol, ip, port, suffix))
		return err
	}, time.Second*30).Should(HaveOccurred())

	By(withTimestamp(fmt.Sprintf("connection %s://%s:%s/%s error as expected", protocol, ip, port, suffix)))
}

func killLeader(leaderName string, clusterName string) {
	cmd := exec.Command(
		"docker", "kill", leaderName,
	)

	session, err := gexec.Start(cmd, GinkgoWriter, GinkgoWriter)
	Expect(err).NotTo(HaveOccurred())
	Eventually(session, "5s").Should(gexec.Exit(0))
}

func findLeader(leaderIPAddr string, clusterName string) string {
	dockerControlPlaneContainerNames := []string{
		fmt.Sprintf("%s-control-plane", clusterName),
		fmt.Sprintf("%s-control-plane2", clusterName),
		fmt.Sprintf("%s-control-plane3", clusterName),
	}
	var leaderName string
	for _, name := range dockerControlPlaneContainerNames {
		cmdOut := new(bytes.Buffer)
		cmd := exec.Command(
			"docker", "exec", name, "ip", "addr",
		)
		cmd.Stdout = cmdOut
		Eventually(cmd.Run(), "5s").Should(Succeed())

		if strings.Contains(cmdOut.String(), leaderIPAddr) {
			leaderName = name
			break
		}
	}
	return leaderName
}

func withTimestamp(text string) string {
	return fmt.Sprintf("%s: %s", time.Now(), text)
}

func createTestDS(name, namespace string, client kubernetes.Interface) {
	labels := make(map[string]string)
	labels["app"] = name
	d := v1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: v1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					Tolerations: []corev1.Toleration{
						{
							Key:      "node-role.kubernetes.io/control-plane",
							Operator: corev1.TolerationOpExists,
							Effect:   corev1.TaintEffectNoSchedule,
						},
						{
							Key:      "node-role.kubernetes.io/master",
							Operator: corev1.TolerationOpExists,
							Effect:   corev1.TaintEffectNoSchedule,
						},
					},
					Containers: []corev1.Container{
						{
							Name:  fmt.Sprintf("%s-v4", name),
							Image: "ghcr.io/traefik/whoami:v1.11",
							Ports: []corev1.ContainerPort{
								{
									ContainerPort: 80,
								},
							},
							Env: []corev1.EnvVar{
								{
									Name:  "PORT",
									Value: "80",
								},
							},
						},
					},
				},
			},
		},
	}

	_, err := client.AppsV1().DaemonSets(namespace).Create(context.TODO(), &d, metav1.CreateOptions{})
	Expect(err).ToNot(HaveOccurred())
}

func createTestService(name, namespace, target, lbAddress string, client kubernetes.Interface, ipfPolicy corev1.IPFamilyPolicy, ipFamiles []corev1.IPFamily, externalPolicy corev1.ServiceExternalTrafficPolicy) {
	svcAnnotations := make(map[string]string)
	svcAnnotations[kubevip.LoadbalancerIPAnnotation] = lbAddress

	labels := make(map[string]string)
	labels["app"] = target

	By(withTimestamp(fmt.Sprintf("creating service %s/%s", namespace, name)))

	s := corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Labels:      labels,
			Annotations: svcAnnotations,
		},
		Spec: corev1.ServiceSpec{
			IPFamilies:            ipFamiles,
			IPFamilyPolicy:        &ipfPolicy,
			Type:                  corev1.ServiceTypeLoadBalancer,
			ExternalTrafficPolicy: externalPolicy,
			Ports: []corev1.ServicePort{
				{
					Protocol: corev1.ProtocolTCP,
					Port:     80,
				},
			},
			Selector: labels,
		},
	}

	Eventually(func() error {
		_, err := client.CoreV1().Services(namespace).Create(context.TODO(), &s, metav1.CreateOptions{})
		return err
	}, time.Second*60, time.Second).Should(Succeed())
	By(withTimestamp(fmt.Sprintf("service %s/%s created", namespace, name)))
	svcs, err := client.CoreV1().Services(namespace).List(context.Background(), metav1.ListOptions{})
	Expect(err).ToNot(HaveOccurred())
	By(svcs.String())

	Eventually(func() error {
		By(withTimestamp(fmt.Sprintf("getting service %s/%s\n", namespace, name)))
		_, err := client.CoreV1().Services(namespace).Get(context.Background(), name, metav1.GetOptions{})
		return err
	}, time.Second*60, time.Second).Should(Succeed())
}

func checkIPAddress(lbAddress, container string, expected bool) bool {
	By(withTimestamp(fmt.Sprintf("checking LB %q, should exist: %t", lbAddress, expected)))
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	to := time.NewTimer(time.Second * 180)
	defer to.Stop()
	for {
		select {
		case <-to.C:
			return false
		case <-ticker.C:
			if e2e.CheckIPAddressPresence(lbAddress, container, expected) {
				return true
			}
		}
	}
}

func checkIPAddressByLease(name, namespace, lbAddress string, expected bool, client kubernetes.Interface) bool {
	By(withTimestamp(fmt.Sprintf("checking LB %q by lease %s/%s, should exist: %t", lbAddress, namespace, name, expected)))
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	to := time.NewTimer(time.Second * 180)
	defer to.Stop()
	for {
		select {
		case <-to.C:
			return false
		case <-ticker.C:
			if e2e.CheckIPAddressPresenceByLease(name, namespace, lbAddress, client, expected) {
				return true
			}
		}
	}
}

func prepareCluster(tempDirPath, clusterNameSuffix, k8sImagePath string,
	v129 bool, kubeVIPManifestTemplate *template.Template, logger log.Logger,
	manifestValues *e2e.KubevipManifestValues, networking *kindconfigv1alpha4.Networking, nodesNum int,
	addSAN *san) (string, kubernetes.Interface, *rest.Config) {

	manifestPath := filepath.Join(tempDirPath, fmt.Sprintf("kube-vip-%s.yaml", clusterNameSuffix))

	manifestFile, err := os.Create(manifestPath)
	Expect(err).NotTo(HaveOccurred())

	defer manifestFile.Close()

	clusterConfig := kindconfigv1alpha4.Cluster{
		Networking: *networking,
		Nodes:      []kindconfigv1alpha4.Node{},
	}

	kubeadmPatches := []kindconfigv1alpha4.PatchJSON6902{}

	if addSAN != nil {
		for i := 0; i < 64; i++ {
			(*addSAN.ip)[len(*addSAN.ip)-1]++
			if addSAN.ipnet.Contains(*addSAN.ip) {
				kubeadmPatches = append(kubeadmPatches, kindconfigv1alpha4.PatchJSON6902{
					Group:   "kubeadm.k8s.io",
					Version: "v1beta3",
					Kind:    "ClusterConfiguration",
					Patch:   testJSON + addSAN.ip.String(),
				})
			}
		}
	}

	clusterConfig.KubeadmConfigPatchesJSON6902 = kubeadmPatches

	for range nodesNum {
		nodeConfig := kindconfigv1alpha4.Node{
			Role: kindconfigv1alpha4.ControlPlaneRole,
			ExtraMounts: []kindconfigv1alpha4.Mount{
				{
					HostPath:      manifestPath,
					ContainerPath: "/etc/kubernetes/manifests/kube-vip.yaml",
				},
			},
		}
		// Override the kind image version
		if k8sImagePath != "" {
			nodeConfig.Image = k8sImagePath
		}
		clusterConfig.Nodes = append(clusterConfig.Nodes, nodeConfig)
	}

	Expect(kubeVIPManifestTemplate.Execute(manifestFile, *manifestValues)).To(Succeed())

	if v129 {
		// create a seperate manifest
		manifestPath2 := filepath.Join(tempDirPath, fmt.Sprintf("kube-vip-%s-first.yaml", clusterNameSuffix))

		// change the path of the mount to the new file
		clusterConfig.Nodes[0].ExtraMounts[0].HostPath = manifestPath2

		manifestFile2, err := os.Create(manifestPath2)
		Expect(err).NotTo(HaveOccurred())

		defer manifestFile2.Close()

		manifestValues.ConfigPath = "/etc/kubernetes/super-admin.conf"

		Expect(kubeVIPManifestTemplate.Execute(manifestFile2, manifestValues)).To(Succeed())
	}

	clusterName := fmt.Sprintf("%s-%s", filepath.Base(tempDirPath), clusterNameSuffix)

	By(withTimestamp("creating a kind cluster with multiple control plane nodes"))
	client, cfg := createKindCluster(logger, &clusterConfig, clusterName)

	By(withTimestamp("creating test daemonset"))
	createTestDS(dsName, dsNamespace, client)

	By(withTimestamp("loading local docker image to kind cluster"))
	e2e.LoadDockerImageToKind(logger, manifestValues.ImagePath, clusterName)

	return clusterName, client, cfg
}

func cleanupCluster(clusterName string, configMtx *sync.Mutex, logger log.Logger) {
	if os.Getenv("E2E_PRESERVE_CLUSTER") == "true" {
		return
	}

	provider := cluster.NewProvider(
		cluster.ProviderWithLogger(logger),
		cluster.ProviderWithDocker(),
	)

	By(withTimestamp(fmt.Sprintf("deleting cluster: %s", clusterName)))

	Eventually(func() error {
		configMtx.Lock()
		defer configMtx.Unlock()
		return provider.Delete(clusterName, "")
	}, "60s", "200ms").Should(Succeed())
}

func testControlPlaneVIPs(cpVIPs []string, clusterName string, client kubernetes.Interface) {
	Expect(cpVIPs).ToNot(BeEmpty())

	By(withTimestamp("checking that the Kubernetes control plane nodes are accessible via the assigned VIP"))
	// Allow enough time for control plane nodes to load the docker image and
	// use the default timeout for establishing a connection to the VIP
	for _, cpVIP := range cpVIPs {
		By(withTimestamp(fmt.Sprintf("testing connection to VIP: %s", cpVIP)))
		assertControlPlaneIsRoutable(cpVIP, time.Duration(0), 20*time.Second)
	}

	var leaderName string
	Eventually(func() string {
		leaderName = findLeader(cpVIPs[0], clusterName)
		return leaderName
	}, "600s").ShouldNot(BeEmpty())

	Eventually(client.CoreV1().Nodes().Delete(context.Background(), leaderName, metav1.DeleteOptions{}), "60s", "1s").Should(Succeed())

	By(withTimestamp("killing the leader Kubernetes control plane node to trigger a fail-over scenario"))
	killLeader(leaderName, clusterName)

	By(withTimestamp("checking that the Kubernetes control plane nodes are still accessible via the assigned VIP with little downtime"))
	for _, cpVIP := range cpVIPs {
		By(withTimestamp(fmt.Sprintf("testing connection to VIP: %s", cpVIP)))
		// Allow at most 30 seconds of downtime when polling the control plane nodes
		assertControlPlaneIsRoutable(cpVIP, time.Duration(0), 20*time.Second)
	}
}

func testService(svcName, lbAddress, leaseName, leaseNamespace string, trafficPolicy corev1.ServiceExternalTrafficPolicy,
	client kubernetes.Interface, serviceElection bool, ipFamily []corev1.IPFamily, numberOfServices int) {
	lbAddresses := vip.Split(lbAddress)

	services := []string{}
	leases := []string{}
	for i := range numberOfServices {
		services = append(services, fmt.Sprintf("%s-%d", svcName, i))
		if serviceElection {
			leases = append(leases, fmt.Sprintf("%s-%d", leaseName, i))
		} else {
			leases = append(leases, leaseName)
		}

	}

	for _, svc := range services {
		createTestService(svc, dsNamespace, dsName, lbAddress,
			client, corev1.IPFamilyPolicyPreferDualStack, ipFamily, trafficPolicy)
	}

	for _, addr := range lbAddresses {
		Expect(checkIPAddressByLease(leases[0], leaseNamespace, addr, true, client)).To(BeTrue())
		assertConnection("http", addr, "80", "", 3*time.Second, 60*time.Second)
	}

	container := e2e.GetLeaseHolder(leases[0], leaseNamespace, client)

	for i := range numberOfServices {
		expected := i < numberOfServices-1

		err := client.CoreV1().Services(dsNamespace).Delete(context.TODO(), services[i], metav1.DeleteOptions{})
		Expect(err).ToNot(HaveOccurred())

		for _, addr := range lbAddresses {
			if serviceElection {
				Expect(checkIPAddress(addr, container, expected)).To(BeTrue())
			} else {
				Expect(checkIPAddressByLease(leases[i], leaseNamespace, addr, expected, client)).To(BeTrue())
			}

			if expected {
				assertConnection("http", addr, "80", "", 3*time.Second, 60*time.Second)
			} else {
				assertConnectionError("http", addr, "80", "", 3*time.Second)
			}
		}
	}
}
