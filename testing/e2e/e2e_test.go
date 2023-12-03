//go:build e2e
// +build e2e

package e2e_test

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"k8s.io/klog/v2"
	"sigs.k8s.io/kind/pkg/apis/config/v1alpha4"
	kindconfigv1alpha4 "sigs.k8s.io/kind/pkg/apis/config/v1alpha4"
	"sigs.k8s.io/kind/pkg/cluster"
	"sigs.k8s.io/kind/pkg/log"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/format"
	"github.com/onsi/gomega/gexec"

	"github.com/kube-vip/kube-vip/testing/e2e"
)

var _ = Describe("kube-vip broadcast neighbor", func() {
	var (
		logger                  log.Logger
		imagePath               string
		kubeVIPManifestTemplate *template.Template
		clusterName             string
		tempDirPath             string
	)

	BeforeEach(func() {
		klog.SetOutput(GinkgoWriter)
		logger = e2e.TestLogger{}

		imagePath = os.Getenv("E2E_IMAGE_PATH")

		curDir, err := os.Getwd()
		Expect(err).NotTo(HaveOccurred())
		templatePath := filepath.Join(curDir, "kube-vip.yaml.tmpl")

		kubeVIPManifestTemplate, err = template.New("kube-vip.yaml.tmpl").ParseFiles(templatePath)
		Expect(err).NotTo(HaveOccurred())

		tempDirPath, err = ioutil.TempDir("", "kube-vip-test")
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if os.Getenv("E2E_PRESERVE_CLUSTER") == "true" {
			return
		}

		provider := cluster.NewProvider(
			cluster.ProviderWithLogger(logger),
			cluster.ProviderWithDocker(),
		)

		Expect(provider.Delete(clusterName, "")).To(Succeed())

		Expect(os.RemoveAll(tempDirPath)).To(Succeed())
	})

	Describe("kube-vip IPv4 functionality", func() {
		var (
			clusterConfig kindconfigv1alpha4.Cluster
			ipv4VIP       string
		)

		BeforeEach(func() {
			clusterName = fmt.Sprintf("%s-ipv4", filepath.Base(tempDirPath))

			clusterConfig = kindconfigv1alpha4.Cluster{
				Networking: kindconfigv1alpha4.Networking{
					IPFamily: kindconfigv1alpha4.IPv4Family,
				},
				Nodes: []kindconfigv1alpha4.Node{},
			}

			manifestPath := filepath.Join(tempDirPath, "kube-vip-ipv4.yaml")

			for i := 0; i < 3; i++ {
				clusterConfig.Nodes = append(clusterConfig.Nodes, kindconfigv1alpha4.Node{
					Role: kindconfigv1alpha4.ControlPlaneRole,
					ExtraMounts: []kindconfigv1alpha4.Mount{
						{
							HostPath:      manifestPath,
							ContainerPath: "/etc/kubernetes/manifests/kube-vip.yaml",
						},
					},
				})
			}

			manifestFile, err := os.Create(manifestPath)
			Expect(err).NotTo(HaveOccurred())

			defer manifestFile.Close()

			ipv4VIP = e2e.GenerateIPv4VIP()

			Expect(kubeVIPManifestTemplate.Execute(manifestFile, e2e.KubevipManifestValues{
				ControlPlaneVIP: ipv4VIP,
				ImagePath:       imagePath,
			})).To(Succeed())
		})

		It("provides an IPv4 VIP address for the Kubernetes control plane nodes", func() {
			By(withTimestamp("creating a kind cluster with multiple control plane nodes"))
			createKindCluster(logger, &clusterConfig, clusterName)

			By(withTimestamp("loading local docker image to kind cluster"))
			e2e.LoadDockerImageToKind(logger, imagePath, clusterName)

			By(withTimestamp("checking that the Kubernetes control plane nodes are accessible via the assigned IPv4 VIP"))
			// Allow enough time for control plane nodes to load the docker image and
			// use the default timeout for establishing a connection to the VIP
			assertControlPlaneIsRoutable(ipv4VIP, time.Duration(0), 10*time.Second)

			By(withTimestamp("killing the leader Kubernetes control plane node to trigger a fail-over scenario"))
			killLeader(ipv4VIP, clusterName)

			By(withTimestamp("checking that the Kubernetes control plane nodes are still accessible via the assigned IPv4 VIP with little downtime"))
			// Allow at most 20 seconds of downtime when polling the control plane nodes
			assertControlPlaneIsRoutable(ipv4VIP, 1*time.Second, 20*time.Second)
		})
	})

	Describe("kube-vip IPv6 functionality", func() {
		var (
			clusterConfig kindconfigv1alpha4.Cluster
			ipv6VIP       string
		)

		BeforeEach(func() {
			clusterName = fmt.Sprintf("%s-ipv6", filepath.Base(tempDirPath))

			clusterConfig = kindconfigv1alpha4.Cluster{
				Networking: kindconfigv1alpha4.Networking{
					IPFamily: kindconfigv1alpha4.IPv6Family,
				},
				Nodes: []kindconfigv1alpha4.Node{},
			}

			manifestPath := filepath.Join(tempDirPath, "kube-vip-ipv6.yaml")

			for i := 0; i < 3; i++ {
				clusterConfig.Nodes = append(clusterConfig.Nodes, kindconfigv1alpha4.Node{
					ExtraMounts: []kindconfigv1alpha4.Mount{
						{
							HostPath:      manifestPath,
							ContainerPath: "/etc/kubernetes/manifests/kube-vip.yaml",
						},
					},
				})
			}

			ipv6VIP = e2e.GenerateIPv6VIP()

			manifestFile, err := os.Create(manifestPath)
			Expect(err).NotTo(HaveOccurred())

			defer manifestFile.Close()

			Expect(kubeVIPManifestTemplate.Execute(manifestFile, e2e.KubevipManifestValues{
				ControlPlaneVIP: ipv6VIP,
				ImagePath:       imagePath,
			})).To(Succeed())
		})

		It("provides an IPv6 VIP address for the Kubernetes control plane nodes", func() {
			By(withTimestamp("creating a kind cluster with multiple control plane nodes"))
			createKindCluster(logger, &clusterConfig, clusterName)

			By(withTimestamp("loading local docker image to kind cluster"))
			e2e.LoadDockerImageToKind(logger, imagePath, clusterName)

			By(withTimestamp("checking that the Kubernetes control plane nodes are accessible via the assigned IPv6 VIP"))
			// Allow enough time for control plane nodes to load the docker image and
			// use the default timeout for establishing a connection to the VIP
			assertControlPlaneIsRoutable(ipv6VIP, time.Duration(0), 10*time.Second)

			By(withTimestamp("killing the leader Kubernetes control plane node to trigger a fail-over scenario"))
			killLeader(ipv6VIP, clusterName)

			By(withTimestamp("checking that the Kubernetes control plane nodes are still accessible via the assigned IPv6 VIP with little downtime"))
			// Allow at most 20 seconds of downtime when polling the control plane nodes
			assertControlPlaneIsRoutable(ipv6VIP, 1*time.Second, 20*time.Second)
		})
	})
})

