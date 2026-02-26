//go:build e2e
// +build e2e

package e2e_test

import (
	"context"
	"os"

	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	kindconfigv1alpha4 "sigs.k8s.io/kind/pkg/apis/config/v1alpha4"
	"sigs.k8s.io/kind/pkg/log"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/kube-vip/kube-vip/pkg/utils"
	"github.com/kube-vip/kube-vip/testing/e2e"
)

var _ = Describe("kube-vip RT functionality when deployed as a regular pod", Ordered, func() {
	if Mode == ModeRT {
		var (
			logger          log.Logger
			imagePath       string
			k8sImagePath    string
			configPath      string
			tempDirPathRoot string
		)

		ctx, cancel := context.WithCancel(context.TODO())

		BeforeEach(func() {
			klog.SetOutput(GinkgoWriter)
			logger = e2e.TestLogger{}

			imagePath = os.Getenv("E2E_IMAGE_PATH")    // Path to kube-vip image
			configPath = os.Getenv("CONFIG_PATH")      // path to the api server config
			k8sImagePath = os.Getenv("K8S_IMAGE_PATH") // path to the kubernetes image (version for kind)
			if configPath == "" {
				configPath = "/etc/kubernetes/admin.conf"
			}
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
			cancel()
		})

		Describe("kube-vip RT functionality", Ordered, func() {
			var (
				client      kubernetes.Interface
				clusterName string
				tempDirPath string
			)

			BeforeAll(func() {
				networking := &kindconfigv1alpha4.Networking{
					IPFamily: kindconfigv1alpha4.IPv4Family,
				}

				var err error
				tempDirPath, err = os.MkdirTemp(tempDirPathRoot, "kube-vip-test")
				Expect(err).NotTo(HaveOccurred())

				clusterName, client, _ = prepareClusterForDS(ctx, tempDirPath, "rt-ds", imagePath, k8sImagePath,
					logger, networking, 1, nil, 1)
			})

			AfterAll(func() {
				Eventually(func() error {
					return e2e.GetLogs(ctx, client, tempDirPath)
				}, "60s", "5s").Should(Succeed())
				cleanupCluster(clusterName, ConfigMtx, logger)
			})

			It(clusterName+" exits gracefully on exit when only control plane is enabled", func() {
				manifestValues := &e2e.KubevipManifestValues{
					Mode:                 Mode,
					ControlPlaneEnable:   "true",
					VipElectionEnable:    "false",
					ImagePath:            imagePath,
					ConfigPath:           configPath,
					SvcEnable:            "false",
					SvcElectionEnable:    "false",
					EnableEndpointslices: "true",
					EnableNodeLabeling:   "false",
				}

				testDsArp(ctx, manifestValues, client, utils.IPv4Family, clusterName)
			})

			It(clusterName+" exits gracefully on exit when only services are enabled without leaderelection", func() {
				manifestValues := &e2e.KubevipManifestValues{
					Mode:                 Mode,
					ControlPlaneEnable:   "false",
					VipElectionEnable:    "false",
					ImagePath:            imagePath,
					ConfigPath:           configPath,
					SvcEnable:            "true",
					SvcElectionEnable:    "false",
					EnableEndpointslices: "true",
					EnableNodeLabeling:   "false",
				}

				testDsArp(ctx, manifestValues, client, utils.IPv4Family, clusterName)
			})

			It(clusterName+" exits gracefully on exit when only services are enabled with leaderelection", func() {
				manifestValues := &e2e.KubevipManifestValues{
					Mode:                 Mode,
					ControlPlaneEnable:   "false",
					VipElectionEnable:    "true",
					ImagePath:            imagePath,
					ConfigPath:           configPath,
					SvcEnable:            "true",
					SvcElectionEnable:    "false",
					EnableEndpointslices: "true",
					EnableNodeLabeling:   "false",
				}

				testDsArp(ctx, manifestValues, client, utils.IPv4Family, clusterName)
			})

			It(clusterName+" exits gracefully on exit when only services are enabled with leaderelection per service", func() {
				manifestValues := &e2e.KubevipManifestValues{
					Mode:                 Mode,
					ControlPlaneEnable:   "false",
					VipElectionEnable:    "true",
					ImagePath:            imagePath,
					ConfigPath:           configPath,
					SvcEnable:            "true",
					SvcElectionEnable:    "true",
					EnableEndpointslices: "true",
					EnableNodeLabeling:   "false",
				}

				testDsArp(ctx, manifestValues, client, utils.IPv4Family, clusterName)
			})

			It(clusterName+" exits gracefully on exit when control-plane and services are enabled without leaderelection", func() {
				manifestValues := &e2e.KubevipManifestValues{
					Mode:                 Mode,
					ControlPlaneEnable:   "true",
					VipElectionEnable:    "false",
					ImagePath:            imagePath,
					ConfigPath:           configPath,
					SvcEnable:            "true",
					SvcElectionEnable:    "false",
					EnableEndpointslices: "true",
					EnableNodeLabeling:   "false",
				}

				testDsArp(ctx, manifestValues, client, utils.IPv4Family, clusterName)
			})

			It(clusterName+" exits gracefully on exit when control-plane and services are enabled with leaderelection", func() {
				manifestValues := &e2e.KubevipManifestValues{
					Mode:                 Mode,
					ControlPlaneEnable:   "true",
					VipElectionEnable:    "true",
					ImagePath:            imagePath,
					ConfigPath:           configPath,
					SvcEnable:            "true",
					SvcElectionEnable:    "false",
					EnableEndpointslices: "true",
					EnableNodeLabeling:   "false",
				}

				testDsArp(ctx, manifestValues, client, utils.IPv4Family, clusterName)
			})

			It(clusterName+" exits gracefully when control-plane and services are enabled with leaderelection per service", func() {
				manifestValues := &e2e.KubevipManifestValues{
					Mode:                 Mode,
					ControlPlaneEnable:   "true",
					VipElectionEnable:    "true",
					ImagePath:            imagePath,
					ConfigPath:           configPath,
					SvcEnable:            "true",
					SvcElectionEnable:    "true",
					EnableEndpointslices: "true",
					EnableNodeLabeling:   "false",
				}

				testDsArp(ctx, manifestValues, client, utils.IPv4Family, clusterName)
			})

		})
	}
})
