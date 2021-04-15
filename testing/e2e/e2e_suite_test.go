package e2e_test

import (
	"fmt"
	"os/exec"
	"testing"
	"time"

	kindconfigv1alpha4 "sigs.k8s.io/kind/pkg/apis/config/v1alpha4"
	"sigs.k8s.io/kind/pkg/cluster"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"
)

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "E2E Suite")
}

var _ = SynchronizedBeforeSuite(func() []byte {
	ensureKindNetwork()
	return []byte{}
}, func(_ []byte) {})

func ensureKindNetwork() {
	By("checking if the Docker \"kind\" network exists")
	cmd := exec.Command("docker", "inspect", "kind")
	session, err := gexec.Start(cmd, GinkgoWriter, GinkgoWriter)
	Expect(err).NotTo(HaveOccurred())
	Eventually(session).Should(gexec.Exit())
	if session.ExitCode() == 0 {
		return
	}

	By("Docker \"kind\" network was not found. Creating dummy Kind cluster to ensure creation")
	clusterConfig := kindconfigv1alpha4.Cluster{
		Networking: kindconfigv1alpha4.Networking{
			IPFamily: kindconfigv1alpha4.IPv6Family,
		},
	}

	provider := cluster.NewProvider(
		cluster.ProviderWithDocker(),
	)
	dummyClusterName := fmt.Sprintf("dummy-cluster-%d", time.Now().Unix())
	Expect(provider.Create(
		dummyClusterName,
		cluster.CreateWithV1Alpha4Config(&clusterConfig),
	)).To(Succeed())

	By("deleting dummy Kind cluster")
	Expect(provider.Delete(dummyClusterName, ""))

	By("checking if the Docker \"kind\" network was successfully created")
	cmd = exec.Command("docker", "inspect", "kind")
	session, err = gexec.Start(cmd, GinkgoWriter, GinkgoWriter)
	Expect(err).NotTo(HaveOccurred())
	Eventually(session).Should(gexec.Exit(0))
}
