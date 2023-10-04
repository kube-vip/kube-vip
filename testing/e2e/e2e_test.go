//go:build e2e
// +build e2e

package e2e_test

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"io/ioutil"
	"net"
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
	"sigs.k8s.io/kind/pkg/cmd"
	load "sigs.k8s.io/kind/pkg/cmd/kind/load/docker-image"
	"sigs.k8s.io/kind/pkg/log"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/format"
	"github.com/onsi/gomega/gexec"
)

type kubevipManifestValues struct {
	ControlPlaneVIP string
	ImagePath       string
}

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
		logger = TestLogger{}

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

			ipv4VIP = generateIPv4VIP()

			Expect(kubeVIPManifestTemplate.Execute(manifestFile, kubevipManifestValues{
				ControlPlaneVIP: ipv4VIP,
				ImagePath:       imagePath,
			})).To(Succeed())
		})

		It("provides an IPv4 VIP address for the Kubernetes control plane nodes", func() {
			By(withTimestamp("creating a kind cluster with multiple control plane nodes"))
			createKindCluster(logger, &clusterConfig, clusterName)

			By(withTimestamp("loading local docker image to kind cluster"))
			loadDockerImageToKind(logger, imagePath, clusterName)

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

			ipv6VIP = generateIPv6VIP()

			manifestFile, err := os.Create(manifestPath)
			Expect(err).NotTo(HaveOccurred())

			defer manifestFile.Close()

			Expect(kubeVIPManifestTemplate.Execute(manifestFile, kubevipManifestValues{
				ControlPlaneVIP: ipv6VIP,
				ImagePath:       imagePath,
			})).To(Succeed())
		})

		It("provides an IPv6 VIP address for the Kubernetes control plane nodes", func() {
			By(withTimestamp("creating a kind cluster with multiple control plane nodes"))
			createKindCluster(logger, &clusterConfig, clusterName)

			By(withTimestamp("loading local docker image to kind cluster"))
			loadDockerImageToKind(logger, imagePath, clusterName)

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

func loadDockerImageToKind(logger log.Logger, imagePath string, clusterName string) {
	loadImageCmd := load.NewCommand(logger, cmd.StandardIOStreams())
	loadImageCmd.SetArgs([]string{"--name", clusterName, imagePath})
	Expect(loadImageCmd.Execute()).To(Succeed())
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

func getKindNetworkSubnetCIDRs() []string {
	cmd := exec.Command(
		"docker", "inspect", "kind",
		"--format", `{{ range $i, $a := .IPAM.Config }}{{ println .Subnet }}{{ end }}`,
	)
	cmdOut := new(bytes.Buffer)
	cmd.Stdout = cmdOut
	Expect(cmd.Run()).To(Succeed(), "The Docker \"kind\" network was not found.")
	reader := bufio.NewReader(cmdOut)

	cidrs := []string{}
	for {
		line, readErr := reader.ReadString('\n')
		if readErr != nil && readErr != io.EOF {
			Expect(readErr).NotTo(HaveOccurred(), "Error finding subnet CIDRs in the Docker \"kind\" network")
		}

		cidrs = append(cidrs, strings.TrimSpace(line))
		if readErr == io.EOF {
			break
		}
	}

	return cidrs
}

func generateIPv4VIP() string {
	cidrs := getKindNetworkSubnetCIDRs()

	for _, cidr := range cidrs {
		ip, ipNet, parseErr := net.ParseCIDR(cidr)
		Expect(parseErr).NotTo(HaveOccurred())

		if ip.To4() != nil {
			mask := binary.BigEndian.Uint32(ipNet.Mask)
			start := binary.BigEndian.Uint32(ipNet.IP)
			end := (start & mask) | (^mask)

			chosenVIP := make([]byte, 4)
			binary.BigEndian.PutUint32(chosenVIP, end-5)
			return net.IP(chosenVIP).String()
		}
	}
	Fail("Could not find any IPv4 CIDRs in the Docker \"kind\" network")
	return ""
}

func generateIPv6VIP() string {
	cidrs := getKindNetworkSubnetCIDRs()

	for _, cidr := range cidrs {
		ip, ipNet, parseErr := net.ParseCIDR(cidr)
		Expect(parseErr).NotTo(HaveOccurred())

		if ip.To4() == nil {
			lowerMask := binary.BigEndian.Uint64(ipNet.Mask[8:])
			lowerStart := binary.BigEndian.Uint64(ipNet.IP[8:])
			lowerEnd := (lowerStart & lowerMask) | (^lowerMask)

			chosenVIP := make([]byte, 16)
			// Copy upper half into chosenVIP
			copy(chosenVIP, ipNet.IP[0:8])
			// Copy lower half into chosenVIP
			binary.BigEndian.PutUint64(chosenVIP[8:], lowerEnd-5)
			return net.IP(chosenVIP).String()
		}
	}
	Fail("Could not find any IPv6 CIDRs in the Docker \"kind\" network")
	return ""
}

type TestLogger struct{}

func (t TestLogger) Warnf(format string, args ...interface{}) {
	klog.Warningf(format, args...)
}

func (t TestLogger) Warn(message string) {
	klog.Warning(message)
}

func (t TestLogger) Error(message string) {
	klog.Error(message)
}

func (t TestLogger) Errorf(format string, args ...interface{}) {
	klog.Errorf(format, args...)
}

func (t TestLogger) V(level log.Level) log.InfoLogger {
	return TestInfoLogger{Verbose: klog.V(klog.Level(level))}
}

type TestInfoLogger struct {
	klog.Verbose
}

func (t TestInfoLogger) Info(message string) {
	t.Verbose.Info(message)
}