func createKindCluster(logger log.Logger, config *v1alpha4.Cluster, clusterName string) {
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
}

func assertControlPlaneIsRoutable(controlPlaneVIP string, transportTimeout, eventuallyTimeout time.Duration) {
	if strings.Contains(controlPlaneVIP, ":") {
		controlPlaneVIP = fmt.Sprintf("[%s]", controlPlaneVIP)
	}

	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // nolint
	}
	client := &http.Client{Transport: transport, Timeout: transportTimeout}
	Eventually(func() int {
		resp, _ := client.Get(fmt.Sprintf("https://%s:6443/livez", controlPlaneVIP))
		if resp == nil {
			return -1
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}, eventuallyTimeout).Should(Equal(http.StatusOK), "Failed to connect to VIP")
}

func killLeader(leaderIPAddr string, clusterName string) {
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

		Eventually(cmd.Run).Should(Succeed())

		if strings.Contains(cmdOut.String(), leaderIPAddr) {
			leaderName = name
			break
		}
	}
	Expect(leaderName).ToNot(BeEmpty())

	cmd := exec.Command(
		"docker", "kill", leaderName,
	)

	session, err := gexec.Start(cmd, GinkgoWriter, GinkgoWriter)
	Expect(err).NotTo(HaveOccurred())
	Eventually(session, "5s").Should(gexec.Exit(0))
}

func withTimestamp(text string) string {
	return fmt.Sprintf("%s: %s", time.Now(), text)
}
